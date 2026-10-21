package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/qoder"
	"orchids-api/internal/store"
)

// runQoderProbe implements `orchids-server qoder-probe`: it sends one minimal
// prompt per client identity through a single Qoder account and prints which
// queue the gateway admitted each identity to. See qoder.ProbeIdentities.
func runQoderProbe(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("qoder-probe", flag.ContinueOnError)
	configPath := flags.String("config", "", "Path to config.json/config.yaml")
	accountID := flags.Int64("account", 0, "Qoder account id (default: the enabled account with the most credits left)")
	model := flags.String("model", "qwen3.8-flash", "model to probe, as a client would name it")
	rounds := flags.Int("rounds", 0, "send the selected variants this many times, alternating their order (0: one pass)")
	interval := flags.Duration("interval", 35*time.Second, "pause before every request in a multi-round comparison")
	variants := flags.String("variants", "", "comma-separated variant letters to send, e.g. A,E (default: all)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	var letters []string
	for _, letter := range strings.Split(*variants, ",") {
		if letter = strings.TrimSpace(letter); letter != "" {
			letters = append(letters, letter)
		}
	}

	cfg, resolvedConfigPath, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	credentialKey, _, err := config.LoadOrCreateCredentialEncryptionKey(resolvedConfigPath, cfg)
	if err != nil {
		return fmt.Errorf("load credential encryption key: %w", err)
	}
	s, err := store.New(store.Options{
		RedisAddr:               cfg.RedisAddr,
		RedisPassword:           cfg.RedisPassword,
		RedisDB:                 cfg.RedisDB,
		RedisPrefix:             cfg.RedisPrefix,
		CredentialEncryptionKey: credentialKey,
	})
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer s.Close()
	budget := 10 * time.Minute
	if *rounds > 1 {
		perRound := max(len(letters), 7)
		budget += time.Duration(*rounds*perRound) * (*interval + 90*time.Second)
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	// The running server's settings decide endpoints and proxy, as they do for
	// real traffic.
	if saved, err := s.GetSetting(ctx, "config"); err == nil && saved != "" {
		savedCfg := cfg.Clone()
		if json.Unmarshal([]byte(saved), savedCfg) == nil {
			*cfg = *savedCfg
			config.ApplyDefaults(cfg)
		}
	}

	acc, err := pickProbeAccount(ctx, s, *accountID)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "account %d (%s, %s), model %s\n\n", acc.ID, acc.QoderQuota.UserType, acc.QoderQuota.PlanTier, *model)

	client := qoder.NewFromAccount(acc, cfg)
	client.SetAccountStore(s) // a token rotated during the probe is persisted
	if *rounds > 1 {
		return runProbeRounds(ctx, client, out, *model, letters, *rounds, *interval)
	}
	results, err := client.ProbeRounds(ctx, *model, qoder.ProbeRoundOptions{Rounds: 1, Variants: letters})
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "VARIANT\tHOST\tOUTCOME\tHTTP\tQUEUE\tAVAILABLE\tSUBCODE\tRETRY\tTIME")
	for _, r := range results {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n", r.Variant, r.Host, r.Outcome, r.Status,
			dash(r.QueueType), dash(r.Available), dash(r.SubCode), dash(r.RetryWait), r.Elapsed.Round(time.Millisecond))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(out)
	for _, r := range results {
		if r.Outcome == "error" && r.Detail != "" {
			fmt.Fprintf(out, "%s: %s\n", r.Variant, r.Detail)
		}
	}
	return nil
}

func dash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

// pickProbeAccount returns the requested Qoder account, or the enabled one with
// the most credits left so the probe is not refused for a spent allowance.
func pickProbeAccount(ctx context.Context, s *store.Store, id int64) (*store.Account, error) {
	if id != 0 {
		acc, err := s.GetAccount(ctx, id)
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(acc.AccountType, "qoder") {
			return nil, fmt.Errorf("account %d is not a Qoder account", id)
		}
		return acc, nil
	}
	accounts, err := s.GetEnabledAccounts(ctx)
	if err != nil {
		return nil, err
	}
	var best *store.Account
	for _, acc := range accounts {
		if !strings.EqualFold(acc.AccountType, "qoder") || acc.QoderQuota.Exhausted || !store.AccountAuthActive(acc) {
			continue
		}
		if best == nil || acc.QoderQuota.Remaining > best.QoderQuota.Remaining {
			best = acc
		}
	}
	if best == nil {
		return nil, errors.New("no enabled Qoder account with credits left; pass --account")
	}
	return best, nil
}

// qoderProbeCommand reports whether args name the probe subcommand.
func qoderProbeCommand(args []string) bool {
	return len(args) > 0 && args[0] == "qoder-probe"
}

// runProbeRounds prints each attempt as it lands and then how often each
// identity was served.
func runProbeRounds(ctx context.Context, client *qoder.Client, out io.Writer, model string, letters []string, rounds int, interval time.Duration) error {
	fmt.Fprintf(out, "%d rounds, %s between requests, order alternating each round\n\n", rounds, interval)
	results, err := client.ProbeRounds(ctx, model, qoder.ProbeRoundOptions{
		Variants: letters, Rounds: rounds, Interval: interval,
		Progress: func(round int, r qoder.ProbeResult) {
			line := fmt.Sprintf("%s round %2d  %-32s %-7s queue=%s available=%s %s", time.Now().Format("15:04:05"), round, r.Variant, r.Outcome,
				dash(r.QueueType), dash(r.Available), r.Elapsed.Round(time.Millisecond))
			// An error is not a queue verdict; without its reason the run only
			// says that something else went wrong (a spent daily count did).
			if r.Outcome == "error" && r.Detail != "" {
				line += "  " + probeErrorReason(r.Detail)
			}
			fmt.Fprintln(out, line)
		},
	})
	type tally struct {
		sent, served int
		queues       map[string]int
	}
	tallies := map[string]*tally{}
	var order []string
	for _, r := range results {
		t := tallies[r.Variant]
		if t == nil {
			t = &tally{queues: map[string]int{}}
			tallies[r.Variant] = t
			order = append(order, r.Variant)
		}
		t.sent++
		switch {
		case r.Outcome == "served":
			t.served++
		case r.Outcome == "error":
			t.queues["error: "+probeErrorReason(r.Detail)]++
		default:
			t.queues[dash(r.QueueType)]++
		}
	}
	fmt.Fprintln(out)
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "VARIANT\tSERVED\tSENT\tREFUSED BY")
	for _, name := range order {
		t := tallies[name]
		var queues []string
		for queue, count := range t.queues {
			queues = append(queues, fmt.Sprintf("%s=%d", queue, count))
		}
		slices.Sort(queues)
		fmt.Fprintf(w, "%s\t%d\t%d\t%s\n", name, t.served, t.sent, dash(strings.Join(queues, " ")))
	}
	if flushErr := w.Flush(); flushErr != nil {
		return flushErr
	}
	return err
}

// probeErrorReason shortens an error detail to its reason, so identical
// errors group in the summary.
func probeErrorReason(detail string) string {
	detail = strings.TrimSpace(detail)
	if index := strings.LastIndex(detail, ": "); index >= 0 && index+2 < len(detail) {
		detail = detail[index+2:]
	}
	if len(detail) > 80 {
		detail = detail[:80] + "..."
	}
	return detail
}

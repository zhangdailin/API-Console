package main

import (
	"context"
	"crypto/rand"
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
// prompt per inference node through a single Qoder account and prints whether
// each was served, queued (and in which queue) or refused. See
// qoder.ProbeRounds.
//
// -model accepts a comma-separated list and -identity selects the client
// identity to present. Operator probes recorded the identity deciding the
// admission queue (QoderWork p4, IDE/CLI p3) while the request body did not, so
// -identity both is the measurement that settles which queue this channel is
// actually using: it sends the same models twice, once per identity, on one
// account.
func runQoderProbe(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("qoder-probe", flag.ContinueOnError)
	configPath := flags.String("config", "", "Path to config.json/config.yaml")
	accountID := flags.Int64("account", 0, "Qoder account id (default: the enabled account with the most credits left)")
	modelsFlag := flags.String("model", "qwen3.8-flash", "comma-separated model keys to probe, as a client would name them")
	identityFlag := flags.String("identity", "default", "client identity to present: default (qoderwork), ide, or both")
	rounds := flags.Int("rounds", 0, "send the selected variants this many times, alternating their order (0: one pass)")
	interval := flags.Duration("interval", 35*time.Second, "pause before every request in a multi-round comparison")
	variants := flags.String("variants", "", "comma-separated variant letters to send, e.g. A,F (default: all)")
	alignWindow := flags.Bool("align-window", false, "send each request at second :09 of a minute, one per minute, instead of every --interval")
	sweep := flags.Int("sweep", 0, "replay the queue-window retry this many times in one process and report how many were admitted")
	queueModel := flags.String("queue", "", "read the admission queue for this model key instead of sending a chat")
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
	models := splitModelKeys(*modelsFlag)
	if len(models) == 0 {
		return errors.New("--model needs at least one key")
	}
	identities, err := selectIdentities(*identityFlag)
	if err != nil {
		return err
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
	if *rounds > 1 || *alignWindow {
		perRound := max(len(letters), 7)
		step := *interval
		if *alignWindow {
			step = time.Minute
		}
		budget += time.Duration(max(*rounds, 1)*perRound*len(models)*len(identities)) * (step + 90*time.Second)
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
	fmt.Fprintf(out, "account %d (%s, %s)\n\n", acc.ID, acc.QoderQuota.UserType, acc.QoderQuota.PlanTier)

	base := qoder.NewFromAccount(acc, cfg)
	base.SetAccountStore(s) // a token rotated during the probe is persisted

	if strings.TrimSpace(*queueModel) != "" {
		requestSetID, idErr := newProbeRequestID()
		if idErr != nil {
			return idErr
		}
		status, statusErr := base.QueueStatus(ctx, requestSetID, "", *queueModel, "")
		if statusErr != nil {
			return statusErr
		}
		fmt.Fprintf(out, "queue model=%s observed=%v queued=%v available=%v type=%s count=%d retry_after=%s wait=%s\n",
			*queueModel, status.Observed, status.IsQueued, status.ServiceAvailable,
			dash(status.QueueType), status.QueueCount, status.RetryAfter, status.WaitTime)
		return nil
	}

	if *sweep > 0 {
		return runProbeSweep(ctx, base, out, models[0], letters, *sweep)
	}

	summaries := make([]probeSummary, 0, len(models)*len(identities))
	for _, identity := range identities {
		// WithIdentity mutates the probe's own client, so it is set again for
		// every profile instead of only for the ones that differ from default.
		client := base.WithIdentity(identity)
		for _, model := range models {
			verbose := len(models)*len(identities) > 1
			if verbose {
				fmt.Fprintf(out, "identity %s  model %s\n", identity.Label, model)
			} else {
				fmt.Fprintf(out, "model %s\n\n", model)
			}
			results, probeErr := probeOneModel(ctx, client, out, model, letters, *rounds, *interval, *alignWindow)
			summary := summarizeProbe(model, results)
			summary.Identity = identity.Label
			summaries = append(summaries, summary)
			if probeErr != nil {
				return probeErr
			}
		}
	}

	if len(summaries) > 1 {
		printProbeSummary(out, summaries)
	}
	return nil
}

// splitModelKeys keeps the order the operator wrote and drops empty entries.
func splitModelKeys(raw string) []string {
	keys := make([]string, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		if key := strings.TrimSpace(part); key != "" {
			keys = append(keys, key)
		}
	}
	return keys
}

// selectIdentities maps -identity onto the profiles the package exposes. An
// unknown name is refused rather than silently treated as the default, because
// a typo would otherwise produce a run that looks like it compared identities.
func selectIdentities(raw string) ([]qoder.Identity, error) {
	defaultIdentity := qoder.DefaultIdentity()
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "default", "qoderwork", "qoder_work":
		return []qoder.Identity{defaultIdentity}, nil
	case "ide":
		return []qoder.Identity{qoder.IDEIdentity()}, nil
	case "both":
		return []qoder.Identity{defaultIdentity, qoder.IDEIdentity()}, nil
	default:
		return nil, fmt.Errorf("unknown --identity %q (want default, ide or both)", raw)
	}
}

// newProbeRequestID makes the identifier the queue status read asks about.
func newProbeRequestID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", raw), nil
}

// runProbeSweep replays the deployed queue-window retry in one process: wait
// for the window, send one request a second through it, and fall back to the
// next window when a window is spent. One process per attempt is not a fair
// simulation — the CLI spends seconds loading the account and deriving runtime
// fields between attempts, which pushes every attempt out of the window and
// measures the CLI instead of the policy.
func runProbeSweep(ctx context.Context, client *qoder.Client, out io.Writer, model string, letters []string, trials int) error {
	if len(letters) == 0 {
		letters = []string{"A"}
	}
	maxWait, maxRetries := qoder.QueueWindowLimits()
	fmt.Fprintf(out, "model %s, %d trials, at most %d attempts each, %s wait bound\n\n",
		model, trials, maxRetries, maxWait)
	admitted := 0
	for trial := 1; trial <= trials; trial++ {
		if ctx.Err() != nil {
			break
		}
		started := time.Now()
		attempts := 0
		served := false
		queue := ""
		for attempts < maxRetries {
			delay := qoder.NextQueueWindowDelay(time.Now())
			if time.Since(started)+delay > maxWait {
				break
			}
			if delay > 0 {
				select {
				case <-ctx.Done():
				case <-time.After(delay):
				}
			}
			attempts++
			results, err := client.ProbeRounds(ctx, model, qoder.ProbeRoundOptions{Rounds: 1, Variants: letters})
			if err != nil {
				return err
			}
			for _, r := range results {
				if r.Outcome == "served" {
					served = true
					break
				}
				queue = r.QueueType
			}
			if served {
				break
			}
		}
		if served {
			admitted++
		}
		fmt.Fprintf(out, "sweep %2d: served=%-5v attempts=%2d elapsed=%-9s queue=%s done_at=%s\n",
			trial, served, attempts, time.Since(started).Round(100*time.Millisecond),
			dash(queue), time.Now().Format("15:04:05"))
	}
	fmt.Fprintf(out, "\nsummary: %d/%d admitted\n", admitted, trials)
	return nil
}

// probeOneModel runs one model and prints the per-node table the command has
// always printed.
func probeOneModel(ctx context.Context, client *qoder.Client, out io.Writer, model string, letters []string, rounds int, interval time.Duration, alignWindow bool) ([]qoder.ProbeResult, error) {
	if rounds > 1 || alignWindow {
		return runProbeRounds(ctx, client, out, model, letters, max(rounds, 1), interval, alignWindow)
	}
	results, err := client.ProbeRounds(ctx, model, qoder.ProbeRoundOptions{Rounds: 1, Variants: letters})
	if err != nil {
		return nil, err
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "VARIANT\tHOST\tOUTCOME\tHTTP\tQUEUE\tAVAILABLE\tSUBCODE\tRETRY\tTIME")
	for _, r := range results {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n", r.Variant, r.Host, r.Outcome, r.Status,
			dash(r.QueueType), dash(r.Available), dash(r.SubCode), dash(r.RetryWait), r.Elapsed.Round(time.Millisecond))
	}
	if err := w.Flush(); err != nil {
		return nil, err
	}
	fmt.Fprintln(out)
	for _, r := range results {
		if r.Outcome == "error" && r.Detail != "" {
			fmt.Fprintf(out, "%s: %s\n", r.Variant, r.Detail)
		}
	}
	return results, nil
}

// probeSummary is one model's queue verdict across every node it was sent to.
type probeSummary struct {
	Identity string
	Model    string
	Served   int
	Queued   int
	Errors   int
	Queue    string
	Verdict  string
}

// summarizeProbe reduces a model's results to the queue class it lands in. A
// model that answers p4 on every node can only be reached by switching tier or
// waiting for the per-minute window; one that answers p3 is admitted on the
// identity the gateway presents.
func summarizeProbe(model string, results []qoder.ProbeResult) probeSummary {
	summary := probeSummary{Model: model}
	queueCounts := map[string]int{}
	for _, r := range results {
		switch r.Outcome {
		case "served":
			summary.Served++
		case "queued":
			summary.Queued++
			if q := strings.TrimSpace(r.QueueType); q != "" {
				queueCounts[q]++
			}
		default:
			summary.Errors++
		}
	}
	for queue, count := range queueCounts {
		if summary.Queue == "" || count > queueCounts[summary.Queue] {
			summary.Queue = queue
		}
	}
	switch {
	case summary.Served > 0 && summary.Queued == 0:
		summary.Verdict = "admitted"
	case summary.Served > 0 && summary.Queued > 0:
		summary.Verdict = "partly admitted"
	case summary.Queued > 0:
		summary.Verdict = "queued " + dash(summary.Queue)
	case summary.Errors > 0:
		summary.Verdict = "refused"
	default:
		summary.Verdict = "no result"
	}
	return summary
}

func printProbeSummary(out io.Writer, summaries []probeSummary) {
	fmt.Fprintln(out)
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "IDENTITY\tMODEL\tSERVED\tQUEUED\tERRORS\tQUEUE\tVERDICT")
	for _, s := range summaries {
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%s\t%s\n", dash(s.Identity), s.Model, s.Served, s.Queued, s.Errors, dash(s.Queue), s.Verdict)
	}
	if err := w.Flush(); err != nil {
		return
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "The client identity picks the queue class (QoderWork p4, IDE/CLI p3); the model key only")
	fmt.Fprintln(out, "decides whether a key is served at all. A key that answers p4 needs a tier switch or a")
	fmt.Fprintln(out, "window-aligned retry, not a longer timeout.")
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
func runProbeRounds(ctx context.Context, client *qoder.Client, out io.Writer, model string, letters []string, rounds int, interval time.Duration, alignWindow bool) ([]qoder.ProbeResult, error) {
	if alignWindow {
		fmt.Fprintf(out, "%d rounds, one request per minute at second :09, order alternating each round\n\n", rounds)
	} else {
		fmt.Fprintf(out, "%d rounds, %s between requests, order alternating each round\n\n", rounds, interval)
	}
	results, err := client.ProbeRounds(ctx, model, qoder.ProbeRoundOptions{
		Variants: letters, Rounds: rounds, Interval: interval, AlignWindow: alignWindow,
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
		return results, flushErr
	}
	return results, err
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

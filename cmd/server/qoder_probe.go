package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
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
	if err := flags.Parse(args); err != nil {
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
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
	results, err := client.ProbeIdentities(ctx, *model)
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

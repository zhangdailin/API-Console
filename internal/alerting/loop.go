package alerting

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"orchids-api/internal/audit"
	"orchids-api/internal/opsagg"
	"orchids-api/internal/poolstate"
	"orchids-api/internal/store"
)

// alertWindowMinutes is the evidence window one evaluation pass looks at. It is
// long enough for a success-rate rule to have samples and short enough to notice
// a regression within a few minutes.
const alertWindowMinutes = 30

// alertEvery is how often rules are evaluated.
const alertEvery = 60 * time.Second

// startAlertLoop evaluates the alert rules and journals every firing and
// recovery, so a failure has a start, an owner and an end in one place.
// StartLoop evaluates the alert rules and journals every firing and recovery.
// It returns immediately: evaluation runs on its own goroutine until ctx is
// cancelled.
func StartLoop(ctx context.Context, agg *opsagg.Aggregator, s *store.Store, engine *Engine, logger audit.Logger) {
	if agg == nil || !agg.Enabled() || engine == nil {
		slog.Debug("Alert evaluation disabled (no metric aggregation)")
		return
	}
	evaluate := func() {
		evaluationCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		snapshot, err := BuildSnapshot(evaluationCtx, agg, s)
		if err != nil {
			slog.Warn("Alert evaluation failed", "error", err)
			return
		}
		transition := engine.Evaluate(snapshot)
		for _, alert := range transition.Firing {
			slog.Warn("Alert firing", "key", alert.Key, "severity", string(alert.Severity), "channel", alert.Channel, "title", alert.Title, "detail", alert.Detail)
		}
		for _, alert := range transition.Recovered {
			slog.Info("Alert recovered", "key", alert.Key, "channel", alert.Channel, "title", alert.Title)
		}
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("Panic in alert loop", "error", r)
			}
		}()
		evaluate()
		ticker := time.NewTicker(alertEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				evaluate()
			}
		}
	}()
}

// buildAlertSnapshot assembles the evidence one evaluation pass needs: per
// channel request outcomes, pool availability and credential state.
func BuildSnapshot(ctx context.Context, agg *opsagg.Aggregator, s *store.Store) (Snapshot, error) {
	now := time.Now()
	since := now.Add(-time.Duration(alertWindowMinutes) * time.Minute)
	snapshot := Snapshot{At: now}

	channels, err := agg.Channels(ctx, since, now)
	if err != nil {
		return snapshot, err
	}
	accounts, accountErr := s.ListAccounts(ctx)
	if accountErr != nil {
		return snapshot, accountErr
	}

	seen := map[string]bool{}
	add := func(channel string) (ChannelSnapshot, error) {
		buckets, rangeErr := agg.Range(ctx, channel, since, now)
		entry := ChannelSnapshot{Channel: channel}
		if rangeErr != nil {
			return entry, rangeErr
		}
		durations, ttfts, err := agg.SamplesForChecked(ctx, channel, buckets)
		if err != nil {
			return entry, err
		}
		summary := agg.SummarizeWith(ctx, opsagg.SummaryInput{Channel: channel, Buckets: buckets, Durations: durations, FirstTokenMS: ttfts, SamplesProvided: true, WindowMinutes: alertWindowMinutes})
		entry.Requests = summary.Requests
		entry.Success = summary.Success
		entry.Failed = summary.Failed
		entry.Samples = summary.Samples
		entry.SuccessRate = summary.SuccessRate
		pool := poolstate.Counts(accounts, channel, now, poolstate.NeedingLoginReverify)
		entry.AccountsEnabled = pool.Enabled
		entry.AccountsAvailable = pool.Available
		entry.AccountsNeedingLogin = pool.NeedingLogin
		entry.ModelCooldowns = pool.ModelCooldowns
		return entry, ctx.Err()
	}

	for _, channel := range channels {
		seen[channel] = true
		entry, err := add(channel)
		if err != nil {
			return snapshot, err
		}
		snapshot.Channels = append(snapshot.Channels, entry)
	}
	// A channel with accounts but no traffic still matters: an exhausted pool must
	// alert even when no request has been served yet.
	for _, channel := range ChannelsForAccounts(accounts) {
		if seen[channel] {
			continue
		}
		entry, err := add(channel)
		if err != nil {
			return snapshot, err
		}
		snapshot.Channels = append(snapshot.Channels, entry)
	}
	return snapshot, nil
}

func ChannelsForAccounts(accounts []*store.Account) []string {
	seen := map[string]bool{}
	out := make([]string, 0, 8)
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(acc.AccountType))
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

func AuditRecorder(logger audit.Logger) func(Alert, bool) {
	return func(alert Alert, firing bool) {
		if logger == nil {
			return
		}
		status := "firing"
		action := "alert_fired"
		errorText := alert.Detail
		if !firing {
			status = "recovered"
			action = "alert_recovered"
			errorText = ""
		}
		logger.Log(context.Background(), audit.Event{
			Kind:    audit.KindSystem,
			Action:  action,
			Channel: alert.Channel,
			Model:   alert.Key,
			Status:  status,
			Error:   errorText,
			Details: alert.Title,
			Metadata: map[string]any{
				"severity": string(alert.Severity),
				"key":      alert.Key,
			},
		})
	}
}

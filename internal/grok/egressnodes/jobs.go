package egressnodes

import (
	"context"
	"log/slog"
	"time"
)

// runJobs drives the automation: periodic probes, account assignment and
// subscription refresh. Each job claims a Redis lease for its interval, so a
// multi-instance deployment runs it once.
func (r *Registry) runJobs(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.runDueJobs(ctx)
		}
	}
}

func (r *Registry) claim(ctx context.Context, job string, interval time.Duration) bool {
	ok, err := r.store.ClaimGrokEgressJob(ctx, job, interval)
	return err == nil && ok
}

func (r *Registry) runDueJobs(ctx context.Context) {
	ops, err := r.store.GetGrokEgressOperations(ctx)
	if err != nil {
		return
	}
	if len(Current().Nodes) > 0 && r.claim(ctx, "probe", time.Duration(ops.ProbeIntervalSeconds)*time.Second) {
		if _, err := r.ProbeAll(ctx, nil); err != nil {
			slog.Warn("egress: scheduled probe failed", "error", err)
		}
	}
	if (ops.AutoAssignEnabled || ops.AutoBalanceEnabled) && r.claim(ctx, "assign", time.Duration(ops.AssignmentIntervalSeconds)*time.Second) {
		if _, err := r.Rebalance(ctx, ops.AutoBalanceEnabled); err != nil {
			slog.Warn("egress: scheduled assignment failed", "error", err)
		}
	}
	sources, err := r.store.ListGrokEgressSources(ctx)
	if err != nil {
		return
	}
	for _, source := range sources {
		interval := time.Duration(source.RefreshIntervalSeconds) * time.Second
		if !source.Enabled || time.Since(source.LastSyncAt) < interval || !r.claim(ctx, "source:"+source.ID, interval) {
			continue
		}
		if _, err := r.SyncSource(ctx, source.ID); err != nil {
			slog.Warn("egress: subscription sync failed", "source", source.Name, "error", err)
		}
	}
}

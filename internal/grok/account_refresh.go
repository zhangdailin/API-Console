package grok

import (
	"context"
	"time"

	"orchids-api/internal/store"
)

// BuildRefreshOptions selects the optional control-plane observations around a
// Build OAuth verification. Zero timeout means inherit the caller context.
type BuildRefreshOptions struct {
	Verify         bool
	VerifyTimeout  time.Duration
	Billing        bool
	BillingTimeout time.Duration
	Models         bool
	ModelsTimeout  time.Duration
	Now            time.Time
}

// BuildRefreshResult reports optional observation failures independently. Only
// verification failure is authentication-fatal to the orchestration.
type BuildRefreshResult struct {
	VerifyStatus string
	VerifyErr    error
	BillingErr   error
	ModelsErr    error
}

// RefreshBuildAccount runs the shared verify/billing/models sequence without
// persisting. Successful billing and catalog reads are applied in memory;
// failed/empty model reads leave the last-known-good catalog untouched.
func RefreshBuildAccount(ctx context.Context, client *CLIClient, acc *store.Account, opts BuildRefreshOptions) BuildRefreshResult {
	var result BuildRefreshResult
	if opts.Verify {
		verifyCtx, cancel := refreshStepContext(ctx, opts.VerifyTimeout)
		result.VerifyStatus, result.VerifyErr = client.VerifyAccount(verifyCtx, acc)
		cancel()
		if result.VerifyErr != nil {
			return result
		}
	}
	if opts.Billing {
		billingCtx, cancel := refreshStepContext(ctx, opts.BillingTimeout)
		billing, err := client.FetchBilling(billingCtx, acc)
		cancel()
		result.BillingErr = err
		if billing != nil {
			ApplyCLIBillingInfo(acc, billing)
		}
	}
	if opts.Models {
		modelsCtx, cancel := refreshStepContext(ctx, opts.ModelsTimeout)
		catalog, err := client.FetchModelCatalog(modelsCtx, acc)
		cancel()
		result.ModelsErr = err
		if err == nil {
			now := opts.Now
			if now.IsZero() {
				now = time.Now()
			}
			ApplyCLIModelCatalog(acc, catalog, now)
		}
	}
	return result
}

func refreshStepContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

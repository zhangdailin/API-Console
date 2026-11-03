package web

import (
	"context"

	"orchids-api/internal/audit"
	"orchids-api/internal/middleware"
	"orchids-api/internal/pricing"
)

func (h *Handler) settleMediaBilling(ctx context.Context, requestModel string, result pricing.Result, metadata map[string]interface{}) {
	if h == nil || result.CostInUSDTicks <= 0 {
		return
	}
	booked := middleware.SettleAPIKeyBillingResult(ctx, nil, result)
	logger := h.auditLoggerSnapshot()
	if logger == nil {
		return
	}
	status := "success"
	if !booked {
		status = "unpriced"
	}
	logger.Log(ctx, audit.Event{
		Kind: audit.KindRequest, RequestID: middleware.GetRequestID(ctx), Action: "grok_media_request",
		APIKeyID: middleware.APIKeyID(ctx), Model: requestModel, Channel: "grok",
		Status: status, UsageSource: audit.UsageSourceUpstream, Metadata: metadata,
		CostInUSDTicks: result.CostInUSDTicks, PricingModel: result.Model, PricingVersion: pricing.Version,
	})
}

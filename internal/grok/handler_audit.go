package grok

import (
	"context"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
	"time"
)

func (h *Handler) auditAttempt(ctx context.Context, acc *store.Account, provider string, attempt int, started time.Time, err error, stages ...string) {
	stage := "request"
	if len(stages) > 0 {
		stage = stages[0]
	}
	h.auditAttemptDiagnostic(ctx, grokbase.Attempt{Account: acc, Provider: provider, Number: attempt, Started: started, Err: err, Stage: stage})
}

func (h *Handler) auditAttemptDiagnostic(ctx context.Context, attempt grokbase.Attempt) {
	grokbase.LogAttempt(ctx, h.auditLoggerSnapshot(), attempt, ClassifyUpstreamError)
}

func (h *Handler) auditChatOutcome(ctx context.Context, acc *store.Account, req *chatwire.Request, result chatOutcome) {
	upstream.NoteServedModelWithSource(ctx, result.ServedModel, "responses.model")
	provider := ""
	if acc != nil {
		provider = ProviderBuild
	}
	grokbase.LogChatOutcome(ctx, h.auditLoggerSnapshot(), h.affinityStore(), acc, req, grokbase.ChatAudit{
		Provider: provider, Finish: result.Finish, Err: result.Err, FirstToken: result.FirstToken,
		Usage: result.Usage, UsageSource: result.UsageSource,
		VisibleOutput: &result.SawText,
	})
}

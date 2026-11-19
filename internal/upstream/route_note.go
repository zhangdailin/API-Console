package upstream

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Route facts an upstream client reports about the call it actually made. The
// request journal shows them next to what the client asked for, so a row can
// answer "which upstream endpoint served this, and which model did the
// upstream say it ran" without opening a diagnostic capture.
const (
	// RouteEndpoint is the upstream URL path (no query, which may carry state).
	RouteEndpoint = "upstream_endpoint"
	// RouteHost is the upstream host the endpoint was called on.
	RouteHost = "upstream_host"
	// RouteSentModel is the model identifier sent in the upstream request,
	// after the gateway's own mapping.
	RouteSentModel = "upstream_model_sent"
	// RouteServedModel is the model identifier the upstream reported in its
	// response. It is absent when the upstream does not report one.
	RouteServedModel   = "upstream_model"
	RouteModelSource   = "upstream_model_source"
	RouteModelStatus   = "upstream_model_status"
	RouteModelFallback = "upstream_model_fallback"
	// RouteProvider is the provider an upstream gateway reports it finally
	// routed the request to (Cline: provider_metadata.gateway.routing).
	RouteProvider = "upstream_provider"
	// RouteProviderAttempts is how many provider attempts the gateway reports
	// for that request; "0" means it did not say.
	RouteProviderAttempts = "upstream_provider_attempts"
)

type routeObserverKey struct{}

// WithRouteObserver installs the callback that records route facts for the
// request carried by ctx. Empty values remove a fact from a previous attempt.
func WithRouteObserver(ctx context.Context, observer func(field, value string)) context.Context {
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, routeObserverKey{}, observer)
}

func noteRoute(ctx context.Context, field, value string) {
	value = strings.TrimSpace(value)
	if ctx == nil || value == "" {
		return
	}
	if observer, ok := ctx.Value(routeObserverKey{}).(func(field, value string)); ok {
		observer(field, value)
	}
}

// NoteUpstreamCall records the endpoint an inference request is about to be
// sent to and the model identifier it carries. Only the host and path of
// rawURL are kept; the query string is dropped.
func NoteUpstreamCall(ctx context.Context, rawURL, sentModel string) {
	// A previous attempt's response must not identify an unreported new one.
	if ctx != nil {
		if observer, ok := ctx.Value(routeObserverKey{}).(func(string, string)); ok {
			for _, field := range []string{RouteHost, RouteEndpoint, RouteSentModel, RouteServedModel, RouteModelSource, RouteModelFallback, RouteProvider, RouteProviderAttempts} {
				observer(field, "")
			}
		}
	}
	noteRoute(ctx, RouteModelStatus, "unreported")
	if parsed, err := url.Parse(strings.TrimSpace(rawURL)); err == nil && parsed.Path != "" {
		noteRoute(ctx, RouteHost, parsed.Host)
		noteRoute(ctx, RouteEndpoint, parsed.Path)
	}
	noteRoute(ctx, RouteSentModel, sentModel)
}

// NoteServedProvider records the provider a gateway says served the request
// and how many provider attempts it took. The two are recorded together so a
// retry never pairs one attempt's provider with another attempt's count.
func NoteServedProvider(ctx context.Context, provider string, attempts int) {
	if strings.TrimSpace(provider) == "" {
		return
	}
	noteRoute(ctx, RouteProvider, provider)
	noteRoute(ctx, RouteProviderAttempts, strconv.Itoa(max(attempts, 0)))
}

// NoteServedModel records the model identifier the upstream reported for the
// response. Callers may report it on every chunk; only a non-empty value is kept.
func NoteServedModel(ctx context.Context, model string) {
	NoteServedModelWithSource(ctx, model, "upstream_response")
}

// NoteServedModelWithSource records response metadata, never a requested alias.
// Sources are fixed identifiers owned by this gateway, not response text.
func NoteServedModelWithSource(ctx context.Context, model, source string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	if len(model) > 256 || !utf8.ValidString(model) || strings.IndexFunc(model, unicode.IsControl) >= 0 {
		NoteRedactedServedModel(ctx, source)
		return
	}
	noteRoute(ctx, RouteServedModel, model)
	noteRoute(ctx, RouteModelStatus, "reported")
	noteRoute(ctx, RouteModelSource, modelSource(source))
}

// NoteRedactedServedModel preserves the distinction between a withheld upstream
// identity and an absent report. It never records the untrusted identifier.
func NoteRedactedServedModel(ctx context.Context, source string) {
	if ctx != nil {
		if observer, ok := ctx.Value(routeObserverKey{}).(func(string, string)); ok {
			observer(RouteServedModel, "")
		}
	}
	noteRoute(ctx, RouteModelStatus, "redacted")
	noteRoute(ctx, RouteModelSource, modelSource(source))
}

func NoteModelFallback(ctx context.Context, fallback bool) {
	noteRoute(ctx, RouteModelFallback, strconv.FormatBool(fallback))
}

func modelSource(source string) string {
	switch source {
	case "warp.model_used", "chat.model", "responses.model", "grok.modelResponse":
		return source
	default:
		return "upstream_response"
	}
}

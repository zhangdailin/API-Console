package upstream

import (
	"context"
	"net/url"
	"strconv"
	"strings"
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
	RouteServedModel = "upstream_model"
	// RouteProvider is the provider an upstream gateway reports it finally
	// routed the request to (Cline: provider_metadata.gateway.routing).
	RouteProvider = "upstream_provider"
	// RouteProviderAttempts is how many provider attempts the gateway reports
	// for that request; "0" means it did not say.
	RouteProviderAttempts = "upstream_provider_attempts"
)

type routeObserverKey struct{}

// WithRouteObserver installs the callback that records route facts for the
// request carried by ctx. A later note for the same field replaces an earlier
// one, so after retries the journal holds the attempt that answered.
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
	noteRoute(ctx, RouteServedModel, model)
}

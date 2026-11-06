package upstream

import (
	"context"
	"testing"

	"orchids-api/internal/testutil"
)

func TestRouteNotesKeepHostAndPathAndLastAttempt(t *testing.T) {
	got := map[string]string{}
	ctx := WithRouteObserver(context.Background(), func(field, value string) { got[field] = value })

	NoteUpstreamCall(ctx, "https://api.example.com/v1/chat/completions?token=secret", "model-a")
	NoteServedModel(ctx, "model-a-2026")
	// A retry on another endpoint replaces the first attempt's facts; an empty
	// served model (an upstream that names none) keeps the reported one.
	NoteUpstreamCall(ctx, "https://edge.example.com/v2/chat/completions", "model-b")
	NoteServedModel(ctx, "  ")

	testutil.Equal(t, got[RouteHost], "edge.example.com")
	testutil.Equal(t, got[RouteEndpoint], "/v2/chat/completions")
	testutil.Equal(t, got[RouteSentModel], "model-b")
	testutil.Equal(t, got[RouteServedModel], "model-a-2026")
	for _, value := range got {
		testutil.CheckNotContain(t, value, "secret")
	}
}

func TestRouteNotesWithoutObserverAreNoOps(t *testing.T) {
	NoteUpstreamCall(context.Background(), "https://api.example.com/v1/responses", "m")
	NoteServedModel(context.Background(), "m")
	testutil.Equal(t, WithRouteObserver(context.Background(), nil), context.Background())
}

func TestNoteServedProviderRecordsProviderAndAttemptsTogether(t *testing.T) {
	got := map[string]string{}
	ctx := WithRouteObserver(context.Background(), func(field, value string) { got[field] = value })
	NoteServedProvider(ctx, "alibaba", 2)
	testutil.Equal(t, got[RouteProvider], "alibaba")
	testutil.Equal(t, got[RouteProviderAttempts], "2")
	// A later attempt that names a provider but no count must not keep the old count.
	NoteServedProvider(ctx, "deepseek", 0)
	testutil.Equal(t, got[RouteProvider], "deepseek")
	testutil.Equal(t, got[RouteProviderAttempts], "0")
	// No provider: nothing is recorded.
	NoteServedProvider(ctx, " ", 5)
	testutil.Equal(t, got[RouteProvider], "deepseek")
}

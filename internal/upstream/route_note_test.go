package upstream

import (
	"context"
	"strings"
	"testing"

	"orchids-api/internal/testutil"
)

func TestRouteNotesKeepHostAndPathAndLastAttempt(t *testing.T) {
	got := map[string]string{}
	ctx := WithRouteObserver(context.Background(), func(field, value string) { got[field] = value })

	NoteUpstreamCall(ctx, "https://api.example.com/v1/chat/completions?token=secret", "model-a")
	NoteServedModel(ctx, "model-a-2026")
	// A retry on another endpoint must not inherit a previous response identity.
	NoteUpstreamCall(ctx, "https://edge.example.com/v2/chat/completions", "model-b")
	NoteServedModel(ctx, "  ")

	testutil.Equal(t, got[RouteHost], "edge.example.com")
	testutil.Equal(t, got[RouteEndpoint], "/v2/chat/completions")
	testutil.Equal(t, got[RouteSentModel], "model-b")
	testutil.Equal(t, got[RouteServedModel], "")
	testutil.Equal(t, got[RouteModelStatus], "unreported")
	testutil.Equal(t, got[RouteModelSource], "")
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

func TestRouteNotesResetResponseIdentityAndRetainOnlyNewAttemptEvidence(t *testing.T) {
	got := map[string]string{}
	ctx := WithRouteObserver(context.Background(), func(k, v string) { got[k] = v })
	NoteUpstreamCall(ctx, "https://first.example/chat?token=private", "alias-a")
	NoteServedModelWithSource(ctx, "actual-a", "chat.model")
	NoteServedProvider(ctx, "provider-a", 2)
	NoteModelFallback(ctx, true)
	NoteUpstreamCall(ctx, "https://second.example/responses", "alias-b")
	for _, key := range []string{RouteServedModel, RouteModelSource, RouteModelFallback, RouteProvider, RouteProviderAttempts} {
		if got[key] != "" {
			t.Fatalf("previous attempt retained %s", key)
		}
	}
	NoteServedModelWithSource(ctx, "actual-b", "responses.model")
	NoteServedModel(ctx, "")
	if got[RouteServedModel] != "actual-b" || got[RouteModelSource] != "responses.model" || got[RouteModelStatus] != "reported" {
		t.Fatal("current response identity missing")
	}
	NoteRedactedServedModel(ctx, "untrusted-source")
	if got[RouteServedModel] != "" || got[RouteModelStatus] != "redacted" || got[RouteModelSource] != "upstream_response" {
		t.Fatal("unsafe identity/source was not withheld")
	}
}

func TestRouteNotesWithholdMalformedResponseIdentifiers(t *testing.T) {
	for _, model := range []string{"model\nprivate", "model\x1bprivate", string([]byte{0xff}), strings.Repeat("m", 257)} {
		got := map[string]string{}
		ctx := WithRouteObserver(context.Background(), func(k, v string) { got[k] = v })
		NoteServedModel(ctx, model)
		if got[RouteServedModel] != "" || got[RouteModelStatus] != "redacted" {
			t.Fatal("unsafe identifier reached journal")
		}
	}
}

package qoder

import (
	"encoding/json"
	"testing"

	"orchids-api/internal/upstream"
)

// A cache hint the upstream has no wire field for must be ignored, not
// rejected, and must not otherwise change the request.
//
// The comparison is on decoded maps rather than raw bytes because
// buildChatBodyProfile stamps business.begin_at with the current time: two
// calls are byte-different whenever they cross a millisecond, which made the
// byte comparison fail intermittently for a reason unrelated to the hint.
func TestProtocolControlsInWireParameters(t *testing.T) {
	req := upstream.UpstreamRequest{Model: "m", Prompt: "hi", ResponseFormat: map[string]any{"type": "json_object"}}
	raw, err := buildChatBodyProfile(req, modelEntry{Key: "m"}, "s", "r", "set", "1", "user", "product")
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(decoded), &body); err != nil {
		t.Fatal(err)
	}
	parameters := body["parameters"].(map[string]any)
	if parameters["response_format"].(map[string]any)["type"] != "json_object" {
		t.Fatalf("format lost: %s", decoded)
	}

	req.PromptCacheKey = "cache"
	withHint, err := buildChatBodyProfile(req, modelEntry{Key: "m"}, "s", "r", "set", "1", "user", "product")
	if err != nil {
		t.Fatalf("optional cache hint rejected: %v", err)
	}
	hintDecoded, err := decodeBody(withHint)
	if err != nil {
		t.Fatal(err)
	}
	var hintBody map[string]any
	if err := json.Unmarshal([]byte(hintDecoded), &hintBody); err != nil {
		t.Fatal(err)
	}
	// begin_at is a wall clock, so it is the one field allowed to differ.
	clearTimestamp(body)
	clearTimestamp(hintBody)
	if !equalJSON(t, body, hintBody) {
		t.Fatalf("ignored cache hint changed Qoder wire request\nbefore=%s\nafter=%s", decoded, hintDecoded)
	}
	// The hint is not forwarded because Qoder has no field for it.
	if _, leaked := hintBody["prompt_cache_key"]; leaked {
		t.Fatalf("cache hint forwarded to Qoder wire request: %s", hintDecoded)
	}
}

// clearTimestamp removes the one field the builder stamps from the clock.
func clearTimestamp(body map[string]any) {
	business, ok := body["business"].(map[string]any)
	if !ok {
		return
	}
	delete(business, "begin_at")
}

func equalJSON(t *testing.T, left, right map[string]any) bool {
	t.Helper()
	a, err := json.Marshal(left)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(right)
	if err != nil {
		t.Fatal(err)
	}
	return string(a) == string(b)
}

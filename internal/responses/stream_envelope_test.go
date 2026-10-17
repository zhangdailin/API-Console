package responses

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStreamEnvelopeRetainsErrorMessages(t *testing.T) {
	for _, input := range []string{
		"event: error\ndata: {\"message\":\"upstream unavailable\",\"vendor\":{\"code\":42}}\n\n",
		"data: {\"error\":{\"message\":\"upstream unavailable\",\"code\":42}}\n\n",
	} {
		w := httptest.NewRecorder()
		WriteStreamFromChatReader(w, CreateRequest{Model: "m"}, strings.NewReader(input), StreamOptions{})
		if !strings.Contains(w.Body.String(), "response.failed") || !strings.Contains(w.Body.String(), "upstream unavailable") {
			t.Fatalf("lost upstream error: %s", w.Body.String())
		}
	}
}

func TestStreamEnvelopeRejectsNonObjects(t *testing.T) {
	for _, input := range []string{"null", "[]", "42", `"text"`} {
		w := httptest.NewRecorder()
		WriteStreamFromChatReader(w, CreateRequest{Model: "m"}, strings.NewReader("data: "+input+"\n\n"), StreamOptions{})
		if !strings.Contains(w.Body.String(), "response.failed") || strings.Contains(w.Body.String(), "response.completed") {
			t.Fatalf("invalid envelope %s: %s", input, w.Body.String())
		}
	}
}

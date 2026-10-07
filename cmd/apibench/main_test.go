package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProbeDistinguishesReasoningTruncationAndMissingTerminal(t *testing.T) {
	for _, terminal := range []bool{true, false} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"finish_reason\":\"length\"}],\"usage\":{\"completion_tokens\":64}}\n\n")
			if terminal {
				fmt.Fprint(w, "data: [DONE]\n\n")
			}
		}))
		r := probe(server.Client(), server.URL, "test-key", "m", "high", 64, time.Second)
		server.Close()
		if r.FirstGeneratedMS == nil || r.FirstVisibleMS != nil || r.OutputTokens != 64 || r.Finish != "length" {
			t.Fatalf("evidence lost: %+v", r)
		}
		if terminal && r.Error != "" {
			t.Fatal(r.Error)
		}
		if !terminal && r.Error != "missing_terminal" {
			t.Fatal("EOF must not count as protocol success")
		}
	}
}

func TestProbeJoinsDeltaFragmentsWithoutInventingSpaces(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"1 2 3 4 5 6 7 8 9 1"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"0 11 12 13 14 15 16 17 18 19 20"},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	r := probe(server.Client(), server.URL, "test-key", "m", "", 1024, time.Second)
	if !r.AnswerCorrect || r.Error != "" {
		t.Fatal("split token corrupted the answer", r)
	}
}

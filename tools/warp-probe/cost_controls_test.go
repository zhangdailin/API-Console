package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
)

func TestMinimalProbeRejectsHiddenActivityAndModelFallback(t *testing.T) {
	for _, m := range []*warpapi.Message{
		warpapi.Message_builder{ServerEvent: warpapi.Message_ServerEvent_builder{}.Build()}.Build(),
		warpapi.Message_builder{EventsFromAgents: warpapi.Message_EventsFromAgents_builder{}.Build()}.Build(),
		warpapi.Message_builder{ToolCallResult: warpapi.Message_ToolCallResult_builder{}.Build()}.Build(),
		warpapi.Message_builder{ModelUsed: warpapi.Message_ModelUsed_builder{ModelId: proto.String("other-model")}.Build()}.Build(),
		warpapi.Message_builder{ModelUsed: warpapi.Message_ModelUsed_builder{ModelId: proto.String("chosen-model"), IsFallback: proto.Bool(true)}.Build()}.Build(),
		warpapi.Message_builder{RequestMetadata: warpapi.Message_RequestMetadata_builder{ToolCallSummary: warpapi.Message_RequestMetadata_ToolCallSummary_builder{FilesChanged: proto.Uint32(1)}.Build()}.Build()}.Build(),
	} {
		event := probeTestAction(warpapi.ClientAction_builder{AddMessagesToTask: warpapi.ClientAction_AddMessagesToTask_builder{Messages: []*warpapi.Message{m}}.Build()}.Build())
		result, err := readProbeStreamForModel(strings.NewReader(probeTestSSE(t, event)+probeTestSSE(t, probeTestDone())), "chosen-model")
		if err == nil || result.Complete {
			t.Fatal("hidden activity or model substitution accepted")
		}
	}
}

func TestMinimalProbeRejectsMalformedTransactions(t *testing.T) {
	begin := probeTestSSE(t, probeTestAction(warpapi.ClientAction_builder{BeginTransaction: warpapi.ClientAction_BeginTransaction_builder{}.Build()}.Build()))
	commit := probeTestSSE(t, probeTestAction(warpapi.ClientAction_builder{CommitTransaction: warpapi.ClientAction_CommitTransaction_builder{}.Build()}.Build()))
	rollback := probeTestSSE(t, probeTestAction(warpapi.ClientAction_builder{RollbackTransaction: warpapi.ClientAction_RollbackTransaction_builder{}.Build()}.Build()))
	for _, s := range []string{commit, rollback, begin + begin, begin + probeTestSSE(t, probeTestDone())} {
		result, err := readProbeStream(strings.NewReader(s))
		if err == nil || result.Complete {
			t.Fatal("malformed transaction accepted")
		}
	}
}

func TestMinimalProbeCapsOutputAndDoesNotRewriteModel(t *testing.T) {
	for _, model := range []string{"auto", "auto-open", "auto-efficient", "auto-genius", "cli-agent-auto", "computer-use-agent-auto"} {
		if _, err := buildProbeRequest(model); err == nil {
			t.Fatal("auto routing accepted")
		}
	}
	raw, err := buildProbeRequest("explicit-model")
	if err != nil {
		t.Fatal(err)
	}
	var request warpapi.Request
	if proto.Unmarshal(raw, &request) != nil || request.GetSettings().GetModelConfig().GetBase() != "explicit-model" {
		t.Fatal("explicit model changed")
	}
	event := probeTestAction(warpapi.ClientAction_builder{AddMessagesToTask: warpapi.ClientAction_AddMessagesToTask_builder{Messages: []*warpapi.Message{probeTestOutput("m", strings.Repeat("x", 4097))}}.Build()}.Build())
	_, err = readProbeStream(strings.NewReader(probeTestSSE(t, event)))
	if !errors.Is(err, errProbeLimit) {
		t.Fatal("oversize output accepted")
	}
}

func TestUnobservedModelNeverSendsRequest(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) }))
	defer s.Close()
	c := &probeClient{http: s.Client(), base: s.URL}
	_, err := c.generateObserved(context.Background(), "synthetic-token", "unobserved", []string{"observed"})
	if err == nil || calls != 0 {
		t.Fatal("unobserved model reached upstream")
	}
}

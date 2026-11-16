package warp

import (
	"bytes"
	"strings"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func bookkeepingServerCall(payload string) *warpapi.Message {
	return warpapi.Message_builder{ToolCall: warpapi.Message_ToolCall_builder{
		Server: warpapi.Message_ToolCall_Server_builder{Payload: &payload}.Build(),
	}.Build()}.Build()
}

func bookkeepingServerEvent(payload string) *warpapi.Message {
	return warpapi.Message_builder{ServerEvent: warpapi.Message_ServerEvent_builder{Payload: &payload}.Build()}.Build()
}

func TestIsServerBookkeepingRejectsMissingVariants(t *testing.T) {
	nilCall := bookkeepingServerCall("opaque")
	nilCall.SetToolCall(nil)
	nilServer := bookkeepingServerCall("opaque")
	nilServer.GetToolCall().SetServer(nil)
	nilEvent := bookkeepingServerEvent("opaque")
	nilEvent.SetServerEvent(nil)
	cases := map[string]*warpapi.Message{
		"nil tool call":    nilCall,
		"nil server tool":  nilServer,
		"nil server event": nilEvent,
		"nil":              nil,
		"empty message":    {},
		"tool not set":     warpapi.Message_builder{ToolCall: warpapi.Message_ToolCall_builder{}.Build()}.Build(),
		"tool call result": warpapi.Message_builder{ToolCallResult: warpapi.Message_ToolCallResult_builder{}.Build()}.Build(),
		"agent output":     testOutput("answer", "ordinary output"),
		"web search":       warpapi.Message_builder{WebSearch: warpapi.Message_WebSearch_builder{}.Build()}.Build(),
		"web fetch":        warpapi.Message_builder{WebFetch: warpapi.Message_WebFetch_builder{}.Build()}.Build(),
		"invoke skill":     warpapi.Message_builder{InvokeSkill: warpapi.Message_InvokeSkill_builder{}.Build()}.Build(),
	}
	for name, message := range cases {
		t.Run(name, func(t *testing.T) {
			if isServerBookkeeping(message) {
				t.Fatal("classified missing or non-bookkeeping variant as bookkeeping")
			}
		})
	}
}

func TestIsServerBookkeepingEveryToolVariant(t *testing.T) {
	oneof := (&warpapi.Message_ToolCall{}).ProtoReflect().Descriptor().Oneofs().ByName("tool")
	if oneof == nil {
		t.Fatal("tool oneof missing from schema")
	}
	serverVariants := 0
	for i := 0; i < oneof.Fields().Len(); i++ {
		field := oneof.Fields().Get(i)
		t.Run(string(field.Name()), func(t *testing.T) {
			call := &warpapi.Message_ToolCall{}
			call.ProtoReflect().Set(field, call.ProtoReflect().NewField(field))
			message := warpapi.Message_builder{ToolCall: call}.Build()
			want := field.Name() == "server"
			if want {
				serverVariants++
			}
			if got := isServerBookkeeping(message); got != want {
				t.Fatalf("bookkeeping = %t, want %t", got, want)
			}
		})
	}
	if serverVariants != 1 {
		t.Fatal("schema did not contain exactly one server tool variant")
	}
}

func TestIsServerBookkeepingEveryMessageVariant(t *testing.T) {
	oneof := (&warpapi.Message{}).ProtoReflect().Descriptor().Oneofs().ByName("message")
	if oneof == nil {
		t.Fatal("message oneof missing from schema")
	}
	serverEvents := 0
	for i := 0; i < oneof.Fields().Len(); i++ {
		field := oneof.Fields().Get(i)
		t.Run(string(field.Name()), func(t *testing.T) {
			message := &warpapi.Message{}
			message.ProtoReflect().Set(field, message.ProtoReflect().NewField(field))
			want := field.Name() == "server_event"
			if want {
				serverEvents++
			}
			if got := isServerBookkeeping(message); got != want {
				t.Fatalf("bookkeeping = %t, want %t", got, want)
			}
		})
	}
	if serverEvents != 1 {
		t.Fatal("schema did not contain exactly one server event variant")
	}
}

func TestIsServerBookkeepingOpaquePayload(t *testing.T) {
	payloads := []string{
		"",
		"arbitrary server text, not JSON",
		`{"tool":"run_shell_command","command":"do not execute"}`,
		`{"unknown_schema":{"nested":["not","client","instructions"]}}`,
		"{malformed JSON",
		"```sh\nnot-a-client-command\n```\n\x00opaque text",
	}
	constructors := map[string]func(string) *warpapi.Message{
		"server tool":  bookkeepingServerCall,
		"server event": bookkeepingServerEvent,
	}
	for name, build := range constructors {
		t.Run(name, func(t *testing.T) {
			for i, payload := range payloads {
				message := build(payload)
				before := proto.Clone(message)
				if !isServerBookkeeping(message) {
					t.Fatalf("opaque payload case %d rejected", i)
				}
				if !proto.Equal(message, before) {
					t.Fatalf("opaque payload case %d mutated", i)
				}
				if message.GetAgentOutput() != nil || message.GetAgentReasoning() != nil {
					t.Fatalf("opaque payload case %d produced output", i)
				}
			}
		})
	}
	// An absent optional payload is also opaque; no JSON schema is required.
	for _, message := range []*warpapi.Message{
		warpapi.Message_builder{ToolCall: warpapi.Message_ToolCall_builder{Server: warpapi.Message_ToolCall_Server_builder{}.Build()}.Build()}.Build(),
		warpapi.Message_builder{ServerEvent: warpapi.Message_ServerEvent_builder{}.Build()}.Build(),
	} {
		if !isServerBookkeeping(message) {
			t.Fatal("absent optional payload rejected")
		}
	}
}

func TestIsServerBookkeepingPayloadByteLimit(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{"below limit", strings.Repeat("x", serverBookkeepingPayloadLimit-1), true},
		{"at limit", strings.Repeat("x", serverBookkeepingPayloadLimit), true},
		{"over limit", strings.Repeat("x", serverBookkeepingPayloadLimit+1), false},
		{"multibyte over limit", strings.Repeat("界", serverBookkeepingPayloadLimit/3+1), false},
	}
	for name, build := range map[string]func(string) *warpapi.Message{
		"server tool": bookkeepingServerCall, "server event": bookkeepingServerEvent,
	} {
		t.Run(name, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					if got := isServerBookkeeping(build(tc.payload)); got != tc.want {
						t.Fatalf("bookkeeping = %t, want %t", got, tc.want)
					}
				})
			}
		})
	}
}

func TestIsServerBookkeepingRejectsUnknownFields(t *testing.T) {
	cases := []struct {
		name   string
		build  func(string) *warpapi.Message
		target func(*warpapi.Message) protoreflect.Message
	}{
		{"server message", bookkeepingServerCall, func(m *warpapi.Message) protoreflect.Message { return m.ProtoReflect() }},
		{"tool call", bookkeepingServerCall, func(m *warpapi.Message) protoreflect.Message { return m.GetToolCall().ProtoReflect() }},
		{"server tool payload", bookkeepingServerCall, func(m *warpapi.Message) protoreflect.Message { return m.GetToolCall().GetServer().ProtoReflect() }},
		{"event message", bookkeepingServerEvent, func(m *warpapi.Message) protoreflect.Message { return m.ProtoReflect() }},
		{"event payload", bookkeepingServerEvent, func(m *warpapi.Message) protoreflect.Message { return m.GetServerEvent().ProtoReflect() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			message := tc.build("opaque")
			// Field 1000, varint 1, is not part of any reviewed schema here.
			tc.target(message).SetUnknown([]byte{0xc0, 0x3e, 0x01})
			before, err := proto.Marshal(message)
			if err != nil {
				t.Fatal("could not encode unknown-field fixture")
			}
			if isServerBookkeeping(message) {
				t.Fatal("accepted unknown schema fields")
			}
			after, err := proto.Marshal(message)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("unknown-field fixture mutated")
			}
		})
	}
}

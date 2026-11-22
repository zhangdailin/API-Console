package grok

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"orchids-api/internal/chatwire"
)

func TestBuildChatDocumentWire(t *testing.T) {
	const fileURL = "https://example.test/report.pdf"
	const fileData = "data:application/pdf;base64,JVBERi0xLjQK"
	tests := []struct{ name, input, field, value string }{
		{"chat nested data", `{"type":"file","file":{"file_data":"` + fileData + `","filename":"report.pdf"}}`, "file_data", fileData},
		{"chat nested URL", `{"type":"file","file":{"file_url":"` + fileURL + `","filename":"report.pdf"}}`, "file_url", fileURL},
		{"chat flattened URL", `{"type":"input_file","file_url":"` + fileURL + `","filename":"report.pdf"}`, "file_url", fileURL},
		{"chat legacy URL", `{"type":"file","file":{"file_data":"` + fileURL + `","filename":"report.pdf"}}`, "file_url", fileURL},
		{"messages URL", `{"type":"document","title":"report.pdf","source":{"type":"url","url":"` + fileURL + `"}}`, "file_url", fileURL},
		{"messages base64", `{"type":"document","title":"report.pdf","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0xLjQK"}}`, "file_data", fileData},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			message := `{"model":"grok-4.7","max_tokens":128,"messages":[{"role":"user","content":[{"type":"text","text":" "},{"type":"text","text":"Read this PDF"},` + tc.input + `]}]}`
			var req chatwire.Request
			if strings.HasPrefix(tc.name, "messages") {
				var raw anthropicMessagesRequest
				if err := json.Unmarshal([]byte(message), &raw); err != nil {
					t.Fatal(err)
				}
				var err error
				req, err = anthropicRequestToChat(raw)
				if err != nil {
					t.Fatal(err)
				}
			} else if err := json.Unmarshal([]byte(message), &req); err != nil {
				t.Fatal(err)
			}
			if err := normalizeBuildChatDocuments(req.Messages); err != nil {
				t.Fatal(err)
			}
			if err := req.Validate(); err != nil {
				t.Fatal(err)
			}
			payload, err := (&Handler{}).responsesPayloadFromChat(ModelSpec{UpstreamModel: req.Model}, &req, true)
			if err != nil {
				t.Fatal(err)
			}
			got := payload["input"].([]any)[0].(map[string]any)["content"]
			want := []any{
				map[string]any{"type": "input_text", "text": "Read this PDF"},
				map[string]any{"type": "input_file", "filename": "report.pdf", tc.field: tc.value},
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Build input = %#v, want %#v", got, want)
			}
		})
	}
}

func TestBuildChatRejectsAmbiguousOrUnscopedDocuments(t *testing.T) {
	for _, file := range []string{
		`{"type":"input_file","file_id":"file_foreign"}`,
		`{"type":"input_file","file_url":"https://example.test/a.pdf","file_data":"data:application/pdf;base64,JVBERg=="}`,
		`{"type":"input_file","file_url":"https://user:secret@example.test/a.pdf"}`,
		`{"type":"file","file":{"file_data":"JVBERg=="}}`,
	} {
		var req chatwire.Request
		if err := json.Unmarshal([]byte(`{"model":"grok-4.7","messages":[{"role":"user","content":[`+file+`]}]}`), &req); err != nil {
			t.Fatal(err)
		}
		if err := normalizeBuildChatDocuments(req.Messages); err == nil || !strings.Contains(err.Error(), "messages[0].content[0]") {
			t.Fatalf("expected a file validation error, got %v", err)
		}
	}
}

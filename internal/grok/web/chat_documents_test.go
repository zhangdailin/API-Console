package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"orchids-api/internal/config"
)

func TestChatDocumentUsesUpstreamMultipartUpload(t *testing.T) {
	const fixture = "JVBERi0xLjQKJSVFT0Y="
	for _, part := range []string{
		`{"type":"file","file":{"filename":"report.pdf","file_data":"data:application/pdf;base64,` + fixture + `"}}`,
		`{"type":"input_file","filename":"report.pdf","file_data":"data:application/pdf;base64,` + fixture + `"}`,
	} {
		var request ChatCompletionsRequest
		raw := `{"model":"grok-chat-fast","messages":[{"role":"user","content":[{"type":"text","text":" "},` + part + `,{"type":"input_text","text":"Summarize this"}]}]}`
		if err := json.Unmarshal([]byte(raw), &request); err != nil {
			t.Fatal(err)
		}
		if err := request.Validate(); err != nil {
			t.Fatal(err)
		}
		prompt, inputs, err := extractMessageAndAttachmentsWithTools(request.Messages, false, nil, nil, false)
		if err != nil || prompt != "Summarize this" || len(inputs) != 1 {
			t.Fatalf("prompt=%q, attachments=%d, err=%v", prompt, len(inputs), err)
		}
		client := &Client{cfg: &config.Config{GrokStatsigSignerURL: new("")}, httpClient: &http.Client{
			Transport: chatAssetTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodPost || r.URL.Path != "/http/upload-file-v2/direct" {
					t.Fatalf("unexpected upload route: %s %s", r.Method, r.URL.Path)
				}
				reader, err := r.MultipartReader()
				if err != nil {
					t.Fatal(err)
				}
				file, err := reader.NextPart()
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(file)
				if err != nil || file.FormName() != "file" || file.FileName() != "report.pdf" ||
					file.Header.Get("Content-Type") != "application/pdf" || !bytes.Equal(data, []byte("%PDF-1.4\n%%EOF")) {
					t.Fatalf("invalid document multipart payload: %s %s %v", file.FormName(), file.FileName(), err)
				}
				if _, err := reader.NextPart(); err != io.EOF {
					t.Fatal("chat upload must not send an Imagine file source")
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"fileMetadata":{"fileMetadataId":"fixture-upload"}}`))}, nil
			}),
		}}
		h := &Handler{client: client}
		prepared, err := h.prepareChatAttachments(t.Context(), append(inputs, inputs...))
		if err != nil {
			t.Fatal(err)
		}
		ids, err := h.uploadChatAttachments(t.Context(), "fixture", prepared)
		if err != nil || len(ids) != 1 || ids[0] != "fixture-upload" {
			t.Fatalf("IDs=%v, err=%v", ids, err)
		}
		payload := h.buildChatPayload(ModelSpec{}, prompt, ids, &request)
		if got := payload["fileAttachments"].([]string); len(got) != 1 || got[0] != ids[0] {
			t.Fatalf("chat payload missing the upstream attachment: %v", got)
		}
	}
}

func TestChatDocumentValidationBoundaries(t *testing.T) {
	for _, tc := range []struct{ part, want string }{
		{`{"type":"text","text":""}`, "text content cannot be empty"},
		{`{"type":"input_file","file_id":"file_unavailable"}`, "file_id is unsupported"},
		{`{"type":"input_file","file_url":"file:///report.pdf"}`, "must be an HTTP or HTTPS URL"},
		{`{"type":"input_file","file_url":"https://example.com/report.pdf","file_data":"data:application/pdf;base64,abc"}`, "exactly one"},
	} {
		var request ChatCompletionsRequest
		if err := json.Unmarshal([]byte(`{"model":"grok-chat-fast","messages":[{"role":"user","content":[`+tc.part+`]}]}`), &request); err != nil {
			t.Fatal(err)
		}
		if err := request.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("want %q, got %v", tc.want, err)
		}
	}
	h := &Handler{}
	if _, err := h.loadChatAttachment(t.Context(), AttachmentInput{Data: "data:application/pdf;base64,aGVsbG8="}, 100, true); err == nil {
		t.Fatal("non-PDF body declared as PDF passed validation")
	}
}

func TestChatAttachmentBatchRejectsInvalidLaterFile(t *testing.T) {
	h := &Handler{}
	prepared, err := h.prepareChatAttachments(t.Context(), []AttachmentInput{
		{Type: "file", Filename: "first.pdf", Data: "data:application/pdf;base64,JVBERi0xLjQKJSVFT0Y="},
		{Type: "image", Data: "data:image/png;base64,invalid%%"},
	})
	if err == nil || prepared != nil {
		t.Fatalf("a rejected batch must leave nothing to upload: %v", err)
	}
}

func TestChatDocumentOfficeMIMEFallback(t *testing.T) {
	h := &Handler{}
	file, err := h.loadChatAttachment(t.Context(), AttachmentInput{
		Type: "file", Filename: "report.docx", Data: "data:application/octet-stream;base64,UEsDBA==",
	}, 100, true)
	if err != nil {
		t.Fatal(err)
	}
	if file.name != "report.docx" || file.mime != "application/vnd.openxmlformats-officedocument.wordprocessingml.document" {
		t.Fatalf("name=%q MIME=%q", file.name, file.mime)
	}
}

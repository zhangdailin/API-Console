package warp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

func TestWarpImagesEnterOfficialInputContextInTranscriptOrder(t *testing.T) {
	pngData := warpTestImage(t, "image/png")
	jpegData := warpTestImage(t, "image/jpeg")
	// Exercise the two public image forms with the same history the gateway
	// has to replay if a client includes an image from a previous turn.
	raw := `{"model":"model-a","messages":[
	  {"role":"user","content":[{"type":"text","text":"Earlier question"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + pngData + `"}}]},
	  {"role":"assistant","content":"Earlier answer"},
	  {"role":"user","content":[{"type":"text","text":"Current question"},{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,` + jpegData + `"}}]}
	]}`
	req, prepared, err := warpTestImageRequest(raw)
	if err != nil {
		t.Fatalf("valid image request rejected: %v", err)
	}
	body, err := buildRequest(req.Model, prepared)
	if err != nil {
		t.Fatalf("cannot encode image request: %v", err)
	}
	var decoded warpapi.Request
	if err := proto.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	images := decoded.GetInput().GetContext().GetImages()
	if len(images) != 2 {
		t.Fatalf("Warp received %d images, want 2", len(images))
	}
	for i, want := range []struct{ mime, data string }{{"image/png", pngData}, {"image/jpeg", jpegData}} {
		if got := images[i]; got.GetMimeType() != want.mime || string(got.GetData()) != want.data {
			t.Errorf("Warp image %d has the wrong media type, encoding, or position", i)
		}
	}
	inputs := decoded.GetInput().GetUserInputs().GetInputs()
	if len(inputs) != 1 || inputs[0].GetUserQuery() == nil {
		t.Fatalf("expected a single replayed query, got %d inputs", len(inputs))
	}
	query := inputs[0].GetUserQuery().GetQuery()
	earlier := strings.Index(query, "User: Earlier question")
	answer := strings.Index(query, "Assistant: Earlier answer")
	current := strings.Index(query, "User: Current question")
	if earlier < 0 || answer <= earlier || current <= answer {
		t.Fatalf("original transcript not preserved in order: %q", query)
	}
	if strings.Contains(query, pngData) || strings.Contains(query, jpegData) {
		t.Error("image bytes were copied into the text query")
	}
	if decoded.GetSettings().GetSupportsReadImageFiles() {
		t.Error("attachments unexpectedly enabled the file-reading tool")
	}
}

func TestWarpImageOnlyReachesGeneration(t *testing.T) {
	pngData := warpTestImage(t, "image/png")
	req, _, err := warpTestImageRequest(`{"model":"model-a","messages":[{"role":"user","content":[
	  {"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + pngData + `"}}
	]}]}`)
	if err != nil {
		t.Fatalf("image-only request rejected: %v", err)
	}
	stream := testSSE(t, testMessages(testOutput("m", "image seen"))) + testSSE(t, testDone())
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, err := io.ReadAll(r.Body)
		var wire warpapi.Request
		if err != nil || proto.Unmarshal(body, &wire) != nil || r.URL.Path != "/ai/multi-agent" {
			t.Error("invalid Warp generation request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		images := wire.GetInput().GetContext().GetImages()
		if len(images) != 1 || images[0].GetMimeType() != "image/png" || string(images[0].GetData()) != pngData {
			t.Error("single image did not reach the generation request")
		}
		if inputs := wire.GetInput().GetUserInputs().GetInputs(); len(inputs) != 1 || inputs[0].GetUserQuery() == nil {
			t.Error("image-only turn did not create a user query")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	}))
	defer server.Close()
	client := NewFromAccount(testAccount(), testConfig(server.URL))
	invalid := testRequest()
	invalid.Messages[0].Content = prompt.MessageContent{Blocks: []prompt.ContentBlock{
		{Type: "text", Text: "Do not silently ignore the attachment."},
		{Type: "image", Source: &prompt.ImageSource{Type: "base64", MediaType: "image/png", Data: "invalid"}},
	}}
	var appErr *apperrors.AppError
	if err := client.SendRequestWithPayload(t.Context(), invalid, nil, nil); !errors.As(err, &appErr) ||
		appErr.HTTPStatus != http.StatusBadRequest || calls.Load() != 0 {
		t.Fatalf("invalid image did not fail before network calls: %v (calls %d)", err, calls.Load())
	}
	textOnly := testAccount()
	textOnly.WarpModelVision = map[string]bool{req.Model: false}
	appErr = nil
	if err := NewFromAccount(textOnly, testConfig(server.URL)).SendRequestWithPayload(t.Context(), req, nil, nil); !errors.As(err, &appErr) ||
		appErr.HTTPStatus != http.StatusBadRequest || calls.Load() != 0 {
		t.Fatalf("known text-only model should reject images before network calls: %v (calls %d)", err, calls.Load())
	}
	if err := client.SendRequestWithPayload(t.Context(), req, nil, nil); err != nil || calls.Load() != 1 {
		t.Fatalf("wanted one image generation request: %v (calls %d)", err, calls.Load())
	}
}

func TestWarpImagesOnBoundToolTurnAndExplicitResume(t *testing.T) {
	ctx, binding, req, _ := recoveryFixture(t)
	earlier := warpTestImage(t, "image/jpeg")
	current := warpTestImage(t, "image/png")
	req.Messages[0].Content = prompt.MessageContent{Blocks: []prompt.ContentBlock{
		{Type: "text", Text: "Earlier context"},
		{Type: "image", Source: &prompt.ImageSource{Type: "base64", MediaType: "image/jpeg", Data: earlier}},
	}}
	req.Messages[2].Content.Blocks = append(req.Messages[2].Content.Blocks,
		prompt.ContentBlock{Type: "image", Source: &prompt.ImageSource{Type: "base64", MediaType: "image/png", Data: current}})
	// The provider can echo the accepted user inputs into the private root.
	// The explicit resume must keep that snapshot without resending its image.
	accepted := warpapi.Message_builder{
		Id: proto.String("accepted-image-query"),
		UserQuery: warpapi.Message_UserQuery_builder{
			Query: proto.String("Accepted image"),
			Context: warpapi.InputContext_builder{Images: []*warpapi.InputContext_Image{
				warpapi.InputContext_Image_builder{Data: []byte(current), MimeType: proto.String("image/png")}.Build(),
			}}.Build(),
		}.Build(),
	}.Build()
	failed := integrityStream(t, initEvent(t, binding.ConversationID), integrityTaskMessages(accepted))
	done := integrityStream(t, initEvent(t, binding.ConversationID),
		integrityTaskMessages(continuationText("resume-answer", "done")), testDone())
	requests := make(chan *warpapi.Request, 2)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		raw, err := io.ReadAll(r.Body)
		wire := &warpapi.Request{}
		if call > 2 || err != nil || proto.Unmarshal(raw, wire) != nil || r.URL.Path != "/ai/multi-agent" {
			t.Error("unexpected generation request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests <- wire
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			_, _ = io.WriteString(w, failed)
			return
		}
		_, _ = io.WriteString(w, done)
	}))
	defer server.Close()
	s := catalogRotationStore(t)
	account := testAccount()
	if err := s.CreateAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	client := newClientWithStore(account, testConfig(server.URL), s)
	if !client.bindings.saveScoped(ctx, account.ID, req.Model, "call-recovery", binding) {
		t.Fatal("could not seed the owned tool binding")
	}
	err := client.SendRequestWithPayload(ctx, req, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "X-Warp-Resume: true") || calls.Load() != 1 {
		t.Fatalf("expected replay-safe failed generation with an explicit resume option: %v", err)
	}
	first := (<-requests).GetInput()
	images := first.GetContext().GetImages()
	if len(images) != 1 || string(images[0].GetData()) != current || len(first.GetUserInputs().GetInputs()) != 2 {
		t.Fatal("bound tool turn did not send only the current image and keep the typed tool result")
	}
	if err := client.SendRequestWithPayload(upstream.WithExplicitResume(ctx, true), req, nil, nil); err != nil || calls.Load() != 2 {
		t.Fatalf("explicit image turn resume failed: %v", err)
	}
	resumed := <-requests
	in := resumed.GetInput()
	if len(in.GetContext().GetImages()) != 0 || in.GetUserInputs() != nil || in.GetResumeConversation() == nil {
		t.Fatal("explicit resume retransmitted images or user inputs")
	}
	tasks := resumed.GetTaskContext().GetTasks()
	if len(tasks) != 1 {
		t.Fatalf("unexpected number of restored task roots: %d", len(tasks))
	}
	var restored bool
	for _, message := range tasks[0].GetMessages() {
		restored = restored || proto.Equal(message, accepted)
	}
	if !restored {
		t.Fatal("explicit resume lost the provider-side image on the restored task")
	}
}

func TestWarpImageInputsFailLocally(t *testing.T) {
	pngData := warpTestImage(t, "image/png")
	const fixtureSecret = "synthetic-client-data"
	for _, tc := range []struct {
		name, messages string
	}{
		{"missing image URL", `[{"role":"user","content":[{"type":"image_url","image_url":{}}]}]`},
		{"invalid base64", `[{"role":"user","content":[{"type":"text","text":"Describe this."},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + fixtureSecret + `"}}]}]`},
		{"media type mismatch", `[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,` + pngData + `"}}]}]`},
		{"assistant image", `[{"role":"user","content":"question"},{"role":"assistant","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + pngData + `"}}]},{"role":"user","content":"follow up"}]`},
		{"remote URL", `[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.invalid/` + fixtureSecret + `"}}]}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := warpTestImageRequest(`{"model":"model-a","messages":` + tc.messages + `}`)
			var appErr *apperrors.AppError
			if !errors.As(err, &appErr) || appErr.HTTPStatus != http.StatusBadRequest {
				t.Fatalf("expected local HTTP 400, got %v", err)
			}
			if strings.Contains(err.Error(), fixtureSecret) || strings.Contains(err.Error(), pngData) {
				t.Error("validation error exposed client image content")
			}
		})
	}
}

func TestWarpImageBudgetIncludesReplayedUserTurns(t *testing.T) {
	var img bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.NoCompression}
	if err := encoder.Encode(&img, image.NewRGBA(image.Rect(0, 0, 320, 240))); err != nil {
		t.Fatal(err)
	}
	pngData := base64.StdEncoding.EncodeToString(img.Bytes())
	tinyPNG := warpTestImage(t, "image/png")
	turnWith := func(data string, n int) string {
		part := `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + data + `"}}`
		return `{"role":"user","content":[` + strings.Repeat(part+",", n-1) + part + `]}`
	}
	for _, tc := range []struct {
		name, earlier, current string
	}{
		{"image count", turnWith(tinyPNG, 4), turnWith(tinyPNG, 5)},
		{"inline bytes", turnWith(pngData, 1), turnWith(pngData, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, turn := range []string{tc.earlier, tc.current} {
				if _, _, err := warpTestImageRequest(`{"model":"model-a","messages":[` + turn + `]}`); err != nil {
					t.Fatalf("individual turn should be below the budget: %v", err)
				}
			}
			raw := `{"model":"model-a","messages":[` + tc.earlier +
				`,{"role":"assistant","content":"Earlier answer"},` + tc.current + `]}`
			_, _, err := warpTestImageRequest(raw)
			var appErr *apperrors.AppError
			if !errors.As(err, &appErr) || appErr.HTTPStatus != http.StatusBadRequest {
				t.Fatalf("joint image budget was not enforced: %v", err)
			}
		})
	}
}

func warpTestImageRequest(raw string) (upstream.UpstreamRequest, preparedRequest, error) {
	var request upstream.UpstreamRequest
	if err := validateRawRequest(json.RawMessage(raw)); err != nil {
		return request, preparedRequest{}, err
	}
	var wire struct {
		Model    string           `json:"model"`
		Messages []prompt.Message `json:"messages"`
	}
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		return request, preparedRequest{}, err
	}
	request = upstream.UpstreamRequest{Model: wire.Model, Messages: wire.Messages}
	prepared, err := validateRequest(request)
	return request, prepared, err
}

func warpTestImage(t *testing.T, mime string) string {
	t.Helper()
	canvas := image.NewRGBA(image.Rect(0, 0, 2, 2))
	canvas.Set(0, 0, color.RGBA{R: 255, A: 255})
	var out bytes.Buffer
	var err error
	switch mime {
	case "image/png":
		err = png.Encode(&out, canvas)
	case "image/jpeg":
		err = jpeg.Encode(&out, canvas, nil)
	default:
		t.Fatalf("unknown fixture media type: %s", mime)
	}
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(out.Bytes())
}

package web

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/config"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func editFrame(url string, progress any, extra map[string]any) map[string]any {
	frame := map[string]any{"imageUrl": url, "imageIndex": 0}
	if progress != nil {
		frame["progress"] = progress
	}
	for k, v := range extra {
		frame[k] = v
	}
	return map[string]any{"streamingImageGenerationResponse": frame}
}

// A grok.com edit stream sends one image several times while it sharpens.
// Only the finished frame is a result; taking two images used to return the
// finished image and a blurry 50% preview.
func TestImageEditKeepsOnlyTheFinishedImage(t *testing.T) {
	const (
		preview1 = "https://assets.grok.com/users/u1/generated/e1/preview-30.jpg"
		preview2 = "https://assets.grok.com/users/u1/generated/e1/preview-60.jpg"
		final    = "https://assets.grok.com/users/u1/generated/e1/image.jpg"
		echoed   = "https://assets.grok.com/users/u1/input-asset-1/content.jpg"
	)
	lines := []map[string]any{
		editFrame(preview1, 30, nil),
		editFrame(preview2, 60.0, nil),
		editFrame(final, 100, nil),
		{"modelResponse": map[string]any{"fileAttachments": []any{echoed}, "generatedImageUrls": []any{final}}},
	}
	var urls []string
	for _, line := range lines {
		urls = appendImageEditResultURLs(urls, line)
	}
	urls = normalizeGeneratedImageURLs(withoutInputAssets(urls, []string{"input-asset-1"}), 2)
	testutil.Equal(t, len(urls), 1)
	testutil.Equal(t, urls[0], final)
}

func TestImageEditPreviewFrame(t *testing.T) {
	testutil.True(t, imageEditPreviewFrame(editFrame("u", 50, nil)), "a 50% frame is a preview")
	testutil.False(t, imageEditPreviewFrame(editFrame("u", 100, nil)), "a 100% frame is the result")
	testutil.False(t, imageEditPreviewFrame(editFrame("u", 40, map[string]any{"isFinal": true})), "isFinal marks the result")
	testutil.True(t, imageEditPreviewFrame(editFrame("u", 100, map[string]any{"moderated": true})), "a moderated image is withheld")
	testutil.False(t, imageEditPreviewFrame(editFrame("u", nil, nil)), "a frame without progress is not assumed to be a preview")
	testutil.False(t, imageEditPreviewFrame(map[string]any{"modelResponse": map[string]any{"message": "x"}}), "other lines are not frames")
}

func TestWebImageEditEntrypoints(t *testing.T) {
	const model = "grok-imagine-image-edit"
	const asset = "https://assets.grok.com/users/test/generated/output/image.png"
	const image = "aW1hZ2U="
	const dataURL = "data:image/png;base64," + image
	st, err := store.New(store.Options{RedisAddr: miniredis.RunT(t).Addr()})
	testutil.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	testutil.NoError(t, st.CreateModel(t.Context(), &store.Model{
		Channel: "grok", ModelID: model, Provider: ProviderWeb, Status: store.ModelStatusAvailable, Verified: true,
		Capabilities: []string{store.CapabilityChat, store.CapabilityImageEdit},
	}))
	testutil.NoError(t, st.CreateAccount(t.Context(), webAccount(0, "super", true, true)))
	fixtureBytes, err := base64.StdEncoding.DecodeString(image)
	testutil.NoError(t, err)
	var calls []string
	transport := chatAssetTransport(func(r *http.Request) (*http.Response, error) {
		var output string
		calls = append(calls, r.URL.Path)
		switch r.URL.Path {
		case imagineUploadPath:
			testutil.NoError(t, r.ParseMultipartForm(1024))
			testutil.Equal(t, r.FormValue("file_source"), imagineSelfUploadSource)
			file, _, err := r.FormFile("file")
			testutil.NoError(t, err)
			got, err := io.ReadAll(file)
			_ = file.Close()
			testutil.NoError(t, err)
			testutil.Equal(t, string(got), string(fixtureBytes))
			output = `{"fileMetadata":{"fileMetadataId":"fixture-input"}}`
		case defaultChatPath:
			var payload struct {
				MediaGenInput struct {
					ImageToImage struct {
						Prompt, AspectRatio string
						InputAssets         []string
					}
				}
			}
			testutil.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
			edit := payload.MediaGenInput.ImageToImage
			testutil.Equal(t, edit.Prompt, "Retouch @fixture-input")
			testutil.Equal(t, edit.AspectRatio, "3:2")
			testutil.Equal(t, strings.Join(edit.InputAssets, ","), "fixture-input")
			output = `{"result":{"response":{"streamingImageGenerationResponse":{"imageUrl":"` + asset + `","progress":100}}}}` + "\n"
		case "/users/test/generated/output/image.png":
			output = string(fixtureBytes)
		default:
			return nil, fmt.Errorf("unexpected upstream path: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(output)), Header: make(http.Header)}, nil
	})
	client := &http.Client{Transport: transport}
	cfg := &config.Config{GrokStatsigSignerURL: new("")}
	h := &Handler{cfg: cfg, lb: loadbalancer.NewWithCacheTTL(st, 0), client: &Client{cfg: cfg, httpClient: client, assetClient: client}}
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	for key, value := range map[string]string{"prompt": "Retouch @IMAGE1", "size": "1536x1024", "response_format": "b64_json"} {
		testutil.NoError(t, writer.WriteField(key, value))
	}
	part, err := writer.CreateFormFile("image", "source.png")
	testutil.NoError(t, err)
	_, err = part.Write(fixtureBytes)
	testutil.NoError(t, err)
	testutil.NoError(t, writer.Close())
	for _, tc := range []struct {
		name, contentType, body string
		handle                  http.HandlerFunc
		stream                  bool
	}{
		{"chat", "application/json", `{"model":"` + model + `","stream":false,"image_config":{"size":"1536x1024","response_format":"base64"},"messages":[{"role":"user","content":[{"type":"text","text":"Retouch @IMAGE1"},{"type":"image_url","image_url":{"url":"` + dataURL + `"}}]}]}`, h.HandleChatCompletions, false},
		{"json edit", "application/json", `{"prompt":"Retouch @IMAGE1","aspect_ratio":"3:2","size":"1024x1536","response_format":"b64_json","stream":true,"image":{"url":"` + dataURL + `"}}`, h.HandleImagesEdits, true},
		{"multipart edit", writer.FormDataContentType(), form.String(), h.HandleImagesEdits, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls = calls[:0]
			r := httptest.NewRequest(http.MethodPost, "/grok/v1/test", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", tc.contentType)
			w := httptest.NewRecorder()
			tc.handle(w, r)
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"b64_json":"`+image+`"`) {
				t.Fatalf("status=%d body=%s calls=%v", w.Code, w.Body, calls)
			}
			if tc.stream && (!strings.Contains(w.Body.String(), "image_generation.completed") || !strings.Contains(w.Body.String(), "data: [DONE]")) {
				t.Fatalf("incomplete SSE response: %s", w.Body)
			}
			if len(calls) != 3 || calls[0] != imagineUploadPath || calls[1] != defaultChatPath {
				t.Fatalf("unexpected upstream request order: %v", calls)
			}
		})
	}
}

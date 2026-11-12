package grokbase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Video job helpers shared by the planes that create videos on an xAI video
// API (Build's CLI proxy and console.x.ai): the create/status response shapes
// and the credential-free download from xAI's CDN.

const (
	maxVideoStatusBytes = 2 << 20
	maxVideoAssetBytes  = 512 << 20
	videoMessageLimit   = 160
)

func ReadVideoBody(response *http.Response) ([]byte, error) {
	if response == nil || response.Body == nil {
		return nil, errors.New("xAI video returned an empty response")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxVideoStatusBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxVideoStatusBytes {
		return nil, errors.New("xAI video response exceeds 2 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// The body is not attached: it is upstream prose on a client-visible job.
		return nil, fmt.Errorf("xAI video upstream returned status %d", response.StatusCode)
	}
	return body, nil
}

func ParseVideoCreate(body []byte) (string, error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return "", errors.New("xAI video create response is not JSON")
	}
	if message := nestedString(root, "error", "message"); message != "" {
		return "", fmt.Errorf("xAI video creation failed: %s", boundVideoMessage(message))
	}
	if id := firstStringField(root, "request_id", "id"); id != "" {
		return id, nil
	}
	if data, ok := root["data"].(map[string]any); ok {
		if id := firstStringField(data, "request_id", "id"); id != "" {
			return id, nil
		}
	}
	return "", errors.New("xAI video create response has no request_id")
}

// ParseVideoStatus reads one poll: the video URL once the job is done, an
// error when the upstream reports failure, otherwise "keep polling".
func ParseVideoStatus(body []byte, progress func(int)) (videoURL string, done bool, err error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return "", false, errors.New("xAI video status response is not JSON")
	}
	if message := nestedString(root, "error", "message"); message != "" {
		return "", false, fmt.Errorf("xAI video generation failed: %s", boundVideoMessage(message))
	}
	source := root
	if data, ok := root["data"].(map[string]any); ok {
		source = data
	}
	if value, ok := source["progress"].(float64); ok && progress != nil {
		progress(int(value))
	}
	videoURL = videoURLFrom(root)
	switch strings.ToLower(firstStringField(source, "status", "state")) {
	case "failed", "error", "cancelled", "canceled":
		message := nestedString(source, "error", "message")
		if message == "" {
			message = firstStringField(source, "error_message", "message", "status", "state")
		}
		return "", false, fmt.Errorf("xAI video generation failed: %s", boundVideoMessage(message))
	case "completed", "succeeded", "success", "ready", "done":
		if videoURL == "" {
			return "", false, errors.New("xAI video completed without a content URL")
		}
		return videoURL, true, nil
	default:
		return videoURL, videoURL != "", nil
	}
}

func videoURLFrom(root map[string]any) string {
	candidates := []string{
		firstStringField(root, "video_url", "url"),
		nestedString(root, "video", "url"), nestedString(root, "output", "url"), nestedString(root, "result", "url"),
	}
	collect := func(items []any) {
		for _, item := range items {
			if entry, ok := item.(map[string]any); ok {
				candidates = append(candidates, firstStringField(entry, "url", "video_url"))
			}
		}
	}
	switch data := root["data"].(type) {
	case map[string]any:
		candidates = append(candidates, firstStringField(data, "video_url", "url"), nestedString(data, "video", "url"), nestedString(data, "output", "url"))
		if items, ok := data["data"].([]any); ok {
			collect(items)
		}
	case []any:
		collect(data)
	}
	for _, candidate := range candidates {
		if candidate != "" {
			return candidate
		}
	}
	return ""
}

func firstStringField(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, ok := values[key].(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func nestedString(values map[string]any, object, field string) string {
	nested, _ := values[object].(map[string]any)
	if nested == nil {
		return ""
	}
	return firstStringField(nested, field)
}

// boundVideoMessage keeps an upstream failure message short: it ends up on a
// job a client reads back.
func boundVideoMessage(message string) string {
	message = strings.TrimSpace(message)
	if len(message) <= videoMessageLimit {
		return message
	}
	return strings.ToValidUTF8(message[:videoMessageLimit], "")
}

// TrustedVideoURL accepts only https URLs on xAI's own asset hosts.
func TrustedVideoURL(rawURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Hostname() == "" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	for _, suffix := range []string{"x.ai", "grok.com"} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

// DownloadVideo fetches a finished video from xAI's CDN. The asset host
// needs no credential, so none is sent, and redirects must stay on xAI hosts.
func DownloadVideo(ctx context.Context, httpClient *http.Client, rawURL string) ([]byte, string, error) {
	if httpClient == nil || !TrustedVideoURL(rawURL) {
		return nil, "", errors.New("xAI video content URL is not trusted")
	}
	client := *httpClient
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if !TrustedVideoURL(request.URL.String()) {
			return errors.New("xAI video redirected to an untrusted URL")
		}
		if len(via) >= 10 {
			return errors.New("too many Build video redirects")
		}
		return nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	request.Header.Set("Accept", "video/*,*/*;q=0.8")
	response, err := client.Do(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, "", fmt.Errorf("xAI video download returned status %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxVideoAssetBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(raw) == 0 || len(raw) > maxVideoAssetBytes {
		return nil, "", errors.New("xAI video is empty or exceeds 512 MiB")
	}
	mimeType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]))
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = "video/mp4"
	}
	if !strings.HasPrefix(mimeType, "video/") {
		return nil, "", errors.New("xAI video has an invalid Content-Type")
	}
	return raw, mimeType, nil
}

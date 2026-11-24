package web

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"orchids-api/internal/responses"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"encoding/json"

	"orchids-api/internal/config"
	"orchids-api/internal/util"
)

var (
	reToolUsageCardBlock      = regexp.MustCompile(`(?is)<?xai:tool_usage_card[^>]*>.*?</xai:tool_usage_card>`)
	reToolUsageCardIncomplete = regexp.MustCompile(`(?is)<?xai:tool_usage_card.*?(?:</xai:tool_usage_card>|\z)`)
	reGrokRenderBlock         = regexp.MustCompile(`(?is)<?grok:render.*?</grok:render>`)

	renderableImageExtensions = []string{".png", ".jpg", ".jpeg", ".webp", ".gif"}
	videoAspectRatioMap       = map[string]string{
		"1280x720":  "16:9",
		"720x1280":  "9:16",
		"1792x1024": "3:2",
		"1024x1792": "2:3",
		"1024x1024": "1:1",
		"16:9":      "16:9",
		"9:16":      "9:16",
		"3:2":       "3:2",
		"2:3":       "2:3",
		"4:3":       "4:3",
		"3:4":       "3:4",
		"1:1":       "1:1",
	}
)

func parseUpstreamLines(body io.Reader, onLine func(map[string]any) error) error {
	decoder := json.NewDecoder(body)

	for {
		var line map[string]any
		if err := decoder.Decode(&line); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if message := upstreamStreamErrorMessage(line); message != "" {
			return fmt.Errorf("grok upstream stream error: %s", message)
		}
		result, _ := line["result"].(map[string]any)
		if message := upstreamStreamErrorMessage(result); message != "" {
			return fmt.Errorf("grok upstream stream error: %s", message)
		}
		resp, _ := result["response"].(map[string]any)
		if resp == nil {
			continue
		}
		if err := onLine(resp); err != nil {
			return err
		}
	}
}

func upstreamStreamErrorMessage(value map[string]any) string {
	if value == nil {
		return ""
	}
	if raw, exists := value["error"]; exists {
		if message := upstreamErrorValueMessage(raw); message != "" {
			return message
		}
		return "upstream returned an unspecified error"
	}
	// Current Grok streams can also carry failures as an event envelope rather
	// than a top-level error.  Do not silently turn that into a successful empty
	// completion just because app-chat normally uses result.response envelopes.
	event, _ := value["event"].(map[string]any)
	if !strings.EqualFold(strings.TrimSpace(fmt.Sprint(event["type"])), "error") {
		return ""
	}
	if raw, exists := event["error"]; exists {
		if message := upstreamErrorValueMessage(raw); message != "" {
			return message
		}
		return "upstream returned an unspecified error"
	}
	if message := upstreamErrorValueMessage(event); message != "" {
		return message
	}
	return "upstream returned an unspecified error"
}

func upstreamErrorValueMessage(raw any) string {
	if raw == nil {
		return ""
	}
	message := strings.TrimSpace(fmt.Sprint(raw))
	if details, ok := raw.(map[string]any); ok {
		message = firstNonEmpty(
			strings.TrimSpace(fmt.Sprint(details["message"])),
			strings.TrimSpace(fmt.Sprint(details["error"])),
			strings.TrimSpace(fmt.Sprint(details["code"])),
		)
	}
	if message == "" || message == "<nil>" {
		return ""
	}
	if len(message) > 512 {
		message = message[:512]
	}
	return message
}

func parseGrokJSONData(v any) any {
	switch x := v.(type) {
	case map[string]any, []any:
		return x
	case string:
		raw := strings.TrimSpace(x)
		if raw == "" {
			return nil
		}
		var parsed any
		if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
			return parsed
		}
		return x
	default:
		return x
	}
}

func parseGrokJSONText(raw string) any {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
	if !strings.HasPrefix(s, "{") && !strings.HasPrefix(s, "[") {
		return nil
	}
	var parsed any
	if err := json.Unmarshal([]byte(s), &parsed); err != nil {
		return nil
	}
	return parsed
}

func normalizeGrokAssetURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" || s == "<nil>" {
		return ""
	}
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return s
	}
	if strings.HasPrefix(s, "/") {
		return defaultAssetsBaseURL + s
	}
	if strings.HasPrefix(lower, "users/") || strings.HasPrefix(lower, "generated/") || strings.Contains(lower, "/generated/") || strings.Contains(lower, "/image/") {
		return defaultAssetsBaseURL + "/" + strings.TrimLeft(s, "/")
	}
	return s
}

// firstNonEmpty delegates to the shared implementation in internal/util so the
// package keeps its short local name without duplicating the logic.
func firstNonEmpty(values ...string) string {
	return util.FirstNonEmpty(values...)
}

// NormalizeSSOToken extracts the raw SSO token from a cookie-like string.
func NormalizeSSOToken(raw string) string {
	token := strings.TrimSpace(raw)
	if token == "" {
		return ""
	}

	// Cookie-style input: scan pairs and prefer exact "sso" key.
	if strings.Contains(token, ";") {
		parts := strings.SplitSeq(token, ";")
		for part := range parts {
			kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
			if len(kv) != 2 {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(kv[0]), "sso") {
				return strings.TrimSpace(kv[1])
			}
		}
		return strings.TrimSpace(token)
	}

	// Plain "sso=<token>" input.
	lower := strings.ToLower(strings.TrimSpace(token))
	if strings.HasPrefix(lower, "sso=") {
		return strings.TrimSpace(token[len("sso="):])
	}
	return strings.TrimSpace(token)
}

func dataURIFromBytes(mime string, data []byte) string {
	mime = strings.TrimSpace(mime)
	if mime == "" {
		mime = "application/octet-stream"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func parseDataURI(input string) (fileName, contentBase64, mime string, err error) {
	s := strings.TrimSpace(input)
	if !strings.HasPrefix(strings.ToLower(s), "data:") {
		return "", "", "", errors.New("not a data uri")
	}
	idx := strings.Index(s, ",")
	if idx <= 0 {
		return "", "", "", errors.New("invalid data uri")
	}
	header := s[5:idx]
	payload := strings.TrimSpace(s[idx+1:])
	if !strings.Contains(strings.ToLower(header), ";base64") {
		return "", "", "", errors.New("data uri is not base64 encoded")
	}
	mime = strings.TrimSpace(strings.Split(header, ";")[0])
	if mime == "" {
		mime = "application/octet-stream"
	}
	ext := "bin"
	if slash := strings.Index(mime, "/"); slash >= 0 && slash+1 < len(mime) {
		ext = strings.TrimSpace(mime[slash+1:])
	}
	return "file." + ext, payload, mime, nil
}

func isRemoteURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil {
		return false
	}
	scheme := strings.ToLower(strings.TrimSpace(u.Scheme))
	return scheme == "http" || scheme == "https"
}

func fetchRemoteAsDataURI(ctx context.Context, rawURL string, timeout time.Duration, proxyFunc func(*http.Request) (*url.URL, error), maxBytes int64) (string, error) {
	data, mime, err := fetchRemoteAttachment(ctx, rawURL, timeout, proxyFunc, maxBytes, "image/*, video/*;q=0.9, */*;q=0.8")
	if err != nil {
		return "", err
	}
	return dataURIFromBytes(mime, data), nil
}

func fetchRemoteAttachment(ctx context.Context, rawURL string, timeout time.Duration, proxyFunc func(*http.Request) (*url.URL, error), maxBytes int64, accept string) ([]byte, string, error) {
	u := strings.TrimSpace(rawURL)
	if u == "" {
		return nil, "", fmt.Errorf("empty url")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	data, contentType, err := fetchRemoteMedia(ctx, u, timeout, proxyFunc, maxBytes, accept, false)
	if err != nil {
		if errors.Is(err, errRemoteMediaTooLarge) {
			err = fmt.Errorf("remote attachment exceeds the size limit")
		}
		return nil, "", err
	}
	mime, _, _ := strings.Cut(contentType, ";")
	if mime = strings.TrimSpace(mime); mime == "" {
		mime = mimeFromFilename(u)
	}
	return data, mime, err
}

var errRemoteMediaTooLarge = errors.New("remote media exceeds size limit")

// Fetch only after validating the target, then keep the same guard on every
// redirect. Staging requires a full 200; chat attachments accept all 2xx replies.
func fetchRemoteMedia(ctx context.Context, rawURL string, timeout time.Duration, proxy func(*http.Request) (*url.URL, error), maxBytes int64, accept string, staging bool) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	target, err := checkRemoteFetchTarget(ctx, rawURL, proxy != nil)
	if err != nil {
		if staging {
			err = fmt.Errorf("%w: %v", errAdminMediaBlocked, err)
		}
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", config.DefaultGrokWebUserAgent)
	req.Header.Set("Accept", accept)
	resp, err := newRemoteFetchClient(timeout, proxy).Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if staging && resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("remote server returned HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("fetch url status=%d", resp.StatusCode)
	}
	if resp.ContentLength > maxBytes {
		return nil, "", errRemoteMediaTooLarge
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(raw)) > maxBytes {
		return nil, "", errRemoteMediaTooLarge
	}
	return raw, resp.Header.Get("Content-Type"), nil
}

func mimeFromFilename(name string) string {
	ext := strings.ToLower(path.Ext(strings.TrimSpace(name)))
	switch ext {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".pdf":
		return "application/pdf"
	case ".txt":
		return "text/plain"
	case ".md":
		return "text/markdown"
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	default:
		return "application/octet-stream"
	}
}

func uniqueStrings(input []string) []string {
	return util.UniqueStrings(input)
}

func parseBoolLoose(raw string, fallback bool) bool {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch s {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return fallback
	}
}

func parseIntLoose(raw string, fallback int) int {
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return fallback
	}
	return v
}

func diagnosticValueSummary(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return truncateDiagnosticText(x, 220)
	case []any:
		if len(x) == 0 {
			return "[]"
		}
		return fmt.Sprintf("len=%d first=%s", len(x), truncateDiagnosticText(fmt.Sprint(x[0]), 180))
	case map[string]any:
		if len(x) == 0 {
			return "{}"
		}
		b, _ := json.Marshal(x)
		return truncateDiagnosticText(string(b), 220)
	default:
		return truncateDiagnosticText(fmt.Sprint(x), 220)
	}
}

func truncateDiagnosticText(s string, max int) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max]) + "..."
}

// Paths are constant provider field names joined by dots, not user input.
func valueAtPath(root any, path string) any {
	for key := range strings.SplitSeq(path, ".") {
		object, ok := root.(map[string]any)
		if !ok {
			return nil
		}
		root = object[key]
	}
	return root
}

func mapAtAnyPath(root any, paths ...string) map[string]any {
	for _, path := range paths {
		if m, ok := valueAtPath(root, path).(map[string]any); ok && len(m) > 0 {
			return m
		}
	}
	return nil
}

func stringAtAnyPath(root any, paths ...string) string {
	for _, path := range paths {
		v := strings.TrimSpace(fmt.Sprint(valueAtPath(root, path)))
		if v != "" && v != "<nil>" {
			return v
		}
	}
	return ""
}

func rawStringAtAnyPath(root any, paths ...string) string {
	for _, path := range paths {
		raw := valueAtPath(root, path)
		if text, ok := raw.(string); ok {
			if text != "" {
				return text
			}
			continue
		}
		value := strings.TrimSpace(fmt.Sprint(raw))
		if value != "" && value != "<nil>" {
			return value
		}
	}
	return ""
}

func intAtAnyPath(root any, paths ...string) int {
	for _, path := range paths {
		if v := responses.InterfaceToInt(valueAtPath(root, path)); v != 0 {
			return v
		}
	}
	return 0
}

package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	grokJSONEmptyObjectBytes  = []byte("{}")
	reToolUsageCardBlock      = regexp.MustCompile(`(?is)<?xai:tool_usage_card[^>]*>.*?</xai:tool_usage_card>`)
	reToolUsageCardIncomplete = regexp.MustCompile(`(?is)<?xai:tool_usage_card.*?(?:</xai:tool_usage_card>|\z)`)
	reGrokRenderBlock         = regexp.MustCompile(`(?is)<?grok:render.*?</grok:render>`)

	rateLimitFamilies = []rateLimitFieldFamily{
		{
			unit: "tokens",
			limitKeys: []string{
				"limit_tokens",
				"limittokens",
				"max_tokens",
				"maxtokens",
				"token_limit",
				"tokenlimit",
				"tokens_limit",
				"tokenslimit",
				"total_tokens",
				"totaltokens",
			},
			remainingKeys: []string{
				"remaining_tokens",
				"remainingtokens",
				"tokens_remaining",
				"tokensremaining",
			},
		},
		{
			unit: "requests",
			limitKeys: []string{
				"max_queries",
				"maxqueries",
				"query_limit",
				"querylimit",
				"queries_limit",
				"querieslimit",
				"total_queries",
				"totalqueries",
				"request_limit",
				"requestlimit",
				"requests_limit",
				"requestslimit",
			},
			remainingKeys: []string{
				"remaining_queries",
				"remainingqueries",
				"queries_remaining",
				"queriesremaining",
				"remaining_requests",
				"remainingrequests",
			},
		},
		{
			unit: "",
			limitKeys: []string{
				"limit",
				"quota",
				"quota_limit",
				"quotalimit",
			},
			remainingKeys: []string{
				"remaining",
				"quota_remaining",
				"quotaremaining",
			},
		},
	}
	rateLimitNumericKeys = buildRateLimitNumericKeySet(rateLimitFamilies)
	rateLimitResetKeys   = map[string]struct{}{
		"reset":           {},
		"reset_at":        {},
		"resetat":         {},
		"reset_at_ms":     {},
		"resetatms":       {},
		"reset_time":      {},
		"resettime":       {},
		"reset_timestamp": {},
		"resettimestamp":  {},
		"next_reset":      {},
		"nextreset":       {},
	}
	renderableImageExtensions = []string{".png", ".jpg", ".jpeg", ".webp", ".gif"}
	allowedMessageRoles       = map[string]struct{}{
		"developer": {},
		"system":    {},
		"user":      {},
		"assistant": {},
		"tool":      {},
	}
	userContentTypes = map[string]struct{}{
		"text":        {},
		"image_url":   {},
		"input_audio": {},
		"file":        {},
		// The Anthropic Messages front end lowers its blocks onto this same
		// validator, so the Responses-shaped parts it produces are valid here
		// too. Without them a document or a multi-part tool_result is rejected
		// before the request ever reaches the upstream.
		"input_text":  {},
		"input_image": {},
		"input_file":  {},
	}
	videoAspectRatioMap = map[string]string{
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

func randomHex(n int) string {
	if n <= 0 {
		return ""
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return hex.EncodeToString(buf)
}

func randomUUID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		buf[0:4],
		buf[4:6],
		buf[6:8],
		buf[8:10],
		buf[10:16],
	)
}

func parseUpstreamLines(body io.Reader, onLine func(map[string]interface{}) error) error {
	decoder := json.NewDecoder(body)

	for {
		var line map[string]interface{}
		if err := decoder.Decode(&line); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if message := upstreamStreamErrorMessage(line); message != "" {
			return fmt.Errorf("grok upstream stream error: %s", message)
		}
		result, _ := line["result"].(map[string]interface{})
		if message := upstreamStreamErrorMessage(result); message != "" {
			return fmt.Errorf("grok upstream stream error: %s", message)
		}
		resp, _ := result["response"].(map[string]interface{})
		if resp == nil {
			continue
		}
		if err := onLine(resp); err != nil {
			return err
		}
	}
}

func upstreamStreamErrorMessage(value map[string]interface{}) string {
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
	event, _ := value["event"].(map[string]interface{})
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

func upstreamErrorValueMessage(raw interface{}) string {
	if raw == nil {
		return ""
	}
	message := strings.TrimSpace(fmt.Sprint(raw))
	if details, ok := raw.(map[string]interface{}); ok {
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

// walkJSONStrings visits every string leaf in a nested JSON value (maps,
// slices, or plain strings).
func walkJSONStrings(value interface{}, visit func(string)) {
	switch x := value.(type) {
	case map[string]interface{}:
		for _, item := range x {
			walkJSONStrings(item, visit)
		}
	case []interface{}:
		for _, item := range x {
			walkJSONStrings(item, visit)
		}
	case string:
		visit(x)
	}
}

func parseGrokJSONData(v interface{}) interface{} {
	switch x := v.(type) {
	case map[string]interface{}, []interface{}:
		return x
	case string:
		raw := strings.TrimSpace(x)
		if raw == "" {
			return nil
		}
		var parsed interface{}
		if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
			return parsed
		}
		return x
	default:
		return x
	}
}

func parseGrokJSONText(raw string) interface{} {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
	if !strings.HasPrefix(s, "{") && !strings.HasPrefix(s, "[") {
		return nil
	}
	var parsed interface{}
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
		parts := strings.Split(token, ";")
		for _, part := range parts {
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

func isDigit(c byte) bool {
	return c >= '0' && c <= '9'
}

func encodeJSONBytes(v interface{}) []byte {
	buf := bytes.Buffer{}
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return grokJSONEmptyObjectBytes
	}
	raw := buf.Bytes()
	if n := len(raw); n > 0 && raw[n-1] == '\n' {
		return raw[:n-1]
	}
	return raw
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

func fetchRemoteAsDataURI(rawURL string, timeout time.Duration, proxyFunc func(*http.Request) (*url.URL, error)) (string, error) {
	u := strings.TrimSpace(rawURL)
	if u == "" {
		return "", fmt.Errorf("empty url")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	// The URL comes from the caller's message payload, so it is untrusted: the
	// target must resolve to a public address before the gateway dials it.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	target, err := checkRemoteFetchTarget(ctx, u, proxyFunc != nil)
	if err != nil {
		return "", err
	}
	client := newRemoteFetchClient(timeout, proxyFunc)
	req, err := http.NewRequest(http.MethodGet, target.String(), nil)
	if err != nil {
		return "", err
	}
	// Many image hosts and CDNs refuse Go's default client identity.
	req.Header.Set("User-Agent", config.DefaultGrokWebUserAgent)
	req.Header.Set("Accept", "image/*, video/*;q=0.9, */*;q=0.8")
	req = req.WithContext(ctx)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("fetch url status=%d body=%s", resp.StatusCode, string(body))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 60*1024*1024))
	if err != nil {
		return "", err
	}
	mime := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	if mime == "" {
		mime = mimeFromFilename(u)
	}
	return dataURIFromBytes(mime, data), nil
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

func diagnosticValueSummary(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return truncateDiagnosticText(x, 220)
	case []interface{}:
		if len(x) == 0 {
			return "[]"
		}
		return fmt.Sprintf("len=%d first=%s", len(x), truncateDiagnosticText(fmt.Sprint(x[0]), 180))
	case map[string]interface{}:
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

func interfaceToInt(v interface{}) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return int(i)
		}
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(x)); err == nil {
			return i
		}
	}
	return 0
}

func valueAtPath(root interface{}, path ...string) interface{} {
	cur := root
	for _, key := range path {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur, ok = m[key]
		if !ok {
			return nil
		}
	}
	return cur
}

func mapAtAnyPath(root interface{}, paths ...[]string) map[string]interface{} {
	for _, path := range paths {
		if len(path) == 0 {
			continue
		}
		if m, ok := valueAtPath(root, path...).(map[string]interface{}); ok && len(m) > 0 {
			return m
		}
	}
	return nil
}

func stringAtAnyPath(root interface{}, paths ...[]string) string {
	for _, path := range paths {
		if len(path) == 0 {
			continue
		}
		v := strings.TrimSpace(fmt.Sprint(valueAtPath(root, path...)))
		if v != "" && v != "<nil>" {
			return v
		}
	}
	return ""
}

func rawStringAtAnyPath(root interface{}, paths ...[]string) string {
	for _, path := range paths {
		if len(path) == 0 {
			continue
		}
		raw := valueAtPath(root, path...)
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

func intAtAnyPath(root interface{}, paths ...[]string) int {
	for _, path := range paths {
		if len(path) == 0 {
			continue
		}
		if v := interfaceToInt(valueAtPath(root, path...)); v != 0 {
			return v
		}
	}
	return 0
}

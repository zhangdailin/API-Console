package web

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"orchids-api/internal/grok/ratelimit"
	"strconv"
	"strings"
	"time"
)

// GetWebQuota reads the Web SSO quota products using the upstream's stable
// mode names. Web rate-limits are keyed by "auto" and "fast", not by the
// public model ID (for example, grok-4.5). A partial result is returned when
// one mode is unavailable; callers can still display the successful window.
func (c *Client) GetWebQuota(ctx context.Context, token string) (map[string]*RateLimitInfo, error) {
	token = strings.TrimSpace(token)
	if NormalizeSSOToken(token) == "" {
		return nil, fmt.Errorf("empty token")
	}
	windows := make(map[string]*RateLimitInfo, 2)
	var errs []string
	for _, mode := range []string{"auto", "fast"} {
		info, err := c.getUsageByMode(ctx, token, mode)
		if err != nil {
			if IsAuthenticationFailure(err) {
				return nil, err
			}
			errs = append(errs, mode+": "+err.Error())
			continue
		}
		if info != nil {
			windows[mode] = info
		}
	}
	if len(windows) > 0 {
		return windows, nil
	}
	if len(errs) == 0 {
		return nil, fmt.Errorf("grok web quota unavailable")
	}
	return nil, fmt.Errorf("grok web quota unavailable: %s", strings.Join(errs, "; "))
}

func (c *Client) getUsageByMode(ctx context.Context, token, mode string) (*RateLimitInfo, error) {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		return nil, fmt.Errorf("empty quota mode")
	}
	return c.getUsageBySpec(ctx, token, ModelSpec{ModeID: mode})
}

func (c *Client) getUsageBySpec(ctx context.Context, token string, spec ModelSpec) (*RateLimitInfo, error) {
	payload := map[string]any{
		"requestKind": "DEFAULT",
		"modelName":   rateLimitModelName(spec),
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	reqURL := c.baseURL() + defaultRateLimitsPath
	resp, err := c.doRequest(ctx, reqURL, http.MethodPost, raw, c.headersFor(ctx, token, http.MethodPost, reqURL), false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var payloadResp map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payloadResp); err != nil {
		return nil, err
	}
	if info := parseRateLimitPayload(payloadResp); info != nil {
		return info, nil
	}
	return ratelimit.ParseHeaders(resp.Header), nil
}

func rateLimitModelName(spec ModelSpec) string {
	return resolveModeID(spec, "fast")
}

func (c *Client) uploadFile(ctx context.Context, token, fileName, fileMimeType, contentBase64 string) (string, string, error) {
	payload := map[string]string{
		"fileName":     strings.TrimSpace(fileName),
		"fileMimeType": strings.TrimSpace(fileMimeType),
		"content":      strings.TrimSpace(contentBase64),
	}
	if payload["fileName"] == "" {
		payload["fileName"] = "file.bin"
	}
	if payload["fileMimeType"] == "" {
		payload["fileMimeType"] = "application/octet-stream"
	}
	if payload["content"] == "" {
		return "", "", fmt.Errorf("empty upload content")
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return "", "", err
	}

	reqURL := c.baseURL() + defaultUploadFilePath
	headers := c.headersFor(ctx, token, http.MethodPost, reqURL)
	headers.Set("Referer", "https://grok.com/files")
	resp, err := c.doRequest(ctx, reqURL, http.MethodPost, raw, headers, false)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	var out struct {
		FileMetadataID string `json:"fileMetadataId"`
		FileURI        string `json:"fileUri"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", err
	}
	if strings.TrimSpace(out.FileMetadataID) == "" && strings.TrimSpace(out.FileURI) == "" {
		return "", "", fmt.Errorf("grok upload returned empty identifiers")
	}
	return strings.TrimSpace(out.FileMetadataID), strings.TrimSpace(out.FileURI), nil
}

func (c *Client) downloadAsset(ctx context.Context, token, rawURL string) ([]byte, string, error) {
	link := strings.TrimSpace(rawURL)
	if link == "" {
		return nil, "", fmt.Errorf("empty asset url")
	}
	if !strings.HasPrefix(strings.ToLower(link), "http://") && !strings.HasPrefix(strings.ToLower(link), "https://") {
		if !strings.HasPrefix(link, "/") {
			link = "/" + link
		}
		link = defaultAssetsBaseURL + link
	}

	var resp *http.Response
	var err error
	if c.shouldSendAuthForAssetURL(link) {
		resp, err = c.doRequest(ctx, link, http.MethodGet, nil, c.assetDownloadHeaders(token, link), true)
	} else {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
		if reqErr != nil {
			return nil, "", reqErr
		}
		req.Header = c.assetDownloadHeaders(token, link)
		resp, err = c.clientForAsset(true).Do(req)
		if err == nil && resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			return nil, "", fmt.Errorf("asset download status=%d body=%s", resp.StatusCode, string(raw))
		}
	}
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	mime := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	return data, mime, nil
}

func (c *Client) getVoiceToken(ctx context.Context, token, voice, personality string, speed float64, instruction string) (map[string]any, error) {
	if strings.TrimSpace(voice) == "" {
		voice = "ara"
	}
	if strings.TrimSpace(personality) == "" {
		personality = "assistant"
	}
	if speed <= 0 {
		speed = 1.0
	}

	session := map[string]any{
		"voice":          strings.TrimSpace(voice),
		"personality":    strings.TrimSpace(personality),
		"playback_speed": speed,
		"enable_vision":  false,
		"turn_detection": map[string]any{"type": "server_vad"},
	}
	_ = instruction

	sessionPayload, err := json.Marshal(session)
	if err != nil {
		return nil, err
	}

	payload := map[string]any{
		"sessionPayload":       string(sessionPayload),
		"requestAgentDispatch": false,
		"livekitUrl":           "wss://livekit.grok.com",
		"params": map[string]any{
			"enable_markdown_transcript": "true",
		},
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	reqURL := c.baseURL() + defaultLivekitPath
	resp, err := c.doRequest(ctx, reqURL, http.MethodPost, raw, c.headersFor(ctx, token, http.MethodPost, reqURL), false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out := map[string]any{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func grpcWebEncode(payload []byte) []byte {
	out := make([]byte, 5+len(payload))
	out[0] = 0x00
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

func parseGRPCStatus(headers http.Header, body []byte) (int, string) {
	if headers != nil {
		rawStatus := strings.TrimSpace(ratelimit.FirstHeader(headers, "grpc-status", "Grpc-Status"))
		if rawStatus != "" {
			if code, err := strconv.Atoi(rawStatus); err == nil {
				return code, strings.TrimSpace(ratelimit.FirstHeader(headers, "grpc-message", "Grpc-Message"))
			}
		}
	}

	buf := body
	for len(buf) >= 5 {
		flag := buf[0]
		frameLen := int(binary.BigEndian.Uint32(buf[1:5]))
		buf = buf[5:]
		if frameLen < 0 || frameLen > len(buf) {
			break
		}
		frame := buf[:frameLen]
		buf = buf[frameLen:]

		// trailer frame
		if flag&0x80 == 0 {
			continue
		}
		lines := strings.Split(string(frame), "\r\n")
		code := -1
		msg := ""
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			kv := strings.SplitN(line, ":", 2)
			if len(kv) != 2 {
				continue
			}
			k := strings.ToLower(strings.TrimSpace(kv[0]))
			v := strings.TrimSpace(kv[1])
			switch k {
			case "grpc-status":
				if n, err := strconv.Atoi(v); err == nil {
					code = n
				}
			case "grpc-message":
				msg = v
			}
		}
		if code >= 0 {
			return code, msg
		}
	}

	// If grpc-status is absent, treat as success by default.
	return -1, ""
}

func (c *Client) grpcWebHeaders(token, origin, referer string) http.Header {
	headers := c.headers(token)
	headers.Set("Origin", origin)
	headers.Set("Referer", referer)
	headers.Set("Content-Type", "application/grpc-web+proto")
	headers.Set("Accept", "*/*")
	headers.Set("Sec-Fetch-Dest", "empty")
	headers.Set("x-grpc-web", "1")
	headers.Set("x-user-agent", "connect-es/2.1.1")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Pragma", "no-cache")
	return headers
}

// readGRPCStatus reads a bounded grpc-web response body and returns the HTTP
// status, grpc status/message, and a descriptive error when the grpc status is
// non-zero.
func readGRPCStatus(resp *http.Response, action string) (int, int, string, error) {
	statusCode := resp.StatusCode
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	grpcStatus, grpcMsg := parseGRPCStatus(resp.Header, raw)
	if grpcStatus != -1 && grpcStatus != 0 {
		if grpcMsg == "" {
			return statusCode, grpcStatus, grpcMsg, fmt.Errorf("%s failed grpc_status=%d", action, grpcStatus)
		}
		return statusCode, grpcStatus, grpcMsg, fmt.Errorf("%s failed grpc_status=%d message=%s", action, grpcStatus, grpcMsg)
	}
	return statusCode, grpcStatus, grpcMsg, nil
}

func (c *Client) acceptTOS(ctx context.Context, token string) (int, error) {
	headers := c.grpcWebHeaders(token, "https://accounts.x.ai", "https://accounts.x.ai/accept-tos")

	payload := grpcWebEncode([]byte{0x10, 0x01})
	resp, err := c.doRequest(ctx, defaultAcceptTOSURL, http.MethodPost, payload, headers, false)
	if err != nil {
		return parseUpstreamStatus(err), err
	}
	defer resp.Body.Close()

	statusCode, _, _, err := readGRPCStatus(resp, "accept tos")
	return statusCode, err
}

func randomAdultBirthDate() string {
	now := time.Now().UTC()
	year := now.Year() - (20 + rand.Intn(29)) // [20,48]
	month := 1 + rand.Intn(12)
	day := 1 + rand.Intn(28)
	hour := rand.Intn(24)
	minute := rand.Intn(60)
	second := rand.Intn(60)
	millis := rand.Intn(1000)
	return fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02d.%03dZ", year, month, day, hour, minute, second, millis)
}

func (c *Client) setBirthDate(ctx context.Context, token string) (int, error) {
	payload := map[string]string{
		"birthDate": randomAdultBirthDate(),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}

	headers := c.headers(token)
	headers.Set("Origin", "https://grok.com")
	headers.Set("Referer", "https://grok.com/?_s=home")
	headers.Set("Content-Type", "application/json")

	reqURL := c.baseURL() + defaultSetBirthPath
	resp, err := c.doRequest(ctx, reqURL, http.MethodPost, raw, headers, false)
	if err != nil {
		statusCode := parseUpstreamStatus(err)
		// Upstream may return 204 No Content when already set.
		if statusCode == http.StatusNoContent {
			return statusCode, nil
		}
		return statusCode, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	return resp.StatusCode, nil
}

func (c *Client) enableNSFWFeature(ctx context.Context, token string) (int, int, string, error) {
	headers := c.grpcWebHeaders(token, "https://grok.com", "https://grok.com/?_s=data")

	name := []byte("always_show_nsfw_content")
	inner := append([]byte{0x0a, byte(len(name))}, name...)
	protobuf := append([]byte{0x0a, 0x02, 0x10, 0x01, 0x12, byte(len(inner))}, inner...)
	payload := grpcWebEncode(protobuf)

	reqURL := c.baseURL() + defaultNSFWMgmtPath
	resp, err := c.doRequest(ctx, reqURL, http.MethodPost, payload, headers, false)
	if err != nil {
		return parseUpstreamStatus(err), -1, "", err
	}
	defer resp.Body.Close()

	return readGRPCStatus(resp, "enable nsfw")
}

func (c *Client) EnableNSFWDetailed(ctx context.Context, token string) NSFWEnableResult {
	result := NSFWEnableResult{
		Success:    false,
		HTTPStatus: 0,
		GRPCStatus: -1,
	}
	if strings.TrimSpace(token) == "" {
		result.Error = "empty token"
		return result
	}

	statusCode, err := c.acceptTOS(ctx, token)
	if statusCode > 0 {
		result.HTTPStatus = statusCode
	}
	if err != nil {
		result.Error = fmt.Sprintf("Accept ToS failed: %v", err)
		return result
	}

	statusCode, err = c.setBirthDate(ctx, token)
	if statusCode > 0 {
		result.HTTPStatus = statusCode
	}
	if err != nil {
		result.Error = fmt.Sprintf("Set birth date failed: %v", err)
		return result
	}

	statusCode, grpcStatus, grpcMsg, err := c.enableNSFWFeature(ctx, token)
	if statusCode > 0 {
		result.HTTPStatus = statusCode
	}
	result.GRPCStatus = grpcStatus
	result.GRPCMessage = grpcMsg
	if err != nil {
		result.Error = fmt.Sprintf("NSFW enable failed: %v", err)
		return result
	}

	result.Success = true
	result.Error = ""
	return result
}

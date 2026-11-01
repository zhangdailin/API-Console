package qoder

// The second inference route the desktop client knows.
//
// Besides the COSY-signed SSE route this channel implements, the client can send
// the same turn to an OpenAI-compatible endpoint on a separate model server:
//
//	https://{QODER_MODEL_SERVER_HOST:-api2-v2.qoder.sh}/model/v1/chat/completions
//	Authorization: Bearer {security_oauth_token || access_token}
//	operation: sendModelServerHttpChat   (the client may also use gRPC there)
//
// It is a different surface — bearer instead of COSY, an OpenAI-shaped body
// instead of the encoded one — so it is implemented as its own transport rather
// than as a variant of the SSE path. Nothing routes through it yet: the request
// body is built by the client's model-server serializer, and mapping this
// channel's request onto it is a separate change.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"orchids-api/internal/upstream"
)

const (
	// DefaultModelServerHost is the model server the client uses in production.
	DefaultModelServerHost = "api2-v2.qoder.sh"
	// envModelServerHost overrides it, as it does for the client.
	envModelServerHost = "QODER_MODEL_SERVER_HOST"
	modelServerPath    = "/model/v1/chat/completions"
)

// ModelServerChatURL is the OpenAI-compatible endpoint for this deployment.
func ModelServerChatURL() string {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv(envModelServerHost)), "/")
	if base == "" {
		base = "https://" + DefaultModelServerHost
	}
	// An override may carry its own scheme; a bare host is served over TLS.
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "https://" + base
	}
	return base + modelServerPath
}

// modelServerBearer is the credential this route accepts: the OAuth token when
// one is recorded, otherwise the account's access token.
func (c *Client) modelServerBearer() string {
	return strings.TrimSpace(c.currentCredentials().AccessToken)
}

// ModelServerResult reports what one model-server call produced.
type ModelServerResult struct {
	Status    int
	Events    int
	TextBytes int
}

// SendModelServerChat posts one OpenAI-shaped body to the model server and emits
// the answer as the same delta messages the SSE path emits, so a caller can use
// it without knowing which route answered.
func (c *Client) SendModelServerChat(ctx context.Context, payload []byte, onMessage func(upstream.SSEMessage)) (ModelServerResult, error) {
	result := ModelServerResult{}
	bearer := c.modelServerBearer()
	if bearer == "" {
		return result, fmt.Errorf("qoder model server needs an access token")
	}
	if len(bytes.TrimSpace(payload)) == 0 {
		return result, fmt.Errorf("qoder model server needs a request body")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ModelServerChatURL(), bytes.NewReader(payload))
	if err != nil {
		return result, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	client := c.stream
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return result, err
	}
	defer func() { _ = resp.Body.Close() }()
	result.Status = resp.StatusCode
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return result, fmt.Errorf("qoder model server: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	var sawText bool
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		result.Events++
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content == "" {
				continue
			}
			upstream.EmitTextDelta(onMessage, choice.Delta.Content, &sawText)
			result.TextBytes += len(choice.Delta.Content)
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		return result, err
	}
	return result, ctx.Err()
}

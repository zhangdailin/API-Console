package qoder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// The reference request profile reproduces, field for field, the chat request
// the reference bridge (github.com/EchoPing07/Qoder-2API-Go) sends: the Qoder
// CLI identity, its encrypted COSY identity layout, its header set and its body
// shape. The default profile is the QoderWork client captured on 2026-09-29.
//
// The profile exists because the gateway admits the two identities to
// different queues, and because the COSY identity layout changed on
// 2026-10-19 from the reference one (which carries user_type) to a layout
// without it; which of those the gateway reads has not been isolated. It is
// selected with qoder_request_profile = "reference", so it can be tried and
// reverted with a configuration change.
const (
	// RequestProfileQoderWork is the default QoderWork request.
	RequestProfileQoderWork = "qoderwork"
	// RequestProfileReference is the reference bridge's CLI request.
	RequestProfileReference = "reference"

	referenceCosyVersion = "0.1.43"
	referenceClientType  = "5"
	referenceUserAgent   = "Go-http-client/2.0"
	referenceSessionType = "qodercli"
)

// resolveRequestProfile maps the configured profile onto a known one. An
// unknown value keeps the default rather than inventing a request shape.
func resolveRequestProfile(value string) string {
	if strings.EqualFold(strings.TrimSpace(value), RequestProfileReference) {
		return RequestProfileReference
	}
	return RequestProfileQoderWork
}

func (c *Client) referenceProfile() bool {
	return c != nil && c.profile == RequestProfileReference
}

// referenceRuntimeFieldInput is the reference bridge's encrypted AuthIdentity.
// Every key is serialized, empty or not, in this order. The tokens make the
// plaintext a credential: it is only ever sealed, never logged or stored.
type referenceRuntimeFieldInput struct {
	Name               string `json:"name"`
	Aid                string `json:"aid"`
	UID                string `json:"uid"`
	YxUID              string `json:"yx_uid"`
	OrganizationID     string `json:"organization_id"`
	OrganizationName   string `json:"organization_name"`
	UserType           string `json:"user_type"`
	SecurityOAuthToken string `json:"security_oauth_token"`
	RefreshToken       string `json:"refresh_token"`
}

func (c *Client) referenceRuntimeInput(creds Credentials) referenceRuntimeFieldInput {
	return referenceRuntimeFieldInput{
		Name:               creds.Name,
		Aid:                creds.UID,
		UID:                creds.UID,
		OrganizationID:     creds.OrgID,
		UserType:           aliyunUserTypeOr(c.aliyunUserType()),
		SecurityOAuthToken: creds.AccessToken,
		RefreshToken:       creds.RefreshToken,
	}
}

// referenceRuntimeFields returns the reference-layout pair for creds. It is
// derived in memory per credential and never persisted: the stored pair keeps
// the default layout, so switching profiles back needs no repair. The tokens
// are part of the plaintext, so a rotation derives a new pair.
func (c *Client) referenceRuntimeFields(creds Credentials) (RuntimeFields, error) {
	if strings.TrimSpace(creds.UID) == "" {
		return RuntimeFields{}, fmt.Errorf("qoder account has no user id yet; sign in again")
	}
	c.stateMu.RLock()
	cached, cachedAccess, cachedRefresh := c.referenceRuntime, c.referenceAccess, c.referenceRefresh
	c.stateMu.RUnlock()
	if cached.Complete() && cachedAccess == creds.AccessToken && cachedRefresh == creds.RefreshToken {
		return cached, nil
	}
	plaintext, err := marshalNoEscape(c.referenceRuntimeInput(creds))
	if err != nil {
		return RuntimeFields{}, fmt.Errorf("marshal reference runtime identity: %w", err)
	}
	fields, err := sealRuntimePlaintext(c.entropy, plaintext)
	if err != nil {
		return RuntimeFields{}, err
	}
	c.stateMu.Lock()
	c.referenceRuntime, c.referenceAccess, c.referenceRefresh = fields, creds.AccessToken, creds.RefreshToken
	c.stateMu.Unlock()
	return fields, nil
}

// chatRuntimeFields returns the pair the configured profile signs chat with.
func (c *Client) chatRuntimeFields(ctx context.Context, creds Credentials) (RuntimeFields, error) {
	if c.referenceProfile() {
		return c.referenceRuntimeFields(creds)
	}
	return c.ensureRuntimeFields(ctx, creds)
}

// marshalNoEscape serializes v compactly without HTML escaping, as the
// reference bridge does.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// referenceBody reshapes a default chat body into the reference body: the CLI
// session type, the account class, {type,text} chat context objects, messages
// with response_meta and a contents array for user text, and none of the
// QoderWork-only fields.
func referenceBody(raw []byte, userType string) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var body map[string]any
	if err := decoder.Decode(&body); err != nil {
		return nil, fmt.Errorf("decode qoder body: %w", err)
	}
	for _, key := range []string{"is_reply", "is_retry", "system"} {
		delete(body, key)
	}
	if tools, ok := body["tools"].([]any); ok && len(tools) == 0 {
		delete(body, "tools")
	}
	body["session_type"] = referenceSessionType
	body["aliyun_user_type"] = aliyunUserTypeOr(userType)

	if chatContext, ok := body["chat_context"].(map[string]any); ok {
		for _, key := range []string{"chatPrompt", "features", "imageUrls"} {
			delete(chatContext, key)
		}
		chatContext["text"] = textObject(chatContext["text"])
		if extra, ok := chatContext["extra"].(map[string]any); ok {
			delete(extra, "context")
			extra["originalContent"] = textObject(extra["originalContent"])
		}
	}
	if business, ok := body["business"].(map[string]any); ok {
		for key := range business {
			if key != "id" && key != "name" && key != "begin_at" {
				delete(business, key)
			}
		}
	}
	if modelConfig, ok := body["model_config"].(map[string]any); ok {
		for key := range modelConfig {
			if key != "key" && key != "is_vl" && key != "is_reasoning" && key != "source" {
				delete(modelConfig, key)
			}
		}
	}
	if messages, ok := body["messages"].([]any); ok {
		for _, item := range messages {
			if message, ok := item.(map[string]any); ok {
				referenceMessage(message)
			}
		}
	}
	return marshalNoEscape(body)
}

func textObject(value any) map[string]any {
	if object, ok := value.(map[string]any); ok {
		return object
	}
	text, _ := value.(string)
	return map[string]any{"type": "text", "text": text}
}

// referenceMessage rewrites one history message the way the reference bridge
// builds it: every message carries an empty response_meta and signature, and
// user text travels in contents with an empty content string.
func referenceMessage(message map[string]any) {
	delete(message, "reasoning_content")
	delete(message, "reasoning_item")
	message["response_meta"] = map[string]any{
		"id": "",
		"usage": map[string]any{
			"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0,
			"completion_tokens_details": map[string]any{"reasoning_tokens": 0},
			"prompt_tokens_details":     map[string]any{"cached_tokens": 0},
		},
	}
	message["reasoning_content_signature"] = ""
	if role, _ := message["role"].(string); role != "user" {
		if _, ok := message["content"]; !ok {
			message["content"] = ""
		}
		return
	}
	parts, _ := message["contents"].([]any)
	for _, item := range parts {
		if part, ok := item.(map[string]any); ok {
			delete(part, "cache_control")
			delete(part, "source")
		}
	}
	if len(parts) == 0 {
		text, _ := message["content"].(string)
		parts = []any{map[string]any{"type": "text", "text": text}}
	}
	message["contents"] = parts
	message["content"] = ""
}

// referenceReplayBody gives a reference body a fresh request id, as
// refreshedReplayBody does for the default body, without passing it through
// the default body type that would restore the QoderWork shape.
func referenceReplayBody(encoded []byte, requestID string) ([]byte, error) {
	raw, err := decodeBody(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode qoder replay body: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var body map[string]any
	if err := decoder.Decode(&body); err != nil {
		return nil, fmt.Errorf("decode qoder replay JSON: %w", err)
	}
	body["request_id"] = requestID
	body["chat_record_id"] = requestID
	if business, ok := body["business"].(map[string]any); ok {
		business["begin_at"] = time.Now().UnixMilli()
	}
	updated, err := marshalNoEscape(body)
	if err != nil {
		return nil, fmt.Errorf("marshal qoder replay body: %w", err)
	}
	return EncodeBody(updated), nil
}

// replayBody refreshes a body's attempt identity in the configured profile.
func (c *Client) replayBody(encoded []byte, requestID string) ([]byte, error) {
	if c.referenceProfile() {
		return referenceReplayBody(encoded, requestID)
	}
	return refreshedReplayBody(encoded, requestID)
}

// applyReferenceHeaders sets the reference bridge's chat header set. The COSY
// payload carries its own request id: the reference signs a fresh one rather
// than the body's.
func (c *Client) applyReferenceHeaders(req *http.Request, creds Credentials, fields RuntimeFields, modelKey, modelSource string, body []byte, signedPath string) error {
	payloadID, err := newUUID(c.entropy)
	if err != nil {
		return err
	}
	unixSeconds := strconv.FormatInt(time.Now().Unix(), 10)
	authorization, err := buildCOSYAuthorization(payloadID, fields.EncryptUserInfo, referenceCosyVersion, fields.Key, unixSeconds, body, signedPath)
	if err != nil {
		return err
	}
	header := make(http.Header, 20)
	set := func(key, value string) { header[textproto.CanonicalMIMEHeaderKey(key)] = []string{value} }
	set("Accept", "text/event-stream")
	set("Accept-Encoding", "identity")
	set("Authorization", authorization)
	set("Cache-Control", "no-cache")
	set("Content-Type", "application/json")
	set("Cosy-Clienttype", referenceClientType)
	set("Cosy-Data-Policy", "AGREE")
	set("Cosy-Date", unixSeconds)
	set("Cosy-Key", fields.Key)
	set("Cosy-Machineid", c.machineID)
	set("Cosy-Machinetoken", referenceMachineToken(c.machineID))
	set("Cosy-Machinetype", referenceMachineType(c.machineID))
	set("Cosy-User", strings.TrimSpace(creds.UID))
	set("Cosy-Version", referenceCosyVersion)
	set("Login-Version", "v2")
	set("User-Agent", referenceUserAgent)
	if key := strings.TrimSpace(modelKey); key != "" {
		set("X-Model-Key", key)
		source := strings.TrimSpace(modelSource)
		if source == "" {
			source = "system"
		}
		set("X-Model-Source", source)
	}
	req.Header = header
	return nil
}

// referenceMachineToken and referenceMachineType have the reference bridge's
// shapes (a 50-character token, an 18-character type). The bridge draws them
// at random per process; here they are derived from the login device id so
// they stay stable across requests and restarts, as a real device's would.
func referenceMachineToken(machineID string) string {
	compact := strings.ReplaceAll(machineID, "-", "")
	token := machineID + compact
	if len(token) > 50 {
		token = token[:50]
	}
	return token
}

func referenceMachineType(machineID string) string {
	if len(machineID) > 18 {
		return machineID[:18]
	}
	return machineID
}

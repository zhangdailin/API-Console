package qoder

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/textproto"
	"slices"
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

	// RequestProfileBuddySwitch is the request NextAgentX/qoder-trae-workbuddy-
	// switch (buddy-switch, e664b38) sends: the same CLI shape as the
	// reference profile with a newer client version, a fixed client-ip header,
	// an identity serialized with sorted keys, and the CLI template's full body.
	RequestProfileBuddySwitch = "buddyswitch"

	buddySwitchCosyVersion     = "1.1.64"
	buddySwitchClientIP        = "169.254.198.161"
	buddySwitchDefaultUserType = "personal_professional_trial"
	buddySwitchBusinessVersion = "0.1.43"
	referenceClientType        = "5"
	referenceUserAgent         = "Go-http-client/2.0"
	referenceSessionType       = "qodercli"
)

// resolveRequestProfile maps the configured profile onto a known one. An
// unknown value keeps the default rather than inventing a request shape.
func resolveRequestProfile(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case RequestProfileReference:
		return RequestProfileReference
	case RequestProfileBuddySwitch:
		return RequestProfileBuddySwitch
	}
	return RequestProfileQoderWork
}

// referenceProfile reports a CLI-shaped profile (reference or buddyswitch):
// both sign with the token-bearing identity and build the CLI request.
func (c *Client) referenceProfile() bool {
	return c != nil && isCLIProfile(c.profile)
}

func isCLIProfile(profile string) bool {
	return profile == RequestProfileReference || profile == RequestProfileBuddySwitch
}

// cliCosyVersion is the client version a CLI profile signs and sends.
func cliCosyVersion(profile string) string {
	if profile == RequestProfileBuddySwitch {
		return buddySwitchCosyVersion
	}
	return referenceCosyVersion
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

// cliRuntimePlaintext is the identity a CLI profile seals. buddy-switch
// serializes it with its keys sorted and defaults the account class to the
// professional trial; the reference keeps struct order.
func (c *Client) cliRuntimePlaintext(creds Credentials, profile string) ([]byte, error) {
	in := c.referenceRuntimeInput(creds)
	if profile != RequestProfileBuddySwitch {
		return marshalNoEscape(in)
	}
	userType := strings.TrimSpace(c.aliyunUserType())
	if userType == "" {
		userType = buddySwitchDefaultUserType
	}
	return sortedCompactJSON(map[string]string{
		"name": in.Name, "aid": in.Aid, "uid": in.UID, "yx_uid": in.YxUID,
		"organization_id": in.OrganizationID, "organization_name": in.OrganizationName,
		"user_type": userType, "security_oauth_token": in.SecurityOAuthToken, "refresh_token": in.RefreshToken,
	})
}

// sortedCompactJSON renders a flat string object with sorted keys and no
// insignificant whitespace, as buddy-switch's json_sorted_compact does.
func sortedCompactJSON(values map[string]string) ([]byte, error) {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var out bytes.Buffer
	out.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			out.WriteByte(',')
		}
		k, err := marshalNoEscape(key)
		if err != nil {
			return nil, err
		}
		v, err := marshalNoEscape(values[key])
		if err != nil {
			return nil, err
		}
		out.Write(k)
		out.WriteByte(':')
		out.Write(v)
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

// referenceRuntimeFields returns the reference-layout pair for creds. It is
// derived in memory per credential and never persisted: the stored pair keeps
// the default layout, so switching profiles back needs no repair. The tokens
// are part of the plaintext, so a rotation derives a new pair.
func (c *Client) referenceRuntimeFields(creds Credentials) (RuntimeFields, error) {
	return c.cliRuntimeFields(creds, RequestProfileReference)
}

// cliRuntimeFields returns the pair a CLI profile signs with, cached per
// profile and token pair.
func (c *Client) cliRuntimeFields(creds Credentials, profile string) (RuntimeFields, error) {
	if strings.TrimSpace(creds.UID) == "" {
		return RuntimeFields{}, fmt.Errorf("qoder account has no user id yet; sign in again")
	}
	c.stateMu.RLock()
	cached, cachedAccess, cachedRefresh, cachedProfile := c.referenceRuntime, c.referenceAccess, c.referenceRefresh, c.referenceProfileName
	c.stateMu.RUnlock()
	if cached.Complete() && cachedProfile == profile && cachedAccess == creds.AccessToken && cachedRefresh == creds.RefreshToken {
		return cached, nil
	}
	plaintext, err := c.cliRuntimePlaintext(creds, profile)
	if err != nil {
		return RuntimeFields{}, fmt.Errorf("marshal reference runtime identity: %w", err)
	}
	fields, err := sealRuntimePlaintext(c.entropy, plaintext)
	if err != nil {
		return RuntimeFields{}, err
	}
	c.stateMu.Lock()
	c.referenceRuntime, c.referenceAccess, c.referenceRefresh, c.referenceProfileName = fields, creds.AccessToken, creds.RefreshToken, profile
	c.stateMu.Unlock()
	return fields, nil
}

// chatRuntimeFields returns the pair the configured profile signs chat with.
func (c *Client) chatRuntimeFields(ctx context.Context, creds Credentials) (RuntimeFields, error) {
	if c.referenceProfile() {
		return c.cliRuntimeFields(creds, c.profile)
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

// cliBody reshapes a default chat body for a CLI profile.
func cliBody(raw []byte, userType, profile string) ([]byte, error) {
	if profile == RequestProfileBuddySwitch {
		return buddySwitchBody(raw, userType)
	}
	return referenceBody(raw, userType)
}

// buddySwitchBody reshapes a default chat body into buddy-switch's: its CLI
// template (baseprompt.json) filled the way build_chat_body fills it. Unlike
// the reference body it keeps the template's full chat_context, model_config
// and business card, adds the template's empty image_urls / code_language /
// chat_prompt fields, and sends history messages as plain role/content.
//
// Two of buddy-switch's behaviours are not reproduced. It flattens tool calls
// into marked text for the international region and parses them back out of
// the answer; this gateway keeps native tool calls. And it inserts the
// official CLI system prompt when a client sends none; clients of this
// gateway send their own.
func buddySwitchBody(raw []byte, userType string) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var body map[string]any
	if err := decoder.Decode(&body); err != nil {
		return nil, fmt.Errorf("decode qoder body: %w", err)
	}
	delete(body, "system")
	body["session_type"] = referenceSessionType
	userType = strings.TrimSpace(userType)
	if userType == "" {
		userType = buddySwitchDefaultUserType
	}
	body["aliyun_user_type"] = userType
	body["is_reply"] = true
	body["is_retry"] = false
	body["image_urls"] = nil
	body["code_language"] = ""
	body["chat_prompt"] = ""
	if _, ok := body["tools"].([]any); !ok {
		body["tools"] = []any{}
	}
	if chatContext, ok := body["chat_context"].(map[string]any); ok {
		chatContext["text"] = textObject(chatContext["text"])
		if _, ok := chatContext["chatPrompt"]; !ok {
			chatContext["chatPrompt"] = ""
		}
		if _, ok := chatContext["features"]; !ok {
			chatContext["features"] = []any{}
		}
		if _, ok := chatContext["imageUrls"]; !ok {
			chatContext["imageUrls"] = nil
		}
		if extra, ok := chatContext["extra"].(map[string]any); ok {
			extra["originalContent"] = textObject(extra["originalContent"])
			if _, ok := extra["context"]; !ok {
				extra["context"] = []any{}
			}
		}
	}
	if business, ok := body["business"].(map[string]any); ok {
		next := map[string]any{
			"product": "cli",
			"version": buddySwitchBusinessVersion,
			"type":    "agent",
			"stage":   "start",
		}
		for _, key := range []string{"id", "name", "begin_at"} {
			if value, ok := business[key]; ok {
				next[key] = value
			}
		}
		body["business"] = next
	}
	if messages, ok := body["messages"].([]any); ok {
		for _, item := range messages {
			if message, ok := item.(map[string]any); ok {
				// buddy-switch sends history as plain role/content (and native
				// tool calls); the QoderWork echo fields are not part of it.
				delete(message, "response_meta")
				delete(message, "reasoning_item")
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
	return c.applyCLIHeaders(req, creds, fields, modelKey, modelSource, body, signedPath, RequestProfileReference)
}

// applyCLIHeaders sets a CLI profile's chat header set.
func (c *Client) applyCLIHeaders(req *http.Request, creds Credentials, fields RuntimeFields, modelKey, modelSource string, body []byte, signedPath, profile string) error {
	payloadID, err := newUUID(c.entropy)
	if err != nil {
		return err
	}
	version := cliCosyVersion(profile)
	unixSeconds := strconv.FormatInt(time.Now().Unix(), 10)
	authorization, err := buildCOSYAuthorization(payloadID, fields.EncryptUserInfo, version, fields.Key, unixSeconds, body, signedPath)
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
	machineToken, machineType := referenceMachineToken(c.machineID), referenceMachineType(c.machineID)
	if profile == RequestProfileBuddySwitch {
		machineToken, machineType = buddySwitchMachineToken(creds.UID), buddySwitchMachineType(creds.UID)
		set("Cosy-Clientip", buddySwitchClientIP)
	}
	set("Cosy-Machinetoken", machineToken)
	set("Cosy-Machinetype", machineType)
	set("Cosy-User", strings.TrimSpace(creds.UID))
	set("Cosy-Version", version)
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

// buddySwitchMachineToken and buddySwitchMachineType are buddy-switch's
// per-account device fields (device.rs machine_identity): stable values
// derived from the UID. Its machine id is derived the same way, but here the
// login device id is kept: the credential is bound to the device that
// performed the login.
func buddySwitchMachineToken(uid string) string {
	sum := sha512.Sum512([]byte("machinetoken:" + uid))
	token := base64.RawURLEncoding.EncodeToString(sum[:])
	return token[:43]
}

func buddySwitchMachineType(uid string) string {
	sum := md5.Sum([]byte("machinetype:" + uid))
	return hex.EncodeToString(sum[:])[:18]
}

// cliHostOrder puts a profile's preferred node first: buddy-switch sends to
// api1 and fails over to api2 and api3. Custom hosts are left alone.
func (c *Client) cliHostOrder(hosts []string) []string {
	if c.profile != RequestProfileBuddySwitch || len(c.endpoints.inferenceFallbacks) == 0 {
		return hosts
	}
	const preferred = "https://api1.qoder.sh"
	index := slices.Index(hosts, preferred)
	if index <= 0 {
		return hosts
	}
	return append([]string{preferred}, slices.Delete(slices.Clone(hosts), index, index+1)...)
}

package qoder

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

func TestReferenceDefaultsAndOverrides(t *testing.T) {
	for _, cfg := range []*config.Config{nil, {}} {
		c := NewFromAccount(signedTestAccount(), cfg)
		testutil.Equal(t, c.clientID, DefaultClientID)
		testutil.Equal(t, c.endpoints.inference, DefaultInferenceURL)
		cfg = &config.Config{QoderClientID: "custom-id"}
		cfg.QoderClientVersion = "custom-version"
		cfg.QoderInferenceURL = "http://127.0.0.1:19999"
		c = NewFromAccount(signedTestAccount(), cfg)
		testutil.False(t, c.clientID != "custom-id" || c.clientVersion != "custom-version" || c.endpoints.inference != cfg.QoderInferenceURL, "explicit overrides lost")
	}
	nilClient := NewFromAccount(signedTestAccount(), nil)
	testutil.False(t, nilClient.clientID != DefaultClientID || nilClient.endpoints.inference != DefaultInferenceURL, "default compatibility changed")
}

func TestReferenceRuntimeUsesCurrentIdentity(t *testing.T) {
	c := NewFromAccount(signedTestAccount(), &config.Config{})
	// Deterministic, nonzero RSA padding; these are synthetic credentials only.
	entropy := bytes.Repeat([]byte{17}, 2048)
	c.entropy = newRecordingSource(entropy)
	creds := c.currentCredentials()
	got, err := c.ensureRuntimeFields(context.Background(), creds)
	testutil.NoError(t, err)
	want, err := referenceRuntimeFieldsFor(newRecordingSource(entropy), referenceRuntimeFieldInput{Name: creds.Name, Aid: creds.UID, UID: creds.UID, OrganizationID: creds.OrgID, UserType: aliyunUserTypeOr(c.aliyunUserType()), SecurityOAuthToken: creds.AccessToken, RefreshToken: creds.RefreshToken})
	testutil.Equal(t, err, nil)
	testutil.Equal(t, got, want)
	// A new client derives its own pair instead of trusting stored ciphertext.
	snapshot := signedTestAccount()
	c.CopyAccountState(snapshot)
	switched := NewFromAccount(snapshot, nil)
	testutil.False(t, switched.runtimeSnapshot().Complete(), "new client reused persisted runtime")
}

func TestReferenceChatBodyAndHeadersAgree(t *testing.T) {
	var c *Client
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		encoded, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		raw, err := decodeBody(encoded)
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		var body chatBody
		if err = json.Unmarshal(raw, &body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		product := sceneBusinessProduct
		testutil.CheckFalse(t, body.Business.Product != product || r.Header.Get("Cosy-Business-Product") != product, "body/header dialect mismatch")
		fields := c.runtimeSnapshot()
		parts := strings.Split(r.Header.Get("Authorization"), ".")
		if len(parts) != 3 || parts[0] != "Bearer COSY" {
			t.Error("invalid COSY authorization framing")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		payloadJSON, err := base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var payload map[string]string
		if err := json.Unmarshal(payloadJSON, &payload); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		wantPayload := map[string]string{"version": "v1", "requestId": body.RequestID, "info": fields.EncryptUserInfo, "cosyVersion": c.clientVersion, "ideVersion": ""}
		testutil.CheckEqual(t, len(payload), len(wantPayload))
		for key, want := range wantPayload {
			got, ok := payload[key]
			testutil.CheckFalsef(t, !ok || got != want, "COSY payload %q = %q (present=%t), want %q", key, got, ok, want)
		}
		// Verify the actual payload and body bytes independently of both
		// production authorization construction and signature generation.
		path := strings.TrimPrefix(r.URL.Path, "/algo")
		preimage := strings.Join([]string{parts[1], fields.Key, r.Header.Get("Cosy-Date"), string(encoded), path}, "\n")
		wantSignature := fmt.Sprintf("%x", md5.Sum([]byte(preimage)))
		testutil.CheckEqual(t, parts[2], wantSignature)
		_, _ = io.WriteString(w, envelope(`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`)+"event:finish\n\n")
	}))
	defer srv.Close()
	c = NewFromAccount(signedTestAccount(), &config.Config{QoderInferenceURL: srv.URL})
	testutil.NoError(t, c.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{Model: "Qwen3.7-Max", Prompt: "hello"}, nil, nil))
	testutil.Equal(t, hits, 1)
}

// TestDefaultUpstreamEndpoints pins the international nodes the channel talks
// to. The chat SSE call and the model catalog are two observations of the same
// inference host, so a host change has to move both together.

func TestDefaultUpstreamEndpoints(t *testing.T) {
	t.Parallel()

	c := NewFromAccount(signedTestAccount(), nil)
	want := endpoints{
		oauth:     "https://qoder.com",
		openAPI:   "https://openapi.qoder.sh",
		inference: "https://api2.qoder.sh",
	}
	testutil.Equal(t, c.endpoints, want)
	got, wantURL := chatURL(c.endpoints.inference), "https://api2.qoder.sh"+inferPath+inferQuery
	testutil.Falsef(t, got != wantURL, "chatURL() = %q, want %q", got, wantURL)
	got, wantURL = c.endpoints.inference+modelListPath, "https://api2.qoder.sh/algo/api/v2/model/list?Encode=1"
	testutil.Falsef(t, got != wantURL, "catalog url = %q, want %q", got, wantURL)
}

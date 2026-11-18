package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const clientVersion = "v0.2026.08.19.08.15.stable_01"
const apiBase = "https://app.warp.dev"

var pendingAuthorization = errors.New("authorization pending")
var slowAuthorization = errors.New("authorization slow down")

type probeClient struct {
	http            *http.Client
	base, customURL string
}
type device struct {
	DeviceCode           string `json:"device_code"`
	UserCode             string `json:"user_code"`
	VerificationURI      string `json:"verification_uri"`
	VerificationComplete string `json:"verification_uri_complete"`
	ExpiresIn            int    `json:"expires_in"`
	Interval             int    `json:"interval"`
}

func newProbeClient(key string) *probeClient {
	t := http.DefaultTransport.(*http.Transport).Clone()
	return &probeClient{http: &http.Client{Transport: t, Timeout: 90 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, base: apiBase, customURL: "https://identitytoolkit.googleapis.com/v1/accounts:signInWithCustomToken?" + url.Values{"key": {key}}.Encode()}
}
func (c *probeClient) request(ctx context.Context, endpoint, contentType string, body []byte, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("request construction failed")
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Warp-Client-ID", "warp-app")
	req.Header.Set("X-Warp-OS-Category", "Linux")
	req.Header.Set("X-Warp-OS-Name", "Linux")
	req.Header.Set("User-Agent", "")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New("upstream transport failed (URL and credentials suppressed)")
	}
	return resp, nil
}
func boundedBody(r io.Reader) ([]byte, error) {
	b, e := io.ReadAll(io.LimitReader(r, (2<<20)+1))
	if e != nil {
		return nil, errors.New("upstream body read failed")
	}
	if len(b) > 2<<20 {
		return nil, errors.New("upstream response exceeded limit")
	}
	return b, nil
}
func (c *probeClient) json(ctx context.Context, endpoint, contentType string, body []byte, token string, target any) error {
	r, e := c.request(ctx, endpoint, contentType, body, token)
	if e != nil {
		return e
	}
	defer r.Body.Close()
	b, e := boundedBody(r.Body)
	if e != nil {
		return e
	}
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		var d struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(b, &d)
		if d.Error == "authorization_pending" {
			return pendingAuthorization
		}
		if d.Error == "slow_down" {
			return slowAuthorization
		}
		return fmt.Errorf("upstream HTTP %d (body suppressed)", r.StatusCode)
	}
	if json.Unmarshal(b, target) != nil {
		return errors.New("invalid upstream JSON (body suppressed)")
	}
	return nil
}
func validVerificationURL(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && u.Scheme == "https" && u.User == nil && u.Port() == "" && (u.Hostname() == "app.warp.dev" || u.Hostname() == "warp.dev") && u.Fragment == ""
}
func (c *probeClient) start(ctx context.Context) (device, error) {
	var d device
	e := c.json(ctx, c.base+"/api/v1/oauth/device/auth", "application/x-www-form-urlencoded", []byte("client_id=warp-agent-cli"), "", &d)
	if e != nil {
		return d, e
	}
	if d.DeviceCode == "" || d.UserCode == "" || !validVerificationURL(d.VerificationURI) || (d.VerificationComplete != "" && !validVerificationURL(d.VerificationComplete)) {
		return device{}, errors.New("invalid device authorization response")
	}
	if d.Interval < 1 {
		d.Interval = 2
	}
	if d.ExpiresIn < 1 {
		d.ExpiresIn = 600
	}
	if d.ExpiresIn > 900 {
		d.ExpiresIn = 900
	}
	return d, nil
}
func (c *probeClient) exchangeCustom(ctx context.Context, d device) (string, error) {
	form := url.Values{"client_id": {"warp-agent-cli"}, "device_code": {d.DeviceCode}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}}
	var grant struct {
		AccessToken string `json:"access_token"`
	}
	if e := c.json(ctx, c.base+"/api/v1/oauth/token", "application/x-www-form-urlencoded", []byte(form.Encode()), "", &grant); e != nil {
		return "", e
	}
	if grant.AccessToken == "" {
		return "", errors.New("device grant missing access token")
	}
	return grant.AccessToken, nil
}
func (c *probeClient) exchange(ctx context.Context, d device) (string, error) {
	custom, err := c.exchangeCustom(ctx, d)
	if err != nil {
		return "", err
	}
	return c.exchangeFirebase(ctx, custom)
}
func (c *probeClient) exchangeFirebase(ctx context.Context, custom string) (string, error) {
	payload := []byte(url.Values{"returnSecureToken": {"true"}, "token": {custom}}.Encode())
	var firebase struct {
		IDToken string `json:"idToken"`
	}
	if e := c.json(ctx, c.customURL, "application/x-www-form-urlencoded", payload, "", &firebase); e != nil {
		return "", e
	}
	if firebase.IDToken == "" {
		return "", errors.New("Firebase exchange missing ID token")
	}
	return firebase.IDToken, nil
}
func (c *probeClient) await(ctx context.Context, d device) (string, error) {
	return c.awaitExchange(ctx, d, c.exchange)
}
func (c *probeClient) awaitCustom(ctx context.Context, d device) (string, error) {
	return c.awaitExchange(ctx, d, c.exchangeCustom)
}
func (c *probeClient) awaitExchange(ctx context.Context, d device, exchange func(context.Context, device) (string, error)) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(d.ExpiresIn)*time.Second)
	defer cancel()
	interval := time.Duration(d.Interval) * time.Second
	for {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", errors.New("device authorization expired or cancelled")
		case <-timer.C:
		}
		token, e := exchange(ctx, d)
		if e == nil {
			return token, nil
		}
		if errors.Is(e, slowAuthorization) {
			interval += 5 * time.Second
			continue
		}
		if errors.Is(e, pendingAuthorization) {
			continue
		}
		return "", e
	}
}

const catalogQuery = `query GetFeatureModelChoices($requestContext: RequestContext!) { user(requestContext: $requestContext) { __typename ... on UserOutput { user { workspaces { featureModelChoice { agentMode { defaultId choices { id disableReason } } } } } } } }`

var modelID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$`)

func (c *probeClient) models(ctx context.Context, token string) ([]string, string, error) {
	payload, _ := json.Marshal(map[string]any{"query": catalogQuery, "operationName": "GetFeatureModelChoices", "variables": map[string]any{"requestContext": map[string]any{"clientContext": map[string]any{"version": clientVersion}, "osContext": map[string]any{"category": "Linux", "name": "Linux", "version": "", "linuxKernelVersion": nil}}}})
	var d struct {
		Data struct {
			User struct {
				Type string `json:"__typename"`
				User struct {
					Workspaces []struct {
						Feature struct {
							Agent struct {
								Default string `json:"defaultId"`
								Choices []struct {
									ID       string `json:"id"`
									Disabled string `json:"disableReason"`
								} `json:"choices"`
							} `json:"agentMode"`
						} `json:"featureModelChoice"`
					} `json:"workspaces"`
				} `json:"user"`
			} `json:"user"`
		} `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if e := c.json(ctx, c.base+"/graphql/v2?op=GetFeatureModelChoices", "application/json", payload, token, &d); e != nil {
		return nil, "", e
	}
	if len(d.Errors) > 0 || d.Data.User.Type != "UserOutput" {
		return nil, "", errors.New("catalog rejected (upstream details suppressed)")
	}
	seen := map[string]bool{}
	var rows []string
	preferred := ""
	for _, w := range d.Data.User.User.Workspaces {
		for _, m := range w.Feature.Agent.Choices {
			if strings.TrimSpace(m.Disabled) != "" || !modelID.MatchString(m.ID) || seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			rows = append(rows, m.ID)
			if m.ID == w.Feature.Agent.Default {
				preferred = m.ID
			}
		}
	}
	if len(rows) == 0 {
		return nil, "", errors.New("catalog has no enabled agent model")
	}
	if preferred == "" {
		preferred = rows[0]
	}
	return rows, preferred, nil
}
func (c *probeClient) generateObserved(ctx context.Context, token, model string, catalog []string) (streamResult, error) {
	found := false
	for _, observed := range catalog {
		if model == observed {
			found = true
			break
		}
	}
	if !found {
		return streamResult{}, errors.New("model not in this session's observed catalog")
	}
	return c.generate(ctx, token, model)
}

func (c *probeClient) generate(ctx context.Context, token, model string) (streamResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	payload, e := buildProbeRequest(model)
	if e != nil {
		return streamResult{}, e
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/ai/multi-agent", bytes.NewReader(payload))
	if e != nil {
		return streamResult{}, errors.New("generation request invalid")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Warp-Client-ID", "warp-app")
	req.Header.Set("X-Warp-OS-Category", "Linux")
	req.Header.Set("X-Warp-OS-Name", "Linux")
	req.Header.Set("User-Agent", "")
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Encoding", "identity")
	r, e := c.http.Do(req)
	if e != nil {
		return streamResult{}, errors.New("generation transport failed (details suppressed)")
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return streamResult{}, fmt.Errorf("generation HTTP %d (body suppressed)", r.StatusCode)
	}
	kind, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if kind != "text/event-stream" {
		return streamResult{}, errors.New("generation response is not SSE")
	}
	return readProbeStreamForModel(r.Body, model)
}

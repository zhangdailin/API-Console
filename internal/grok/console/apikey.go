package console

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ── API key ────────────────────────────────────────────────────────────────

// apiKey returns the team's API key id, sent as x-apikey on every /v1
// inference request (free Console teams are refused without it). It is read
// from the Console home page and created only when the team has none, because
// CreateApiKey has no idempotency guard.
func (c *Client) apiKey(ctx context.Context, ssoToken string, refresh bool) (string, error) {
	key := sessionKey(ssoToken)
	if !refresh {
		c.mu.Lock()
		value := c.apiKeys[key]
		c.mu.Unlock()
		if value != "" {
			return value, nil
		}
	}
	value, err, _ := c.fetches.Do("apikey|"+key, func() (any, error) {
		resolved, err := c.resolveAPIKey(context.WithoutCancel(ctx), ssoToken)
		if err != nil {
			return "", err
		}
		c.mu.Lock()
		c.apiKeys[key] = resolved
		c.mu.Unlock()
		return resolved, nil
	})
	if err != nil {
		return "", err
	}
	return value.(string), nil
}

func (c *Client) forgetAPIKey(ssoToken string) {
	c.mu.Lock()
	delete(c.apiKeys, sessionKey(ssoToken))
	c.mu.Unlock()
}

func (c *Client) resolveAPIKey(ctx context.Context, ssoToken string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	home, err := c.readHome(ctx, ssoToken)
	if err != nil {
		return "", err
	}
	if home.apiKeyID != "" {
		return home.apiKeyID, nil
	}
	if home.teamID == "" {
		return "", fmt.Errorf("%w: no team id", errNoAPIKeyFound)
	}
	if err := c.createAPIKey(ctx, ssoToken, home.teamID); err != nil {
		return "", err
	}
	// The create response carries the secret key, not its id: re-read the
	// page so only the id is ever used.
	confirmed, err := c.readHome(ctx, ssoToken)
	if err != nil {
		return "", err
	}
	if confirmed.apiKeyID == "" {
		return "", fmt.Errorf("%w after creating one", errNoAPIKeyFound)
	}
	return confirmed.apiKeyID, nil
}

type consoleHome struct{ apiKeyID, teamID string }

// readHome loads the Console landing page, which redirects to /team/<id>.
// Only same-origin hops are followed.
func (c *Client) readHome(ctx context.Context, ssoToken string) (consoleHome, error) {
	current, err := url.Parse(c.baseURL() + "/")
	if err != nil {
		return consoleHome{}, err
	}
	client := *c.httpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	extra := map[string]string{}
	for hop := 0; hop <= homeMaxRedirects; hop++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current.String(), nil)
		if err != nil {
			return consoleHome{}, err
		}
		c.applyBrowserHeaders(req.Header, ssoToken)
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		req.Header.Set("Sec-Fetch-Dest", "document")
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		req.Header.Set("Sec-Fetch-Site", "none")
		req.Header.Del("Origin")
		req.Header.Del("Referer")
		for name, value := range extra {
			req.Header.Set("Cookie", req.Header.Get("Cookie")+"; "+name+"="+value)
		}
		resp, err := client.Do(req)
		if err != nil {
			return consoleHome{}, err
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			for _, cookie := range resp.Cookies() {
				if cookie.Name != "sso" && cookie.Name != "sso-rw" && !strings.ContainsAny(cookie.Name+cookie.Value, ";\r\n") {
					extra[cookie.Name] = cookie.Value
				}
			}
			_ = resp.Body.Close()
			next, err := current.Parse(resp.Header.Get("Location"))
			if err != nil || resp.Header.Get("Location") == "" || !strings.EqualFold(next.Host, current.Host) {
				return consoleHome{}, fmt.Errorf("the Console home page redirected off console.x.ai (status %d)", resp.StatusCode)
			}
			current = next
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxControlBody))
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			return consoleHome{}, ErrUnauthorized
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return consoleHome{}, &StatusError{Status: resp.StatusCode, Message: "the Console home page could not be read"}
		}
		if readErr != nil {
			return consoleHome{}, readErr
		}
		home := parseHome(data)
		if match := teamPathPattern.FindStringSubmatch(current.Path); match != nil {
			home.teamID = strings.ToLower(match[1])
		}
		return home, nil
	}
	return consoleHome{}, errors.New("the Console home page redirected too many times")
}

// parseHome extracts the first enabled apiKeyId and the team id from the
// page's embedded (escaped) JSON. UUIDs hold no backslashes, so dropping them
// yields plain JSON fragments to scan.
func parseHome(data []byte) consoleHome {
	text := string(bytes.ReplaceAll(data, []byte(`\`), nil))
	var home consoleHome
	matches := apiKeyIDPattern.FindAllStringSubmatchIndex(text, -1)
	for index, match := range matches {
		end := len(text)
		if index+1 < len(matches) {
			end = matches[index+1][0]
		}
		end = min(end, match[1]+apiKeyScanDistance)
		if disabledPattern.MatchString(text[match[1]:end]) {
			continue
		}
		home.apiKeyID = strings.ToLower(text[match[2]:match[3]])
		break
	}
	if match := teamIDPattern.FindStringSubmatch(text); match != nil {
		home.teamID = strings.ToLower(match[1])
	} else if match := teamPathPattern.FindStringSubmatch(text); match != nil {
		home.teamID = strings.ToLower(match[1])
	}
	return home
}

func (c *Client) createAPIKey(ctx context.Context, ssoToken, teamID string) error {
	target := c.baseURL() + createAPIKeyPath
	resp, err := c.dpopRequest(ctx, ssoToken, http.MethodPost, target, createAPIKeyBody(teamID), requestOptions{
		contentType: "application/connect+proto", referer: c.baseURL() + "/team/" + url.PathEscape(teamID) + "/chat",
	})
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxControlBody))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &StatusError{Status: resp.StatusCode, Message: "Console CreateApiKey was refused"}
	}
	return nil
}

// createAPIKeyBody is the Connect envelope (flag byte, big-endian length)
// around CreateApiKeyRequest{name=1, team_id=2, acls=3...}.
func createAPIKeyBody(teamID string) []byte {
	var payload []byte
	field := func(number int, value string) {
		payload = append(payload, byte(number<<3|2))
		length := len(value)
		for length >= 0x80 {
			payload = append(payload, byte(length)|0x80)
			length >>= 7
		}
		payload = append(payload, byte(length))
		payload = append(payload, value...)
	}
	field(1, apiKeyName)
	field(2, teamID)
	field(3, "api-key:endpoint:*")
	field(3, "api-key:model:*")
	length := len(payload)
	envelope := []byte{0, byte(length >> 24), byte(length >> 16), byte(length >> 8), byte(length)}
	return append(envelope, payload...)
}

func isMissingAPIKeyBody(body []byte) bool {
	text := strings.ToLower(string(body))
	return strings.Contains(text, "provision an api key") || strings.Contains(text, "not the owner")
}

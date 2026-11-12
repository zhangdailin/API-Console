package console

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"orchids-api/internal/util"
	"strings"
	"time"
)

// ── DPoP ───────────────────────────────────────────────────────────────────

type dpopJWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

type dpopSession struct {
	accessToken string
	privateKey  *ecdsa.PrivateKey
	publicJWK   dpopJWK
	expiresAt   time.Time
	clockSkew   time.Duration
}

func (c *Client) cachedSession(key string) (dpopSession, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	session, ok := c.sessions[key]
	if ok && session.expiresAt.After(time.Now().Add(dpopRefreshSkew)) {
		return session, true
	}
	delete(c.sessions, key)
	return dpopSession{}, false
}

func (c *Client) storeSession(key string, session dpopSession) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sessions) >= maxDPoPSessions {
		now := time.Now().Add(dpopRefreshSkew)
		for k, s := range c.sessions {
			if !s.expiresAt.After(now) {
				delete(c.sessions, k)
			}
		}
		for k := range c.sessions {
			if len(c.sessions) < maxDPoPSessions {
				break
			}
			delete(c.sessions, k)
		}
	}
	c.sessions[key] = session
}

func (c *Client) invalidateSession(key, accessToken string) {
	c.mu.Lock()
	if session, ok := c.sessions[key]; ok && session.accessToken == accessToken {
		delete(c.sessions, key)
	}
	c.mu.Unlock()
}

// session returns a cached DPoP session for the credential, minting one when
// none is valid. Concurrent misses share one mint.
func (c *Client) session(ctx context.Context, ssoToken string) (dpopSession, string, error) {
	key := sessionKey(ssoToken)
	if session, ok := c.cachedSession(key); ok {
		return session, key, nil
	}
	result := c.fetches.DoChan("dpop|"+key, func() (any, error) {
		if session, ok := c.cachedSession(key); ok {
			return session, nil
		}
		session, err := c.mintSession(context.WithoutCancel(ctx), ssoToken)
		if err == nil {
			c.storeSession(key, session)
		}
		return session, err
	})
	select {
	case <-ctx.Done():
		return dpopSession{}, key, ctx.Err()
	case done := <-result:
		if done.Err != nil {
			return dpopSession{}, key, done.Err
		}
		return done.Val.(dpopSession), key, nil
	}
}

func (c *Client) mintSession(ctx context.Context, ssoToken string) (dpopSession, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return dpopSession{}, err
	}
	jwk := publicDPoPJWK(&key.PublicKey)
	body, _ := json.Marshal(map[string]any{"jwk": jwk})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/dpop/token"), bytes.NewReader(body))
	if err != nil {
		return dpopSession{}, err
	}
	c.applyBrowserHeaders(req.Header, ssoToken)
	req.Header.Set("Content-Type", "application/json")
	before := time.Now().UTC()
	resp, err := c.httpClient.Do(req)
	after := time.Now().UTC()
	if err != nil {
		return dpopSession{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return dpopSession{}, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return dpopSession{}, ErrUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return dpopSession{}, &StatusError{Status: resp.StatusCode, Message: upstreamErrorMessage(raw)}
	}
	var out struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return dpopSession{}, errors.New("the Console DPoP token response is not JSON")
	}
	if out.AccessToken == "" || !strings.EqualFold(out.TokenType, "DPoP") || out.ExpiresIn <= 0 || time.Duration(out.ExpiresIn)*time.Second > maxDPoPLifetime {
		return dpopSession{}, errors.New("the Console DPoP token response is invalid")
	}
	thumbprint := dpopJWKThumbprint(jwk)
	tokenExpiry, tokenThumbprint, err := parseDPoPAccessToken(out.AccessToken)
	if err != nil || tokenThumbprint != thumbprint {
		return dpopSession{}, errors.New("the Console DPoP token is not bound to this key")
	}
	now := time.Now().UTC()
	expires := now.Add(time.Duration(out.ExpiresIn) * time.Second)
	if tokenExpiry.Before(expires) {
		expires = tokenExpiry
	}
	if !expires.After(now.Add(dpopRefreshSkew)) {
		return dpopSession{}, errors.New("the Console DPoP token is already expiring")
	}
	return dpopSession{accessToken: out.AccessToken, privateKey: key, publicJWK: jwk, expiresAt: expires, clockSkew: dpopClockSkew(resp.Header.Get("Date"), before, after)}, nil
}

func publicDPoPJWK(key *ecdsa.PublicKey) dpopJWK {
	return dpopJWK{
		Kty: "EC", Crv: "P-256",
		X: base64.RawURLEncoding.EncodeToString(key.X.FillBytes(make([]byte, 32))),
		Y: base64.RawURLEncoding.EncodeToString(key.Y.FillBytes(make([]byte, 32))),
	}
}

// dpopJWKThumbprint is the RFC 7638 thumbprint: members in lexical order.
func dpopJWKThumbprint(jwk dpopJWK) string {
	canonical := `{"crv":"` + jwk.Crv + `","kty":"` + jwk.Kty + `","x":"` + jwk.X + `","y":"` + jwk.Y + `"}`
	digest := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func parseDPoPAccessToken(value string) (time.Time, string, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return time.Time{}, "", errors.New("invalid DPoP access token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, "", errors.New("invalid DPoP access token payload")
	}
	var claims struct {
		ExpiresAt int64 `json:"exp"`
		CNF       struct {
			JKT string `json:"jkt"`
		} `json:"cnf"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.ExpiresAt <= 0 || strings.TrimSpace(claims.CNF.JKT) == "" {
		return time.Time{}, "", errors.New("invalid DPoP access token claims")
	}
	return time.Unix(claims.ExpiresAt, 0).UTC(), claims.CNF.JKT, nil
}

// dpopClockSkew estimates server time minus local time from the mint
// response's Date header; proof iat is shifted by it.
func dpopClockSkew(date string, before, after time.Time) time.Duration {
	server, err := http.ParseTime(strings.TrimSpace(date))
	if err != nil {
		return 0
	}
	if after.Before(before) {
		after = before
	}
	mid := before.Add(after.Sub(before) / 2)
	return server.UTC().Sub(mid).Round(time.Second)
}

// dpopHTU is the proof's target URI: scheme, host and path, no query.
func dpopHTU(u *url.URL) string {
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	scheme := u.Scheme
	switch scheme {
	case "wss":
		scheme = "https"
	case "ws":
		scheme = "http"
	}
	return scheme + "://" + u.Host + path
}

func signDPoPProof(session dpopSession, method string, target *url.URL) (string, error) {
	header, _ := json.Marshal(map[string]any{"alg": "ES256", "typ": "dpop+jwt", "jwk": session.publicJWK})
	tokenDigest := sha256.Sum256([]byte(session.accessToken))
	claims, _ := json.Marshal(map[string]any{
		"jti": util.RandomHex(16), "htm": strings.ToUpper(method), "htu": dpopHTU(target),
		"iat": time.Now().UTC().Add(session.clockSkew).Unix(),
		"ath": base64.RawURLEncoding.EncodeToString(tokenDigest[:]),
	})
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, session.privateKey, digest[:])
	if err != nil {
		return "", err
	}
	signature := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func applyDPoP(header http.Header, session dpopSession, method string, target *url.URL) error {
	proof, err := signDPoPProof(session, method, target)
	if err != nil {
		return err
	}
	header.Set("Authorization", "DPoP "+session.accessToken)
	header.Set("DPoP", proof)
	return nil
}

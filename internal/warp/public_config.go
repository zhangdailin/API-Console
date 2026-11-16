package warp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Pin the public client configuration to a reviewed official source revision.
// Only one project API-key literal is accepted; source code is never executed.
const publicConfigURL = "https://raw.githubusercontent.com/warpdotdev/warp/7a74a2df86ad81d357daa5a25951f7d45ad9ac3b/crates/warp_core/src/channel/config.rs"

var publicKeyLiteral = regexp.MustCompile(`firebase_auth_api_key\s*:\s*"(AIza[A-Za-z0-9_-]{35})"\s*\.into\(\)`)
var publicProjectConfig struct {
	sync.Mutex
	key string
}
var publicProjectLoad singleflight.Group
var errPublicConfig = errors.New("Warp official public project configuration could not be loaded")

func parsePublicProjectConfig(source []byte) (string, error) {
	matches := publicKeyLiteral.FindAllSubmatch(source, 2)
	if len(matches) != 1 {
		return "", errPublicConfig
	}
	return string(matches[0][1]), nil
}

func (c *Client) ensureProjectConfig(ctx context.Context) error {
	if c.firebaseKey != "" {
		return nil
	}
	// Endpoint overrides may receive credentials. Do not discover a production
	// project key for a test/custom endpoint unless explicitly configured.
	if c.base != DefaultBaseURL || c.firebaseBase != "https://identitytoolkit.googleapis.com" || c.tokenBase != "https://securetoken.googleapis.com" {
		return errPublicConfig
	}
	publicProjectConfig.Lock()
	cached := publicProjectConfig.key
	publicProjectConfig.Unlock()
	if cached != "" {
		c.firebaseKey = cached
		return nil
	}
	result := publicProjectLoad.DoChan("official", func() (any, error) {
		publicProjectConfig.Lock()
		cached := publicProjectConfig.key
		publicProjectConfig.Unlock()
		if cached != "" {
			return cached, nil
		}
		loadCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(loadCtx, http.MethodGet, publicConfigURL, nil)
		if err != nil {
			return nil, errPublicConfig
		}
		req.Header.Set("Accept", "text/plain")
		req.Header.Set("User-Agent", "API-Console-Warp-Config")
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, errPublicConfig
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, errPublicConfig
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, (256<<10)+1))
		if err != nil || len(body) > 256<<10 {
			return nil, errPublicConfig
		}
		key, err := parsePublicProjectConfig(body)
		if err != nil {
			return nil, err
		}
		publicProjectConfig.Lock()
		publicProjectConfig.key = key
		publicProjectConfig.Unlock()
		return key, nil
	})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case r := <-result:
		if r.Err != nil {
			return errPublicConfig
		}
		key, ok := r.Val.(string)
		if !ok || strings.TrimSpace(key) == "" {
			return errPublicConfig
		}
		c.firebaseKey = key
		return nil
	}
}

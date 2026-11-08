package grokbase

import (
	"strings"
	"sync"
	"time"
)

// ModelValidationTTL is how long a model that passed validation is trusted
// without asking the store again.
const ModelValidationTTL = 3 * time.Second

// ModelValidationCache remembers, briefly, that a model ID passed validation,
// so a burst of requests checks the catalog once. Both Grok planes use it; the
// zero value is ready to use.
type ModelValidationCache struct {
	mu    sync.RWMutex
	until map[string]time.Time
}

// Has reports whether modelID passed validation within the TTL. An expired
// entry is removed.
func (c *ModelValidationCache) Has(modelID string) bool {
	if strings.TrimSpace(modelID) == "" {
		return false
	}
	c.mu.RLock()
	expiresAt, ok := c.until[modelID]
	c.mu.RUnlock()
	if !ok {
		return false
	}
	if time.Now().Before(expiresAt) {
		return true
	}
	c.mu.Lock()
	if staleAt, ok := c.until[modelID]; ok && !time.Now().Before(staleAt) {
		delete(c.until, modelID)
	}
	c.mu.Unlock()
	return false
}

// Remember records that modelID passed validation now.
func (c *ModelValidationCache) Remember(modelID string) {
	if strings.TrimSpace(modelID) == "" {
		return
	}
	c.mu.Lock()
	if c.until == nil {
		c.until = make(map[string]time.Time)
	}
	c.until[modelID] = time.Now().Add(ModelValidationTTL)
	c.mu.Unlock()
}

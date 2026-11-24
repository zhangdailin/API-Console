package grokbase

import (
	"context"
	"fmt"

	"orchids-api/internal/modelpolicy"
	"orchids-api/internal/store"
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

// EnabledModel looks up the observed route shared by both Grok planes.
func EnabledModel(ctx context.Context, s *store.Store, id string) (*store.Model, error) {
	if s == nil {
		return nil, fmt.Errorf("model not found")
	}
	model, err := s.GetModelByChannelAndModelID(ctx, "grok", id)
	if err != nil || model == nil || !modelpolicy.IsVisibleGrokModel(id, model.Verified) {
		return nil, fmt.Errorf("model not found")
	}
	if !model.Status.Enabled() {
		return nil, fmt.Errorf("model not available")
	}
	return model, nil
}

// UpstreamRoute keeps the catalog's default when there is no operator
// override owned by this plane.
func UpstreamRoute(ctx context.Context, s *store.Store, id, plane, fallback string) string {
	if s != nil && id != "" {
		if row, err := s.GetModelByChannelAndModelID(ctx, "grok", id); err == nil && row != nil &&
			strings.EqualFold(strings.TrimSpace(row.Provider), plane) {
			if upstream := strings.TrimSpace(row.UpstreamModel); upstream != "" {
				return upstream
			}
		}
	}
	return fallback
}

func ModelNotFoundMessage(modelID string) string {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return "The model does not exist or you do not have access to it."
	}
	return fmt.Sprintf("The model `%s` does not exist or you do not have access to it.", modelID)
}

func ModelValidationMessage(modelID string, err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	if strings.EqualFold(msg, "model not found") {
		return ModelNotFoundMessage(modelID)
	}
	return msg
}

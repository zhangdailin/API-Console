package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// unsupportedGrokRoute keeps retired records in Redis for rollback without
// exposing them to model lookup or startup metadata repair.
func unsupportedGrokRoute(m *Model) bool {
	return m != nil && isUnsupportedModelRoute(m.Channel, m.Provider, m.ModelID)
}

func isUnsupportedModelRoute(channel, provider, modelID string) bool {
	return strings.EqualFold(strings.TrimSpace(channel), "grok") &&
		(!SupportsGrokProvider(provider) || strings.HasPrefix(strings.ToLower(strings.TrimSpace(modelID)), "console/"))
}

// Model wrappers

func (s *Store) CreateModel(ctx context.Context, m *Model) error {
	if unsupportedGrokRoute(m) {
		return fmt.Errorf("unsupported Grok route")
	}
	if m != nil {
		m.NormalizeRoute()
	}
	if s.models == nil {
		return fmt.Errorf("models store not configured")
	}
	return s.models.CreateModel(ctx, m)
}

func (s *Store) UpdateModel(ctx context.Context, m *Model) error {
	if unsupportedGrokRoute(m) {
		return fmt.Errorf("unsupported Grok route")
	}
	if m != nil {
		m.NormalizeRoute()
	}
	if s.models == nil {
		return fmt.Errorf("models store not configured")
	}
	return s.models.UpdateModel(ctx, m)
}

func (s *Store) DeleteModel(ctx context.Context, id string) error {
	if s.models != nil {
		return s.models.DeleteModel(ctx, id)
	}
	return fmt.Errorf("models store not configured")
}

func (s *Store) GetModel(ctx context.Context, id string) (*Model, error) {
	if s.models != nil {
		return visibleModel(s.models.GetModel(ctx, id))
	}
	return nil, fmt.Errorf("models store not configured")
}

func (s *Store) GetModelByChannelAndModelID(ctx context.Context, channel, modelID string) (*Model, error) {
	if s.models != nil {
		return visibleModel(s.models.GetModelByChannelAndModelID(ctx, channel, modelID))
	}
	return nil, fmt.Errorf("models store not configured")
}

func visibleModel(m *Model, err error) (*Model, error) {
	if err == nil && unsupportedGrokRoute(m) {
		return nil, ErrNoRows
	}
	return m, err
}

func (s *Store) ReconcileDiscoveredModels(ctx context.Context, channel string, models []*Model, options ModelReconcileOptions) (*ModelReconcileResult, error) {
	if s == nil || s.models == nil {
		return nil, fmt.Errorf("models store not configured")
	}
	for _, m := range models {
		if m != nil && isUnsupportedModelRoute(channel, m.Provider, m.ModelID) {
			return nil, fmt.Errorf("unsupported Grok route")
		}
	}
	return s.models.ReconcileDiscoveredModels(ctx, channel, models, options)
}

func (s *Store) ListModels(ctx context.Context) ([]*Model, error) {
	if s.models != nil {
		models, err := s.models.ListModels(ctx)
		if err != nil {
			return nil, err
		}
		return slices.DeleteFunc(models, unsupportedGrokRoute), nil
	}
	return nil, fmt.Errorf("models store not configured")
}

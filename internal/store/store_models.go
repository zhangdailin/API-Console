package store

import (
	"context"
	"fmt"
)

// Model wrappers

func (s *Store) CreateModel(ctx context.Context, m *Model) error {
	if m != nil {
		m.NormalizeRoute()
	}
	if s.models == nil {
		return fmt.Errorf("models store not configured")
	}
	return s.models.CreateModel(ctx, m)
}

func (s *Store) UpdateModel(ctx context.Context, m *Model) error {
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
		return s.models.GetModel(ctx, id)
	}
	return nil, fmt.Errorf("models store not configured")
}

func (s *Store) GetModelByChannelAndModelID(ctx context.Context, channel, modelID string) (*Model, error) {
	if s.models != nil {
		return s.models.GetModelByChannelAndModelID(ctx, channel, modelID)
	}
	return nil, fmt.Errorf("models store not configured")
}

func (s *Store) ReconcileDiscoveredModels(ctx context.Context, channel string, models []*Model, options ModelReconcileOptions) (*ModelReconcileResult, error) {
	if s == nil || s.models == nil {
		return nil, fmt.Errorf("models store not configured")
	}
	return s.models.ReconcileDiscoveredModels(ctx, channel, models, options)
}

func (s *Store) ListModels(ctx context.Context) ([]*Model, error) {
	if s.models != nil {
		return s.models.ListModels(ctx)
	}
	return nil, fmt.Errorf("models store not configured")
}

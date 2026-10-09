package store

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"encoding/json"

	"github.com/redis/go-redis/v9"
)

// Model wrappers

func (s *redisStore) CreateModel(ctx context.Context, m *Model) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}

	// Use a counter for ID generation to match screenshot style (numeric)
	id, err := s.client.Incr(ctx, s.modelsNextIDKey()).Result()
	if err != nil {
		return err
	}
	m.ID = strconv.FormatInt(id, 10)
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}

	data, err := json.Marshal(m)
	if err != nil {
		return err
	}

	pipe := s.client.Pipeline()
	pipe.Set(ctx, s.modelsKey(m.ID), data, 0)
	pipe.SAdd(ctx, s.modelsIDsKey(), m.ID)
	if strings.TrimSpace(m.ModelID) != "" {
		pipe.HSet(ctx, s.modelsChannelModelIDMapKey(), modelChannelIndexKey(m.Channel, m.ModelID), m.ID)
	}
	_, err = pipe.Exec(ctx)
	return err
}

func (s *redisStore) UpdateModel(ctx context.Context, m *Model) error {
	if m != nil && m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if m.ID == "" {
		return fmt.Errorf("model id is required")
	}

	prev, _ := s.GetModel(ctx, m.ID)

	data, err := json.Marshal(m)
	if err != nil {
		return err
	}

	pipe := s.client.Pipeline()
	pipe.Set(ctx, s.modelsKey(m.ID), data, 0)
	pipe.SAdd(ctx, s.modelsIDsKey(), m.ID)
	if prev != nil && strings.TrimSpace(prev.ModelID) != "" {
		prevKey := modelChannelIndexKey(prev.Channel, prev.ModelID)
		nextKey := modelChannelIndexKey(m.Channel, m.ModelID)
		if prevKey != nextKey {
			pipe.HDel(ctx, s.modelsChannelModelIDMapKey(), prevKey)
		}
	}
	if strings.TrimSpace(m.ModelID) != "" {

		pipe.HSet(ctx, s.modelsChannelModelIDMapKey(), modelChannelIndexKey(m.Channel, m.ModelID), m.ID)
	}
	_, err = pipe.Exec(ctx)
	return err
}

func (s *redisStore) DeleteModel(ctx context.Context, id string) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if id == "" {
		return nil
	}

	// Fetch model to get ModelID for index cleanup
	m, _ := s.GetModel(ctx, id)

	pipe := s.client.Pipeline()
	pipe.Del(ctx, s.modelsKey(id))
	pipe.SRem(ctx, s.modelsIDsKey(), id)
	if m != nil && strings.TrimSpace(m.ModelID) != "" {

		pipe.HDel(ctx, s.modelsChannelModelIDMapKey(), modelChannelIndexKey(m.Channel, m.ModelID))
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (s *redisStore) GetModel(ctx context.Context, id string) (*Model, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	value, err := s.client.Get(ctx, s.modelsKey(id)).Result()
	if err == redis.Nil {
		return nil, ErrNoRows // reuse ErrNoRows for consistency
	}
	if err != nil {
		return nil, err
	}

	var m Model
	if err := json.Unmarshal([]byte(value), &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *redisStore) ListModels(ctx context.Context) ([]*Model, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	values, err := listModelsScript.Run(ctx, s.client, []string{s.modelsIDsKey()}, s.prefix+"models:id:").StringSlice()
	if err != nil {
		return nil, err
	}

	models := make([]*Model, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		var m Model
		if err := json.Unmarshal([]byte(value), &m); err != nil {
			continue
		}
		models = append(models, &m)
	}
	sort.Slice(models, func(i, j int) bool {
		id1, err1 := strconv.Atoi(models[i].ID)
		id2, err2 := strconv.Atoi(models[j].ID)
		if err1 == nil && err2 == nil {
			return id1 < id2
		}
		return models[i].ID < models[j].ID
	})
	return models, nil
}

func (s *redisStore) ReconcileDiscoveredModels(ctx context.Context, channel string, models []*Model, options ModelReconcileOptions) (*ModelReconcileResult, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	channelKey := normalizeModelChannelKey(channel)
	if channelKey == "" {
		return nil, fmt.Errorf("model channel is required")
	}
	now := time.Now().UTC()
	rows := make([]*Model, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, input := range models {
		if input == nil {
			return nil, fmt.Errorf("discovered model is nil")
		}
		modelID := strings.TrimSpace(input.ModelID)
		if modelID == "" {
			return nil, fmt.Errorf("discovered model id is required")
		}
		if _, exists := seen[modelID]; exists {
			return nil, fmt.Errorf("duplicate discovered model id %q", modelID)
		}
		seen[modelID] = struct{}{}
		row := *input
		row.ID = ""
		row.Channel = strings.TrimSpace(channel)
		row.ModelID = modelID
		row.Origin = "discovery"
		if row.CreatedAt.IsZero() {
			row.CreatedAt = now
		}
		row.NormalizeRoute()
		rows = append(rows, &row)
	}
	payload, err := json.Marshal(rows)
	if err != nil {
		return nil, err
	}
	prune := "0"
	if options.Prune {
		prune = "1"
	}
	text, err := reconcileDiscoveredModelsScript.Run(ctx, s.client, []string{
		s.modelsIDsKey(), s.modelsNextIDKey(), s.modelsChannelModelIDMapKey(),
	}, s.prefix+"models:id:", channelKey, prune, payload, strings.ToLower(strings.TrimSpace(options.ProviderScope))).Text()
	if err != nil {
		return nil, err
	}
	var applied struct {
		Added     []string `json:"added"`
		Updated   []string `json:"updated"`
		Deleted   []string `json:"deleted"`
		Protected []string `json:"protected"`
	}
	if err := json.Unmarshal([]byte(text), &applied); err != nil {
		// Redis Lua represents an empty array as an empty object. Decode through
		// RawMessage so production Redis and miniredis have one stable contract.
		var raw map[string]json.RawMessage
		if rawErr := json.Unmarshal([]byte(text), &raw); rawErr != nil {
			return nil, fmt.Errorf("decode model reconciliation result: %w", err)
		}
		decodeIDs := func(key string) []string {
			value := raw[key]
			if len(value) == 0 || string(value) == "{}" || string(value) == "null" {
				return nil
			}
			var ids []string
			_ = json.Unmarshal(value, &ids)
			return ids
		}
		applied.Added, applied.Updated = decodeIDs("added"), decodeIDs("updated")
		applied.Deleted, applied.Protected = decodeIDs("deleted"), decodeIDs("protected")
	}
	for _, ids := range [][]string{applied.Added, applied.Updated, applied.Deleted, applied.Protected} {
		sort.Strings(ids)
	}
	return &ModelReconcileResult{
		Added: len(applied.Added), Updated: len(applied.Updated), Deleted: len(applied.Deleted), Protected: len(applied.Protected),
		AddedModelIDs: applied.Added, UpdatedModelIDs: applied.Updated, DeletedModelIDs: applied.Deleted, ProtectedIDs: applied.Protected,
	}, nil
}

// Helpers

func (s *redisStore) modelsKey(id string) string { return s.prefix + "models:id:" + id }

func (s *redisStore) modelsIDsKey() string { return s.prefix + "models:ids" }

func (s *redisStore) modelsNextIDKey() string { return s.prefix + "models:next_id" }

func (s *redisStore) modelsChannelModelIDMapKey() string {
	return s.prefix + "models:channel_model_id_map"
}

func normalizeModelChannelKey(channel string) string {
	channel = strings.ToLower(strings.TrimSpace(channel))
	if channel == "" {
		return ""
	}
	channel = strings.ReplaceAll(channel, "_", "-")
	channel = strings.ReplaceAll(channel, " ", "-")
	return channel
}

func modelChannelIndexKey(channel, modelID string) string {
	return normalizeModelChannelKey(channel) + "|" + strings.TrimSpace(modelID)
}

func (s *redisStore) GetModelByChannelAndModelID(ctx context.Context, channel, modelID string) (*Model, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil, fmt.Errorf("model not found")
	}

	channelKey := modelChannelIndexKey(channel, modelID)
	id, err := s.client.HGet(ctx, s.modelsChannelModelIDMapKey(), channelKey).Result()
	if err == nil && id != "" {
		m, err := s.GetModel(ctx, id)
		if err == nil && m != nil && strings.TrimSpace(m.ModelID) == modelID && normalizeModelChannelKey(m.Channel) == normalizeModelChannelKey(channel) {
			return m, nil
		}
		// Index stale or points to a different channel/model, fall through to scan.
	}

	models, err := s.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	wantChannel := normalizeModelChannelKey(channel)
	for _, m := range models {
		if normalizeModelChannelKey(m.Channel) == wantChannel && m.ModelID == modelID {
			s.client.HSet(ctx, s.modelsChannelModelIDMapKey(), channelKey, m.ID)
			return m, nil
		}
	}
	return nil, fmt.Errorf("model not found")
}

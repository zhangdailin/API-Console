package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

type CreateKeyResponse struct {
	ID            int64      `json:"id"`
	Key           string     `json:"key"`
	Name          string     `json:"name"`
	KeyPrefix     string     `json:"key_prefix"`
	KeySuffix     string     `json:"key_suffix"`
	Enabled       bool       `json:"enabled"`
	AllowedModels []string   `json:"allowed_models,omitempty"`
	RPMLimit      int        `json:"rpm_limit,omitempty"`
	MaxConcurrent int        `json:"max_concurrent,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	// BillingLimitUSDTicks is the spending cap in USD ticks
	// (1 USD = 10,000,000,000 ticks); zero means unlimited.
	BillingLimitUSDTicks int64 `json:"billing_limit_usd_ticks,omitempty"`
}

// newCreateKeyResponse is the payload both surfaces that hand a secret back use:
// POST /api/keys and POST /api/keys/{id}/rotate.
func newCreateKeyResponse(key *store.ApiKey, fullKey string) CreateKeyResponse {
	return CreateKeyResponse{
		ID:                   key.ID,
		Key:                  fullKey,
		Name:                 key.Name,
		KeyPrefix:            key.KeyPrefix,
		KeySuffix:            key.KeySuffix,
		Enabled:              key.Enabled,
		AllowedModels:        key.AllowedModels,
		RPMLimit:             key.RPMLimit,
		MaxConcurrent:        key.MaxConcurrent,
		ExpiresAt:            key.ExpiresAt,
		CreatedAt:            key.CreatedAt,
		BillingLimitUSDTicks: key.BillingLimitUSDTicks,
	}
}

// maxApiKeyBillingLimitUSDTicks caps an admin-supplied billing limit at
// 9,000,000,000,000,000 ticks (900,000 USD), which keeps the sum of live holds
// and settled usage comfortably inside int64.
const maxApiKeyBillingLimitUSDTicks int64 = 9_000_000_000_000_000

// maxApiKeyBillingPeriodDays bounds the rollover window: a decade is plenty and
// keeps the arithmetic on the stored period start sane.
const maxApiKeyBillingPeriodDays = 3650

// maxApiKeyMaxConcurrent bounds a key's in-flight ceiling: the load balancer
// holds one semaphore per key, so an unbounded value is a self-inflicted stall.
const maxApiKeyMaxConcurrent = 1024

// apiKeyPolicyBounds are the limits the create and the patch surfaces both
// enforce. One table keeps a rejected value from being accepted on one surface
// and refused on the other, and keeps the two surfaces' error text identical.
var apiKeyPolicyBounds = []struct {
	field string
	min   int64
	max   int64
	limit string
}{
	{"rpm_limit", 0, 0, "rpm_limit must be greater than or equal to zero"},
	{"max_concurrent", 0, maxApiKeyMaxConcurrent, "max_concurrent must be between 0 and 1024"},
	{"billing_limit_usd_ticks", 0, maxApiKeyBillingLimitUSDTicks, "billing_limit_usd_ticks must be between 0 and 9000000000000000"},
	{"billing_period_days", 0, maxApiKeyBillingPeriodDays, "billing_period_days must be between 0 and 3650"},
}

// validateApiKeyPolicyValue checks one policy field against the shared bounds.
func validateApiKeyPolicyValue(field string, value int64) error {
	for _, bound := range apiKeyPolicyBounds {
		if bound.field != field {
			continue
		}
		if value < bound.min || (bound.max > 0 && value > bound.max) {
			return errors.New(bound.limit)
		}
	}
	return nil
}

// validateApiKeyPolicy checks the fields a request actually carried, so the
// create and the patch surfaces report the same value the same way.
func validateApiKeyPolicy(w http.ResponseWriter, fields map[string]int64) bool {
	for _, bound := range apiKeyPolicyBounds {
		value, ok := fields[bound.field]
		if !ok {
			continue
		}
		if err := validateApiKeyPolicyValue(bound.field, value); err != nil {
			http.Error(w, bound.limit, http.StatusBadRequest)
			return false
		}
	}
	return true
}

type UpdateKeyRequest struct {
	Enabled              *bool           `json:"enabled"`
	AllowedModels        *[]string       `json:"allowed_models"`
	RPMLimit             *int            `json:"rpm_limit"`
	MaxConcurrent        *int            `json:"max_concurrent"`
	ExpiresAt            json.RawMessage `json:"expires_at"`
	BillingLimitUSDTicks *int64          `json:"billing_limit_usd_ticks"`
	BillingPeriodDays    *int            `json:"billing_period_days"`
}

func normalizeAllowedModels(models []string) []string {
	seen := make(map[string]struct{}, len(models))
	normalized := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.ToLower(strings.TrimSpace(model))
		if model == "" {
			continue
		}
		if model == "*" {
			return []string{"*"}
		}
		if _, ok := seen[model]; ok {
			continue
		}
		seen[model] = struct{}{}
		normalized = append(normalized, model)
	}
	return normalized
}

func parseOptionalExpiry(raw json.RawMessage) (*time.Time, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var expiresAt time.Time
	if err := json.Unmarshal(raw, &expiresAt); err != nil {
		return nil, fmt.Errorf("expires_at must be an RFC3339 timestamp or null")
	}
	expiresAt = expiresAt.UTC()
	return &expiresAt, nil
}

func generateApiKey() (string, error) {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	b := make([]byte, 48)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			return "", err
		}
		b[i] = charset[n.Int64()]
	}
	return "sk-" + string(b), nil
}

func (a *API) HandleKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		keys, err := a.store.ListApiKeys(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		util.WriteJSON(w, keys)

	case http.MethodPost:
		var req struct {
			Name          string     `json:"name"`
			AllowedModels []string   `json:"allowed_models"`
			RPMLimit      int        `json:"rpm_limit"`
			MaxConcurrent int        `json:"max_concurrent"`
			ExpiresAt     *time.Time `json:"expires_at"`
			// Billing limit in USD ticks; zero means unlimited.
			BillingLimitUSDTicks int64 `json:"billing_limit_usd_ticks"`
			// BillingPeriodDays rolls the settled usage over; zero means only an
			// explicit reset clears it.
			BillingPeriodDays int `json:"billing_period_days"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if req.Name == "" {
			http.Error(w, "name is required", http.StatusBadRequest)
			return
		}
		if !validateApiKeyPolicy(w, map[string]int64{
			"rpm_limit":               int64(req.RPMLimit),
			"max_concurrent":          int64(req.MaxConcurrent),
			"billing_limit_usd_ticks": req.BillingLimitUSDTicks,
			"billing_period_days":     int64(req.BillingPeriodDays),
		}) {
			return
		}
		if req.ExpiresAt != nil {
			expiresAt := req.ExpiresAt.UTC()
			if !time.Now().UTC().Before(expiresAt) {
				http.Error(w, "expires_at must be in the future", http.StatusBadRequest)
				return
			}
			req.ExpiresAt = &expiresAt
		}

		fullKey, err := generateApiKey()
		if err != nil {
			slog.Error("Failed to generate api key", "error", err)
			http.Error(w, "failed to generate api key", http.StatusInternalServerError)
			return
		}

		hash := sha256.Sum256([]byte(fullKey))
		hashStr := hex.EncodeToString(hash[:])
		key := store.ApiKey{
			Name:                 req.Name,
			KeyHash:              hashStr,
			KeyFull:              fullKey,
			KeyPrefix:            "sk-",
			KeySuffix:            fullKey[len(fullKey)-4:],
			Enabled:              true,
			AllowedModels:        normalizeAllowedModels(req.AllowedModels),
			RPMLimit:             req.RPMLimit,
			MaxConcurrent:        req.MaxConcurrent,
			ExpiresAt:            req.ExpiresAt,
			BillingLimitUSDTicks: req.BillingLimitUSDTicks,
			BillingPeriodDays:    req.BillingPeriodDays,
		}
		if err := a.store.CreateApiKey(r.Context(), &key); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Cache-Control", "no-store")
		util.WriteJSONStatus(w, http.StatusCreated, newCreateKeyResponse(&key, fullKey))

	default:
		writeMethodNotAllowed(w)
	}
}

// writeApiKeyStoreError reports a store failure on a key the console named.
// A key that is not there is a 404; anything else is the store's own text.
func writeApiKeyStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNoRows) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func (a *API) HandleKeyByID(w http.ResponseWriter, r *http.Request) {
	// A trailing action segment is stripped before the id is parsed, so
	// /api/keys/5/reset-usage and /api/keys/5/rotate reach the branches below
	// instead of a 400.
	trimmedPath := strings.TrimSuffix(r.URL.Path, "/")
	action := ""
	for _, candidate := range []string{"reset-usage", "rotate", "secret"} {
		if strings.HasSuffix(trimmedPath, "/"+candidate) {
			action = candidate
			trimmedPath = strings.TrimSuffix(trimmedPath, "/"+candidate)
			break
		}
	}
	idStr := strings.TrimPrefix(trimmedPath, "/api/keys/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	if action == "secret" {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w)
			return
		}
		secret, err := a.store.GetApiKeySecret(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNoRows) {
				http.Error(w, "not found", 404)
			} else {
				http.Error(w, "API key secret unavailable", http.StatusServiceUnavailable)
			}
			return
		}
		util.WriteJSON(w, map[string]string{"key": secret})
		return
	}
	if action != "" && r.Method != http.MethodPost {
		writeMethodNotAllowed(w)
		return
	}
	switch r.Method {
	case http.MethodPost:
		// POST /api/keys/{id}/reset-usage: start a fresh billing period for this
		// key. Usage settles when a billing period ends; the manual
		// action has to exist too, because a misconfigured limit is otherwise
		// unrecoverable until the period rolls over.
		if action == "reset-usage" {
			key, err := a.store.GetApiKeyByID(r.Context(), id)
			if err != nil {
				writeApiKeyStoreError(w, err)
				return
			}
			if err := a.store.ResetApiKeyBilling(r.Context(), id); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			key.BillingUsedUSDTicks = 0
			key.BillingPeriodStartedAt = time.Now().UTC()
			if err := a.store.UpdateApiKey(r.Context(), key); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			util.WriteJSON(w, key)
			return
		}

		// POST /api/keys/{id}/rotate: mint a new secret for an existing key.
		// Rotation atomically replaces the encrypted secret and authentication hash,
		// retaining policy and usage; the previous secret immediately loses access.
		if action == "rotate" {
			_, err := a.store.GetApiKeyByID(r.Context(), id)
			if err != nil {
				writeApiKeyStoreError(w, err)
				return
			}
			fullKey, err := generateApiKey()
			if err != nil {
				slog.Error("Failed to rotate api key", "error", err)
				http.Error(w, "failed to generate api key", http.StatusInternalServerError)
				return
			}
			key, err := a.store.RotateApiKey(r.Context(), id, fullKey)
			if err != nil {
				writeApiKeyStoreError(w, err)
				return
			}

			w.Header().Set("Cache-Control", "no-store")
			util.WriteJSON(w, newCreateKeyResponse(key, fullKey))
			return
		}

		writeMethodNotAllowed(w)
		return

	case http.MethodPatch:
		var req UpdateKeyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Enabled == nil && req.AllowedModels == nil && req.RPMLimit == nil && req.MaxConcurrent == nil &&
			req.BillingLimitUSDTicks == nil && req.BillingPeriodDays == nil && len(req.ExpiresAt) == 0 {
			http.Error(w, "at least one policy field is required", http.StatusBadRequest)
			return
		}
		key, err := a.store.GetApiKeyByID(r.Context(), id)
		if err != nil {
			writeApiKeyStoreError(w, err)
			return
		}
		if req.Enabled != nil {
			key.Enabled = *req.Enabled
		}
		if req.AllowedModels != nil {
			key.AllowedModels = normalizeAllowedModels(*req.AllowedModels)
		}
		submitted := map[string]int64{}
		if req.RPMLimit != nil {
			submitted["rpm_limit"] = int64(*req.RPMLimit)
		}
		if req.MaxConcurrent != nil {
			submitted["max_concurrent"] = int64(*req.MaxConcurrent)
		}
		if req.BillingLimitUSDTicks != nil {
			submitted["billing_limit_usd_ticks"] = *req.BillingLimitUSDTicks
		}
		if req.BillingPeriodDays != nil {
			submitted["billing_period_days"] = int64(*req.BillingPeriodDays)
		}
		// Every carried field is checked before any of them is applied: a patch
		// must not store the half it accepted.
		if !validateApiKeyPolicy(w, submitted) {
			return
		}
		if req.RPMLimit != nil {
			key.RPMLimit = *req.RPMLimit
		}
		if req.MaxConcurrent != nil {
			key.MaxConcurrent = *req.MaxConcurrent
		}
		if req.BillingLimitUSDTicks != nil {
			key.BillingLimitUSDTicks = *req.BillingLimitUSDTicks
		}
		if req.BillingPeriodDays != nil {
			key.BillingPeriodDays = *req.BillingPeriodDays
		}
		if len(req.ExpiresAt) > 0 {
			expiresAt, err := parseOptionalExpiry(req.ExpiresAt)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if expiresAt != nil && !time.Now().UTC().Before(*expiresAt) {
				http.Error(w, "expires_at must be in the future", http.StatusBadRequest)
				return
			}
			key.ExpiresAt = expiresAt
		}
		fields := map[string]json.RawMessage{}
		add := func(name string, value any) { fields[name], _ = json.Marshal(value) }
		if req.Enabled != nil {
			add("enabled", key.Enabled)
		}
		if req.AllowedModels != nil {
			add("allowed_models", key.AllowedModels)
		}
		if req.RPMLimit != nil {
			add("rpm_limit", key.RPMLimit)
		}
		if req.MaxConcurrent != nil {
			add("max_concurrent", key.MaxConcurrent)
		}
		if req.BillingLimitUSDTicks != nil {
			add("billing_limit_usd_ticks", key.BillingLimitUSDTicks)
		}
		if req.BillingPeriodDays != nil {
			add("billing_period_days", key.BillingPeriodDays)
		}
		if len(req.ExpiresAt) > 0 {
			add("expires_at", key.ExpiresAt)
		}
		if err := a.store.PatchApiKey(r.Context(), id, fields); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		util.WriteJSON(w, key)

	case http.MethodDelete:
		if err := a.store.DeleteApiKey(r.Context(), id); err != nil {
			writeApiKeyStoreError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		writeMethodNotAllowed(w)
	}
}

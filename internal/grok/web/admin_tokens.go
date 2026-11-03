package web

import (
	"strings"

	"orchids-api/internal/store"
)

func adminTokenStatusFromAccount(acc *store.Account) string {
	if acc == nil {
		return "expired"
	}
	status := strings.TrimSpace(acc.StatusCode)
	if status == "" {
		return "active"
	}
	if status == "429" {
		return "cooling"
	}
	return "expired"
}

func inferTokenPool(acc *store.Account) string {
	if acc == nil {
		return "ssoBasic"
	}
	sub := strings.ToLower(strings.TrimSpace(acc.Subscription))
	if strings.Contains(sub, "heavy") {
		return "ssoHeavy"
	}
	if strings.Contains(sub, "super") || strings.Contains(sub, "pro") {
		return "ssoSuper"
	}
	if strings.Contains(sub, "lite") {
		return "ssoLite"
	}
	if InferQuotaLimit(acc) >= superDefaultQuota {
		return "ssoSuper"
	}
	return "ssoBasic"
}

func normalizeGrokPoolName(pool string) string {
	pool = strings.ToLower(strings.TrimSpace(pool))
	switch pool {
	case "ssoheavy", "heavy":
		return "heavy"
	case "ssosuper", "super", "pro":
		return "super"
	case "ssolite", "lite":
		return "lite"
	case "ssobasic", "basic", "":
		return "basic"
	default:
		return pool
	}
}

func normalizeGrokPoolCandidates(pools []string) []string {
	out := make([]string, 0, len(pools))
	seen := map[string]struct{}{}
	for _, raw := range pools {
		pool := normalizeGrokPoolName(raw)
		if _, ok := seen[pool]; ok {
			continue
		}
		seen[pool] = struct{}{}
		out = append(out, pool)
	}
	return out
}

func grokAccountPool(acc *store.Account) string {
	return normalizeGrokPoolName(inferTokenPool(acc))
}

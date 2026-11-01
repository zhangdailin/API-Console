package grok

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
)

// cliSubscriptionTierFromJWT reads a metadata hint, not authentication proof.
// Live subscription metadata always takes precedence over these unsigned claims.
func cliSubscriptionTierFromJWT(token string) string {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Tier json.RawMessage `json:"tier"`
	}
	if json.Unmarshal(payload, &claims) != nil || len(claims.Tier) == 0 {
		return ""
	}
	var tier string
	if json.Unmarshal(claims.Tier, &tier) == nil {
		tier = strings.TrimSpace(tier)
		if n, err := strconv.Atoi(tier); err == nil {
			return cliSubscriptionTierNumber(n)
		}
		// Unknown numeric strings must not become named plans.
		if _, err := strconv.ParseFloat(tier, 64); err == nil {
			return ""
		}
		return tier
	}
	var n float64
	if json.Unmarshal(claims.Tier, &n) != nil || n < 0 || n > 6 || n != float64(int(n)) {
		return ""
	}
	return cliSubscriptionTierNumber(int(n))
}

func cliSubscriptionTierNumber(tier int) string {
	plans := [...]string{"free", "supergrok", "x_basic", "x_premium", "x_premium_plus", "supergrok_heavy", "supergrok_lite"}
	if tier < 0 || tier >= len(plans) {
		return ""
	}
	return plans[tier]
}

package workbuddy

import (
	"encoding/json"
	"strings"
)

// zeroCredits accepts only the upstream decimal multiplier grammar (x0.00 or
// 0x), avoiding float underflow, missing-as-zero, and display-label guesses.
func zeroCredits(raw string) (zero, known bool) {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "x") {
		s = strings.TrimPrefix(s, "x")
	} else if strings.HasSuffix(s, "x") {
		s = strings.TrimSuffix(s, "x")
	} else {
		return false, false
	}
	if s == "" || s[0] < '0' || s[0] > '9' || s[len(s)-1] < '0' || s[len(s)-1] > '9' {
		return false, false
	}
	digits, dots, nonzero := 0, 0, false
	for _, c := range s {
		if c == '.' {
			dots++
			if dots > 1 {
				return false, false
			}
		} else if c >= '0' && c <= '9' {
			digits++
			nonzero = nonzero || c != '0'
		} else {
			return false, false
		}
	}
	return !nonzero && digits > 0, digits > 0
}

// IsFreeModelInCatalog identifies free models solely from the upstream credits
// marker preserved in this account's catalog. Names and promotions do not override it.
func IsFreeModelInCatalog(ids []string, modelID string) bool {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return false
	}
	for _, raw := range ids {
		var row catalogSnapshotRow
		if json.Unmarshal([]byte(raw), &row) != nil || !strings.EqualFold(strings.TrimSpace(row.ID), modelID) {
			continue
		}
		zero, known := zeroCredits(row.Credits)
		if known && zero {
			return true
		}
	}
	return false
}

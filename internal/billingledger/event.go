package billingledger

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Retention bounds receipt storage and automatic settlement replay. Events older
// than this window are rejected, never charged again after their receipt expires.
const Retention = 7 * 24 * time.Hour

func EventID(now time.Time, requestID string) string {
	return fmt.Sprintf("v2:%d:%s", now.Unix(), requestID)
}

func EventExpiry(event string, now time.Time) (time.Time, error) {
	parts := strings.SplitN(event, ":", 3)
	if len(parts) != 3 || parts[0] != "v2" || parts[2] == "" {
		return time.Time{}, fmt.Errorf("invalid billing event identity")
	}
	seconds, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid billing event timestamp")
	}
	created := time.Unix(seconds, 0)
	expiry := created.Add(Retention)
	if created.After(now.Add(5*time.Minute)) || !expiry.After(now) {
		return time.Time{}, fmt.Errorf("billing event outside settlement retention window")
	}
	return expiry, nil
}

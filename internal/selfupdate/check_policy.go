package selfupdate

import (
	"errors"
	"time"
)

const (
	checkCacheTTL = 20 * time.Minute
	checkCooldown = time.Minute
)

func (m *Manager) checkTime() time.Time {
	if m.now != nil {
		return m.now().UTC()
	}
	return time.Now().UTC()
}

// Both browser checks and background discovery share this in-memory policy.
// Force bypasses a successful cache, never a cooldown or upstream retry deadline.
func (m *Manager) recordCheck(release Release, err error) {
	now := m.checkTime()
	m.checkError = err
	m.nextCheckAt = now.Add(checkCooldown)
	if err == nil {
		m.cached = &release
		m.checkedAt = now
		m.checkFailures = 0
		return
	}
	if m.checkFailures < 6 {
		m.checkFailures++
	}
	delay := min(checkCooldown*time.Duration(1<<(m.checkFailures-1)), checkCacheTTL)
	m.nextCheckAt = now.Add(delay)
	var failure *releaseCheckError
	if errors.As(err, &failure) && failure.RetryAt.After(m.nextCheckAt) {
		m.nextCheckAt = failure.RetryAt
	}
}

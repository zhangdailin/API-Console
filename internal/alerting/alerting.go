// Package alerting turns an operations snapshot into alerts and recoveries.
//
// It is deliberately stateless: evaluation is a pure function of the snapshot
// and the previously seen state, so the same inputs always produce the same
// alerts and a restart cannot invent or lose a firing alert. Fired and recovered
// transitions are written to the audit journal as system events, which is what
// makes "a fault and its recovery close the loop" traceable.
package alerting

import (
	"fmt"
	"maps"
	"strings"
	"time"
)

// Severity levels, ordered by urgency.
type Severity string

const (
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// ChannelSnapshot is the per-channel evidence a rule may inspect.
type ChannelSnapshot struct {
	Channel string `json:"channel"`
	// Requests counts the requests in the window that alerting may judge.
	Requests    int64   `json:"requests"`
	Success     int64   `json:"success"`
	Failed      int64   `json:"failed"`
	Samples     int64   `json:"samples"`
	SuccessRate float64 `json:"success_rate"`
	// AccountsEnabled / AccountsAvailable describe the pool for this channel.
	AccountsEnabled   int `json:"accounts_enabled"`
	AccountsAvailable int `json:"accounts_available"`
	// AccountsNeedingLogin counts credentials the upstream refused.
	AccountsNeedingLogin int `json:"accounts_needing_login"`
	// ModelFailures counts per-model throttles currently in effect.
	ModelCooldowns int `json:"model_cooldowns"`
}

// Snapshot is the whole input to one evaluation pass.
type Snapshot struct {
	At       time.Time
	Channels []ChannelSnapshot
}

// Alert is one firing condition.
type Alert struct {
	// Key is stable across evaluations so a firing alert is not re-announced and
	// its recovery can be matched to it.
	Key      string   `json:"key"`
	Severity Severity `json:"severity"`
	Channel  string   `json:"channel,omitempty"`
	Title    string   `json:"title"`
	Detail   string   `json:"detail"`
}

// Transition is what an evaluation pass produced.
type Transition struct {
	Firing    []Alert `json:"firing"`
	Recovered []Alert `json:"recovered"`
}

// Rules are the thresholds. They live in one struct so an operator can see the
// whole alerting policy in one place instead of hunting through the engine.
type Rules struct {
	// MinRequests is how much real traffic a window needs before a success-rate
	// rule is allowed to fire. Without it a single failed request would raise an
	// alert on a quiet channel.
	MinRequests int64 `json:"MinRequests"`
	// MinFailures is how many failures a window needs before the WARNING band is
	// allowed to fire. A ratio alone is not evidence on a quiet channel: one
	// failure out of nine requests is normal operation, and announcing it made
	// the journal alternate between firing and recovered every few minutes.
	// Severe outages (below SuccessRateCritical) are not gated by this: a channel
	// that answers correctly for almost nobody is broken whatever the count.
	MinFailures int64 `json:"MinFailures"`
	// SuccessRateWarning / SuccessRateCritical are ratios (0..1).
	SuccessRateWarning  float64 `json:"SuccessRateWarning"`
	SuccessRateCritical float64 `json:"SuccessRateCritical"`
	// ClearMargin is the hysteresis band: a firing rate alert only clears once the
	// rate is back above the threshold plus this margin. Without it a channel
	// hovering on the line re-announces and re-clears on every evaluation, which
	// is noise an operator learns to ignore.
	ClearMargin float64 `json:"ClearMargin"`
	// RequireNoAvailableAccounts fires when a channel has enabled accounts but
	// none usable.
	RequireNoAvailableAccounts bool `json:"RequireNoAvailableAccounts"`
}

// Validate rejects rule sets that would collapse the warning and critical bands
// or make recovery impossible to reason about.
func (r Rules) Validate() error {
	if r.MinRequests < 0 || r.MinFailures < 0 {
		return fmt.Errorf("request and failure thresholds cannot be negative")
	}
	if r.SuccessRateWarning <= 0 || r.SuccessRateWarning > 1 {
		return fmt.Errorf("warning success rate must be between 0 and 1")
	}
	if r.SuccessRateCritical <= 0 || r.SuccessRateCritical >= r.SuccessRateWarning {
		return fmt.Errorf("critical success rate must be positive and below the warning threshold")
	}
	if r.SuccessRateWarning+r.ClearMargin > 1 {
		return fmt.Errorf("warning threshold plus clear margin cannot exceed 100%%")
	}
	if r.ClearMargin < 0 || r.ClearMargin > 0.5 {
		return fmt.Errorf("clear margin must be between 0 and 0.5")
	}
	return nil
}

// DefaultRules is the shipped policy.
func DefaultRules() Rules {
	return Rules{
		MinRequests:                5,
		MinFailures:                3,
		SuccessRateWarning:         0.9,
		SuccessRateCritical:        0.5,
		ClearMargin:                0.03,
		RequireNoAvailableAccounts: true,
	}
}

// Evaluate compares a snapshot with the previously firing set and returns the
// nonAlertableChannels are aggregates that must never raise an alert.
//
// "http" collects every request whose path matched no provider prefix: the admin
// UI's own redirects, health checks, and whatever a public scanner tries. Its
// failure ratio therefore measures exposure to the internet rather than upstream
// health, so alerting on it meant a page visit could page the operator.
//
// "probe" stays listed for legacy Redis aggregates written by older builds that
// ran the removed synthetic probe loop; no new probe rows are ever recorded.
var nonAlertableChannels = map[string]bool{
	"http":  true,
	"probe": true,
}

// IsAlertableChannel reports whether a channel takes part in alerting.
func IsAlertableChannel(channel string) bool {
	name := strings.ToLower(strings.TrimSpace(channel))
	if name == "" {
		return false
	}
	return !nonAlertableChannels[name]
}

// transitions. previous is keyed by Alert.Key.
func Evaluate(snapshot Snapshot, previous map[string]Alert, rules Rules) Transition {
	if rules.MinRequests <= 0 {
		rules.MinRequests = 1
	}
	if rules.MinFailures <= 0 {
		rules.MinFailures = 1
	}
	current := map[string]Alert{}

	for _, channel := range snapshot.Channels {
		name := strings.TrimSpace(channel.Channel)
		// Infrastructure aggregates are counted in the overview but never alerted
		// on: their failures belong to the internet, not to the upstream, and a
		// public scanner must not be able to raise a channel alert.
		if name == "" || !IsAlertableChannel(name) {
			continue
		}

		// Credential failures are critical: waiting cannot repair them.
		if channel.AccountsNeedingLogin > 0 {
			key := "credential:" + name
			current[key] = Alert{
				Key:      key,
				Severity: SeverityCritical,
				Channel:  name,
				Title:    fmt.Sprintf("%s 有 %d 个账号需要重新登录", name, channel.AccountsNeedingLogin),
				Detail:   "上游拒绝了这些账号的凭据，等待无法恢复，需要重新授权。",
			}
		}

		// Pool exhaustion.
		if rules.RequireNoAvailableAccounts && channel.AccountsEnabled > 0 && channel.AccountsAvailable == 0 {
			key := "pool-empty:" + name
			current[key] = Alert{
				Key:      key,
				Severity: SeverityCritical,
				Channel:  name,
				Title:    fmt.Sprintf("%s 没有可用账号", name),
				Detail:   fmt.Sprintf("%d 个启用账号全部处于冷却/异常状态。", channel.AccountsEnabled),
			}
		}

		// Success rate, only with enough real traffic to mean something.
		if channel.Requests >= rules.MinRequests && channel.Samples > 0 {
			rate := channel.SuccessRate
			key := "success-rate:" + name
			_, alreadyFiring := previous[key]
			margin := rules.ClearMargin
			if margin < 0 {
				margin = 0
			}
			severe := rate < rules.SuccessRateCritical
			// The warning band needs evidence, not just a ratio: fewer failures than
			// MinFailures is noise on a quiet channel. An already-firing alert is
			// exempt so the hysteresis below can still hold it until it truly clears.
			enoughFailures := severe || alreadyFiring || channel.Failed >= rules.MinFailures
			// Hysteresis: while the alert is firing, the rate must climb above the
			// threshold plus the margin before it clears. A channel sitting exactly
			// on the line then neither re-announces nor re-clears every minute.
			held := alreadyFiring && rate < rules.SuccessRateWarning+margin
			switch {
			case severe:
				current[key] = Alert{
					Key:      key,
					Severity: SeverityCritical,
					Channel:  name,
					Title:    fmt.Sprintf("%s 成功率 %.0f%%", name, rate*100),
					Detail:   fmt.Sprintf("窗口内 %d 次请求，失败 %d 次（阈值 <%.0f%%）。", channel.Requests, channel.Failed, rules.SuccessRateCritical*100),
				}
			case enoughFailures && (rate < rules.SuccessRateWarning || held):
				current[key] = Alert{
					Key:      key,
					Severity: SeverityWarning,
					Channel:  name,
					Title:    fmt.Sprintf("%s 成功率 %.0f%%", name, rate*100),
					Detail:   fmt.Sprintf("窗口内 %d 次请求，失败 %d 次（阈值 <%.0f%%，恢复线 %.0f%%）。", channel.Requests, channel.Failed, rules.SuccessRateWarning*100, (rules.SuccessRateWarning+margin)*100),
				}
			}
		}
	}

	transition := Transition{}
	for key, alert := range current {
		if _, wasFiring := previous[key]; wasFiring {
			continue
		}
		transition.Firing = append(transition.Firing, alert)
	}
	for key, alert := range previous {
		if _, stillFiring := current[key]; stillFiring {
			continue
		}
		recovered := alert
		recovered.Detail = "条件已恢复。"
		transition.Recovered = append(transition.Recovered, recovered)
	}
	sortAlerts(transition.Firing)
	sortAlerts(transition.Recovered)
	return transition
}

func sortAlerts(alerts []Alert) {
	rank := func(severity Severity) int {
		switch severity {
		case SeverityCritical:
			return 0
		case SeverityWarning:
			return 1
		default:
			return 2
		}
	}
	for i := 1; i < len(alerts); i++ {
		for j := i; j > 0 && (rank(alerts[j].Severity) < rank(alerts[j-1].Severity) ||
			(rank(alerts[j].Severity) == rank(alerts[j-1].Severity) && alerts[j].Key < alerts[j-1].Key)); j-- {
			alerts[j], alerts[j-1] = alerts[j-1], alerts[j]
		}
	}
}

// Engine keeps the firing set between passes and records transitions.
type Engine struct {
	rules  Rules
	mu     chan struct{}
	firing map[string]Alert
	record func(alert Alert, firing bool)
}

// NewEngine creates an engine that reports transitions through record.
func NewEngine(rules Rules, record func(alert Alert, firing bool)) *Engine {
	engine := &Engine{
		rules:  rules,
		mu:     make(chan struct{}, 1),
		firing: map[string]Alert{},
		record: record,
	}
	engine.mu <- struct{}{}
	return engine
}

// Evaluate runs one pass and records firing/recovery transitions.
func (e *Engine) Evaluate(snapshot Snapshot) Transition {
	if e == nil {
		return Transition{}
	}
	<-e.mu
	previous := e.firing
	transition := Evaluate(snapshot, previous, e.rules)

	// The new firing set is "what was already firing and did not recover" plus
	// the alerts that just started.
	next := make(map[string]Alert, len(previous)+len(transition.Firing))
	maps.Copy(next, previous)
	for _, alert := range transition.Recovered {
		delete(next, alert.Key)
	}
	for _, alert := range transition.Firing {
		next[alert.Key] = alert
	}
	e.firing = next
	record := e.record
	e.mu <- struct{}{}

	if record != nil {
		for _, alert := range transition.Firing {
			record(alert, true)
		}
		for _, alert := range transition.Recovered {
			record(alert, false)
		}
	}
	return transition
}

// Thresholds returns the rules this engine evaluates against.
//
// The rules are an unexported field, so nothing outside the package could read
// them — which left the operations page showing a success-rate percentage with
// no stated target and no way to explain where the number is measured against.
// Exposing them lets the UI say "target 90%" next to the rate instead of leaving
// the reader to guess why 84.6% is a problem.
//
// A nil engine has no rules at all, and a caller that is already nil-safe
// (Firing() is) must not start panicking here: it falls back to DefaultRules so
// the UI always gets the shipped 0.9 rather than a zero that renders as
// "target 0.0%".
func (e *Engine) Thresholds() Rules {
	if e == nil {
		return DefaultRules()
	}
	// Same semaphore dance as Firing(): take the token, copy the value, give it
	// back before returning. Rules are never mutated after NewEngine, but the
	// copy under the token keeps that guarantee independent of future edits.
	<-e.mu
	rules := e.rules
	e.mu <- struct{}{}
	return rules
}

// SetThresholds changes the policy used by subsequent evaluations while keeping
// the current firing set intact. This avoids manufacturing recoveries merely
// because an operator edited a threshold.
func (e *Engine) SetThresholds(rules Rules) error {
	if e == nil {
		return fmt.Errorf("alert engine is unavailable")
	}
	if err := rules.Validate(); err != nil {
		return err
	}
	<-e.mu
	e.rules = rules
	e.mu <- struct{}{}
	return nil
}

// Firing returns a copy of the currently firing alerts.
func (e *Engine) Firing() []Alert {
	if e == nil {
		return nil
	}
	<-e.mu
	out := make([]Alert, 0, len(e.firing))
	for _, alert := range e.firing {
		out = append(out, alert)
	}
	e.mu <- struct{}{}
	sortAlerts(out)
	return out
}

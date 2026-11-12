package doubao

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/provider"
	"orchids-api/internal/store"
)

// The upstream answers some turns with 710022004 and a shark_admin "verify"
// decision: a challenge a person has to solve. The refusal parks the account
// instead of being retried, and the decision is kept so the operator can
// answer it in the console with the upstream's own captcha SDK.
//
// The SDK is handed what the web client hands it: the decision string
// unchanged, and the device id and fingerprint the refused request presented,
// so the answer is bound to the identity the gateway sends. The parameters
// follow doubao2api's captcha page; that answering the challenge lifts the
// restriction for the next request has not been observed here.

// verificationRequired is the refusal that carries a challenge.
type verificationRequired struct {
	code     string
	decision sharkAdminDecision
	// raw is the decision exactly as the upstream sent it, for the SDK.
	raw string
}

func (e *verificationRequired) Error() string {
	// The "requires human verification" wording is what the account policy
	// classifies on: it stops retries and parks the account.
	return fmt.Sprintf("status=403: doubao account requires human verification: open doubao.com with this account and complete the challenge (%s %s)", e.code, e.decision.summary())
}

type sharkAdminDecision struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	From    string `json:"from"`
}

var decisionInText = regexp.MustCompile(`"decision"\s*:\s*"((?:\\.|[^"\\])*)"`)

// sharkDecision reads the decision of a refusal and returns it with its raw
// string. A stream error carries it under extra.decision, as a JSON string or
// as an object.
func sharkDecision(value map[string]any) (sharkAdminDecision, string) {
	extra, _ := value["extra"].(map[string]any)
	var decision sharkAdminDecision
	raw := ""
	switch typed := extra["decision"].(type) {
	case string:
		raw = typed
	case map[string]any:
		if encoded, err := json.Marshal(typed); err == nil {
			raw = string(encoded)
		}
	}
	_ = json.Unmarshal([]byte(raw), &decision)
	return decision, raw
}

// decisionFromText finds a decision anywhere in a non-stream error body, whose
// envelope differs from the stream's.
func decisionFromText(text string) (sharkAdminDecision, string) {
	var decision sharkAdminDecision
	match := decisionInText.FindStringSubmatch(text)
	if len(match) != 2 {
		return decision, ""
	}
	var raw string
	if json.Unmarshal([]byte(`"`+match[1]+`"`), &raw) == nil {
		_ = json.Unmarshal([]byte(raw), &decision)
	}
	return decision, raw
}

func (d sharkAdminDecision) needsVerification() bool {
	return strings.EqualFold(d.From, "shark_admin") && strings.EqualFold(d.Type, "verify")
}

func (d sharkAdminDecision) summary() string {
	parts := make([]string, 0, 3)
	for _, part := range []string{d.From, d.Type, d.Subtype} {
		if strings.TrimSpace(part) != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return "shark_admin"
	}
	return strings.Join(parts, "/")
}

// challengeTTL bounds how long a recorded decision is offered. A stale one
// fails in the SDK; the next refused request records a fresh one.
const challengeTTL = 2 * time.Hour

type recordedChallenge struct {
	raw string
	at  time.Time
}

// challenges holds the last decision per account in process memory only: it
// is short-lived, and it binds nothing a restart needs to keep.
var challenges = struct {
	sync.Mutex
	byAccount map[int64]recordedChallenge
}{byAccount: map[int64]recordedChallenge{}}

// noteChallenge records the decision of a verification refusal.
func noteChallenge(accountID int64, err error) {
	var challenge *verificationRequired
	if accountID == 0 || !errors.As(err, &challenge) || strings.TrimSpace(challenge.raw) == "" {
		return
	}
	challenges.Lock()
	challenges.byAccount[accountID] = recordedChallenge{raw: challenge.raw, at: time.Now()}
	challenges.Unlock()
}

// clearChallenge forgets an account's decision: it answered again, or the
// operator solved the challenge.
func clearChallenge(accountID int64) {
	challenges.Lock()
	delete(challenges.byAccount, accountID)
	challenges.Unlock()
}

// verification returns the account's recorded challenge as the SDK takes it.
// An account without a stored device id sends a fresh one on every request,
// so no answer could be bound to it and none is offered.
func verification(acc *store.Account) (provider.Verification, bool) {
	if acc == nil || acc.ID == 0 || strings.TrimSpace(acc.DoubaoDeviceID) == "" {
		return provider.Verification{}, false
	}
	challenges.Lock()
	recorded, ok := challenges.byAccount[acc.ID]
	if ok && time.Since(recorded.at) > challengeTTL {
		delete(challenges.byAccount, acc.ID)
		ok = false
	}
	challenges.Unlock()
	if !ok {
		return provider.Verification{}, false
	}
	// The fingerprint is the one the chat request presented: s_v_web_id when
	// the session carries it, nothing otherwise.
	fp := ""
	if values := fingerprintQuery(NewFromAccount(acc, nil).jar()); values != nil {
		fp = values.Get("fp")
	}
	return provider.Verification{
		SDK: "bytedance_captcha",
		Params: map[string]any{
			"info": map[string]any{
				"aid": "582478", "appName": "doubao", "lang": "zh",
				"did": acc.DoubaoDeviceID, "fp": fp, "pageId": "27032",
			},
			"host": "https://verify.zijieapi.com",
			"env": map[string]any{
				"h5_check_version": "4.0.16", "product_host": "https://www.doubao.com", "vc_version": "1.0.0.739",
			},
			"verify_data": recorded.raw,
		},
		RecordedAt: recorded.at,
	}, true
}

func verificationSolved(acc *store.Account) {
	if acc != nil {
		clearChallenge(acc.ID)
	}
}

package doubao

import (
	"context"
	"crypto/sha256"
	"log/slog"

	"encoding/json"
	"errors"
	"fmt"
	"orchids-api/internal/accountpolicy"
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
	raw         string
	parkHandled bool
}

func (e *verificationRequired) VerificationParkHandled() bool { return e.parkHandled }

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
	raw             string
	at              time.Time
	did             string
	fp              string
	identity        [sha256.Size]byte
	accountIdentity [sha256.Size]byte
	accountVersion  time.Time
}

// challengeIdentity binds a decision to the persisted request identity, including
// the session and all three identifiers. Missing persisted identifiers make the
// request identity random, so such a decision cannot safely be offered.
func challengeIdentity(acc *store.Account) ([sha256.Size]byte, string, bool) {
	if acc == nil || acc.ID == 0 || strings.TrimSpace(acc.DoubaoDeviceID) == "" || strings.TrimSpace(acc.DoubaoWebID) == "" {
		return [sha256.Size]byte{}, "", false
	}
	fp := fingerprintQuery(decodeCookies(acc.ClientCookie)).Get("fp")
	encoded, _ := json.Marshal([]string{acc.DoubaoDeviceID, acc.DoubaoWebID, acc.DoubaoTeaUUID, acc.ClientCookie})
	return sha256.Sum256(encoded), fp, true
}

// Standalone clients without a store retain their local challenge state.
// Gateway clients and the admin API always use the shared Redis record.
var challenges = struct {
	sync.Mutex
	byAccount map[int64]recordedChallenge
}{byAccount: map[int64]recordedChallenge{}}

func noteChallengeForRequest(acc, request *store.Account, err error) {
	var challenge *verificationRequired
	if !errors.As(err, &challenge) || strings.TrimSpace(challenge.raw) == "" {
		return
	}
	identity, fp, ok := challengeIdentity(request)
	if !ok {
		return
	}
	accountIdentity, _, ok := challengeIdentity(acc)
	if !ok {
		return
	}
	challenges.Lock()
	if previous, exists := challenges.byAccount[acc.ID]; exists && previous.identity != identity && previous.accountVersion.After(acc.UpdatedAt) {
		challenges.Unlock()
		return
	}
	challenges.byAccount[acc.ID] = recordedChallenge{
		raw: challenge.raw, at: time.Now(), did: request.DoubaoDeviceID, fp: fp, identity: identity, accountIdentity: accountIdentity, accountVersion: acc.UpdatedAt,
	}
	challenges.Unlock()
}

func clearChallengeForRequest(acc, request *store.Account) {
	identity, _, ok := challengeIdentity(request)
	if !ok {
		return
	}
	accountIdentity, _, ok := challengeIdentity(acc)
	if !ok {
		return
	}
	challenges.Lock()
	if recorded, found := challenges.byAccount[acc.ID]; found && recorded.identity == identity && recorded.accountIdentity == accountIdentity {
		delete(challenges.byAccount, acc.ID)
	}
	challenges.Unlock()
}

// verification returns the account's recorded challenge as the SDK takes it.
// A rotated identity must first receive its own refusal; it cannot reuse a
// decision from the previous identity. This does not clear the challenge or
// change the account's human-verification status.
func verification(acc *store.Account) (provider.Verification, bool) {
	identity, _, bound := challengeIdentity(acc)
	if !bound {
		return provider.Verification{}, false
	}
	challenges.Lock()
	recorded, ok := challenges.byAccount[acc.ID]
	if ok && time.Since(recorded.at) > challengeTTL {
		delete(challenges.byAccount, acc.ID)
		ok = false
	}
	challenges.Unlock()
	if !ok || recorded.accountIdentity != identity {
		return provider.Verification{}, false
	}
	return verificationPayload(recorded.raw, recorded.did, recorded.fp, "", recorded.at), true
}

func verificationFromStore(ctx context.Context, env provider.Env, acc *store.Account) (provider.Verification, error) {
	challenge, err := env.Store.GetDoubaoVerification(ctx, acc)
	if err != nil {
		return provider.Verification{}, err
	}
	return verificationPayload(challenge.Decision, challenge.DeviceID, challenge.Fingerprint, challenge.ID, challenge.RecordedAt), nil
}

func acknowledgeVerification(ctx context.Context, env provider.Env, acc *store.Account, version string) error {
	return env.Store.AcknowledgeDoubaoVerification(ctx, acc, version)
}

func (c *Client) noteVerification(ctx context.Context, err error) {
	var challenge *verificationRequired
	if !errors.As(err, &challenge) || strings.TrimSpace(challenge.raw) == "" {
		return
	}
	request := c.challengeAccount()
	if c.store == nil {
		noteChallengeForRequest(c.acc, request, err)
		return
	}
	_, fp, valid := challengeIdentity(request)
	if !valid {
		return
	}
	if _, _, valid := challengeIdentity(c.acc); !valid {
		return
	}
	now := time.Now()
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	saveErr := c.store.SaveDoubaoVerification(saveCtx, c.acc.ID, store.DoubaoVerificationChallenge{
		ID: randomUUID(), Decision: challenge.raw, DeviceID: request.DoubaoDeviceID, Fingerprint: fp,
		AccountVersion: store.DoubaoSessionVersion(c.acc), RequestVersion: store.DoubaoSessionVersion(request),
		Message: challenge.Error(), RecordedAt: now, ParkUntil: now.Add(accountpolicy.CooldownVerification),
	})
	if saveErr == nil || errors.Is(saveErr, store.ErrDoubaoVerificationChanged) || errors.Is(saveErr, store.ErrNoRows) {
		// A stale refusal must not fall through to an unbound status update.
		challenge.parkHandled = true
	} else {
		slog.Warn("Doubao verification observation was not saved", "account_id", c.acc.ID, "error", saveErr)
	}
}

func (c *Client) clearVerificationOnSuccess(ctx context.Context) {
	request := c.challengeAccount()
	if c.store == nil {
		clearChallengeForRequest(c.acc, request)
		return
	}
	// Avoid a Redis read on ordinary successful turns. A request that began
	// before a later refusal cannot clear that refusal.
	if !strings.Contains(c.acc.StatusMessage, "requires human verification") {
		return
	}
	challenge, err := c.store.GetDoubaoVerification(ctx, c.acc)
	if err == nil && c.acc.LastAttempt.Equal(challenge.RecordedAt) &&
		challenge.RequestVersion == store.DoubaoSessionVersion(request) {
		_ = c.store.AcknowledgeDoubaoVerification(ctx, c.acc, challenge.ID)
	}
}

func verificationPayload(raw, did, fp, version string, recordedAt time.Time) provider.Verification {
	return provider.Verification{
		SDK: "bytedance_captcha", Version: version,
		Params: map[string]any{
			"info": map[string]any{
				"aid": "582478", "appName": "doubao", "lang": "zh",
				"did": did, "fp": fp, "pageId": "27032",
			},
			"host": "https://verify.zijieapi.com",
			"env": map[string]any{
				"h5_check_version": "4.0.16", "product_host": "https://www.doubao.com", "vc_version": "1.0.0.739",
			},
			"verify_data": raw,
		},
		RecordedAt: recordedAt,
	}
}

func verificationSolved(acc *store.Account) {
	identity, _, ok := challengeIdentity(acc)
	if !ok {
		return
	}
	challenges.Lock()
	if recorded, found := challenges.byAccount[acc.ID]; found && recorded.accountIdentity == identity {
		delete(challenges.byAccount, acc.ID)
	}
	challenges.Unlock()
}

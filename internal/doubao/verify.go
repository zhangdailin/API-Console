package doubao

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/provider"
	"orchids-api/internal/store"
)

// The upstream answers some turns with 710022004 and a shark_admin "verify"
// decision: a challenge a person has to solve. The web client hands that
// decision to its verification SDK, which opens
//
//	https://rmc.bytedance.com/verifycenter/captcha/v2?...&verify_data=<decision>
//
// in an iframe. That page is self-contained: it reads its query, initialises
// the captcha and renders the decision, whether or not it has a parent.
//
// The gateway cannot solve it and does not try. It keeps the last decision per
// account so the console can offer the same official page for the operator to
// open, bound to the device identity the gateway sends.

const (
	// verifyPageHost serves the page; it is the SDK's default iframe host.
	// verifyAPIHost is the service the page talks to, handed to it as the
	// `host` parameter. Opening the page on the API host answers 404.
	verifyPageHost = "https://rmc.bytedance.com"
	verifyAPIHost  = "https://verify.zijieapi.com"
	// webAppID is the Doubao web application id, the `aid` of every request.
	webAppID = "497858"
	// challengeTTL bounds how long a recorded decision is offered. Its token
	// belongs to one refused request and is not expected to stay valid.
	challengeTTL = 30 * time.Minute
)

// verificationRequired is the refusal that carries a challenge.
type verificationRequired struct {
	code     string
	decision sharkAdminDecision
}

func (e *verificationRequired) Error() string {
	// The "requires human verification" wording is what the account policy
	// classifies on: it stops retries and parks the account.
	return fmt.Sprintf("status=403: doubao account requires human verification: open the verification page from the account list, or doubao.com with this account, and complete the challenge (%s %s)", e.code, e.decision.summary())
}

var challenges = struct {
	sync.Mutex
	byAccount    map[int64]recordedChallenge
	fingerprints map[int64]string
}{byAccount: map[int64]recordedChallenge{}, fingerprints: map[int64]string{}}

type recordedChallenge struct {
	decision sharkAdminDecision
	at       time.Time
}

// noteChallenge records the challenge an upstream refusal carried, if any.
func noteChallenge(accountID int64, err error) {
	var required *verificationRequired
	if accountID == 0 || !errors.As(err, &required) {
		return
	}
	challenges.Lock()
	challenges.byAccount[accountID] = recordedChallenge{decision: required.decision, at: time.Now()}
	challenges.Unlock()
}

// clearChallenge forgets the challenge once the account answers again.
func clearChallenge(accountID int64) {
	challenges.Lock()
	delete(challenges.byAccount, accountID)
	challenges.Unlock()
}

// verification returns the official page for the account's pending challenge.
// It reports false when no challenge was recorded since the process started,
// when the recorded one is too old to offer, or when the account has no stored
// device identity for the page to be bound to.
func verification(acc *store.Account) (provider.Verification, bool) {
	if acc == nil || acc.DoubaoDeviceID == "" {
		return provider.Verification{}, false
	}
	challenges.Lock()
	recorded, ok := challenges.byAccount[acc.ID]
	challenges.Unlock()
	if !ok || time.Since(recorded.at) > challengeTTL {
		return provider.Verification{}, false
	}
	return provider.Verification{
		URL:        verificationURL(acc.DoubaoDeviceID, verifyFingerprint(acc), recorded.decision),
		Kind:       recorded.decision.summary(),
		ObservedAt: recorded.at,
	}, true
}

// verifyFingerprint returns the fp the verification page is opened with. The
// captcha service refuses a challenge without one ("参数错误[5005]"). A session
// exported from a browser carries it as the s_v_web_id cookie; a QR-login
// session has none, so one is minted the way the SDK mints it for a first
// visit and kept for the life of the process, so every link for the account
// names the same browser.
func verifyFingerprint(acc *store.Account) string {
	if fp := strings.TrimSpace(decodeCookies(acc.ClientCookie)["s_v_web_id"]); fp != "" {
		return fp
	}
	challenges.Lock()
	defer challenges.Unlock()
	if fp := challenges.fingerprints[acc.ID]; fp != "" {
		return fp
	}
	fp := newVerifyFingerprint(time.Now())
	challenges.fingerprints[acc.ID] = fp
	return fp
}

// newVerifyFingerprint mints an fp in the SDK's format: "verify_", the
// millisecond clock in base 36, and a 36-character UUID-shaped random part
// drawn from [0-9A-Za-z] with "_" separators.
func newVerifyFingerprint(now time.Time) string {
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	var seed [36]byte
	_, _ = rand.Read(seed[:])
	part := make([]byte, 36)
	for index := range part {
		pick := int(seed[index]) % len(alphabet)
		switch index {
		case 8, 13, 18, 23:
			part[index] = '_'
		case 14:
			part[index] = '4'
		case 19:
			part[index] = alphabet[pick&3|8]
		default:
			part[index] = alphabet[pick]
		}
	}
	return "verify_" + strconv.FormatInt(now.UnixMilli(), 36) + "_" + string(part)
}

// verificationURL renders the page the web client's SDK opens for a decision.
// The parameters and the percent-encoding follow the SDK, which builds the
// query with encodeURIComponent and adds the browser fingerprint (the
// s_v_web_id cookie) as fp.
func verificationURL(deviceID, fingerprint string, decision sharkAdminDecision) string {
	data, _ := json.Marshal(struct {
		Subtype     string `json:"subtype"`
		Detail      string `json:"detail"`
		LogID       string `json:"log_id"`
		VerifyScene string `json:"verify_scene"`
		LoginStatus int    `json:"login_status"`
	}{decision.Subtype, decision.Detail, decision.LogID, decision.VerifyScene, 1})
	params := [][2]string{
		{"from", "iframe"}, {"fp", fingerprint},
		{"aid", webAppID}, {"did", deviceID}, {"scene_level", "p2"}, {"app_name", "doubao"},
		{"host", verifyAPIHost}, {"lang", "zh"}, {"viewport", "1280x800"}, {"baseEM", "16"},
		{"verify_data", string(data)},
	}
	var query strings.Builder
	for _, param := range params {
		if param[1] == "" {
			continue
		}
		if query.Len() > 0 {
			query.WriteByte('&')
		}
		query.WriteString(param[0])
		query.WriteByte('=')
		query.WriteString(strings.ReplaceAll(url.QueryEscape(param[1]), "+", "%20"))
	}
	return verifyPageHost + "/verifycenter/captcha/v2?" + query.String()
}

type sharkAdminDecision struct {
	Type        string `json:"type"`
	Subtype     string `json:"subtype"`
	From        string `json:"from"`
	Detail      string `json:"detail"`
	LogID       string `json:"log_id"`
	VerifyScene string `json:"verify_scene"`
}

var decisionInText = regexp.MustCompile(`"decision"\s*:\s*"((?:\\.|[^"\\])*)"`)

// sharkDecision reads the decision of a refusal. A stream error carries it
// under extra.decision, as a JSON string or as an object.
func sharkDecision(value map[string]any) sharkAdminDecision {
	extra, _ := value["extra"].(map[string]any)
	var decision sharkAdminDecision
	switch raw := extra["decision"].(type) {
	case string:
		_ = json.Unmarshal([]byte(raw), &decision)
	case map[string]any:
		if encoded, err := json.Marshal(raw); err == nil {
			_ = json.Unmarshal(encoded, &decision)
		}
	}
	return decision
}

// decisionFromText finds a decision anywhere in a non-stream error body, whose
// envelope differs from the stream's.
func decisionFromText(text string) sharkAdminDecision {
	var decision sharkAdminDecision
	match := decisionInText.FindStringSubmatch(text)
	if len(match) != 2 {
		return decision
	}
	var raw string
	if json.Unmarshal([]byte(`"`+match[1]+`"`), &raw) == nil {
		_ = json.Unmarshal([]byte(raw), &decision)
	}
	return decision
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

package doubao

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// The upstream answers some turns with 710022004 and a shark_admin "verify"
// decision: a challenge a person has to solve in the web client. The gateway
// cannot solve it and offers no page for it: the web client delivers the
// result of its embedded challenge back into the refused request, which a
// page opened on its own has no way to do. The refusal is only recognised
// here, so the account is parked instead of retried.

// verificationRequired is the refusal that carries a challenge.
type verificationRequired struct {
	code     string
	decision sharkAdminDecision
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

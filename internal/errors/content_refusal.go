package errors

import "strings"

// IsContentPolicyRefusal recognizes explicit request-level safety refusals.
// A permission error alone is not evidence of a content refusal.
func IsContentPolicyRefusal(text string) bool {
	text = strings.ReplaceAll(strings.ToLower(text), "’", "'")
	for _, marker := range []string{
		"content policy rejected", "content_policy_violation", "datainspectionfailed",
		"input text data may contain", "i can't help with that request",
		"i cannot help with that request",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

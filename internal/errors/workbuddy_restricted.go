package errors

import (
	"regexp"
	"strings"
)

var workBuddyRestrictedCode = regexp.MustCompile(`(?:^|[ ,(])code=11140(?:$|[ ,)])`)

// IsWorkBuddyRestricted recognises the account restriction, not an arbitrary
// forbidden response or a similarly numbered business error.
func IsWorkBuddyRestricted(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "workbuddy") && HasExplicitHTTPStatus(lower, "403") && workBuddyRestrictedCode.MatchString(lower)
}

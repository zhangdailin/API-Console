package modelpolicy

import "strings"

// providerPublicPrefixes are the accepted Build qualifiers.
var providerPublicPrefixes = []string{"build/"}

// ExternalPublicID is the model name clients see.
func ExternalPublicID(internalID string) string {
	id := strings.ToLower(strings.TrimSpace(internalID))
	for _, prefix := range providerPublicPrefixes {
		if strings.HasPrefix(id, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(id, prefix))
		}
	}
	return id
}

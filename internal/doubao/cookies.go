package doubao

import (
	"encoding/json"
	"net/http"
	"strings"
)

type cookieJar map[string]string

func decodeCookies(raw string) cookieJar {
	jar := cookieJar{}
	_ = json.Unmarshal([]byte(raw), &jar)
	return jar
}

func (j cookieJar) encode() string { raw, _ := json.Marshal(j); return string(raw) }

func (j cookieJar) header(excludePassportCSRF bool) string {
	parts := make([]string, 0, len(j))
	for key, value := range j {
		// The passport SDK also sets passport_csrf_token_default. Both names are
		// rejected with 2035 when they reach the non-passport endpoints, so the
		// whole prefix is filtered instead of only the exact cookie name.
		if excludePassportCSRF && strings.HasPrefix(strings.ToLower(key), "passport_csrf_token") {
			continue
		}
		if strings.TrimSpace(key) != "" {
			parts = append(parts, key+"="+value)
		}
	}
	return strings.Join(parts, "; ")
}

func (j cookieJar) absorb(response *http.Response) {
	if response == nil {
		return
	}
	for _, cookie := range response.Cookies() {
		if cookie.Name != "" {
			j[cookie.Name] = cookie.Value
		}
	}
}

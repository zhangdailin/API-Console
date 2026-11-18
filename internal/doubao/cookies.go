package doubao

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"time"
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
		if cookie.Name == "" || (cookie.Path != "" && cookie.Path != "/") {
			continue
		}
		if cookie.Path == "" && response.Request != nil && response.Request.URL != nil && strings.LastIndex(response.Request.URL.Path, "/") > 0 {
			// An omitted Path defaults to the request directory, not '/'.
			continue
		}
		// The legacy account format stores site-wide cookies only. Do not
		// broaden a path-specific cookie, or accept an unrelated Domain.
		if response.Request != nil && response.Request.URL != nil && cookie.Domain != "" {
			u := *response.Request.URL
			u.Path = "/"
			probe, _ := cookiejar.New(nil)
			validation := *cookie
			validation.MaxAge, validation.Expires = 0, time.Time{}
			probe.SetCookies(&u, []*http.Cookie{&validation})
			if len(probe.Cookies(&u)) == 0 {
				continue
			}
		}
		if cookie.MaxAge < 0 || (!cookie.Expires.IsZero() && cookie.Expires.Before(time.Now())) {
			delete(j, cookie.Name)
			continue
		}
		j[cookie.Name] = cookie.Value
	}
}

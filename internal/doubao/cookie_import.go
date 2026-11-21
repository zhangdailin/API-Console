package doubao

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/provider"
	"orchids-api/internal/store"
)

var errInvalidBrowserCookie = errors.New("invalid Doubao browser Cookie")

func importBrowserCookie(ctx context.Context, env provider.Env, raw string) (*store.Account, error) {
	jar, err := parseBrowserCookies(raw)
	if err != nil {
		return nil, err
	}
	device, web, tea, err := NewIdentity()
	if err != nil {
		return nil, err
	}
	acc := &store.Account{AccountType: "doubao", ClientCookie: jar.encode(), Enabled: true, Weight: 1,
		DoubaoDeviceID: device, DoubaoWebID: web, DoubaoTeaUUID: tea}
	info, err := NewFromAccount(acc, env.Config).AccountInfo(ctx, jar)
	if err != nil {
		return nil, err
	}
	info.apply(acc, time.Now())
	acc.Name = first(info.Name, info.UserID)
	acc.ClientCookie = jar.encode()
	return acc, nil
}

func parseBrowserCookies(raw string) (cookieJar, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 32<<10 {
		return nil, errInvalidBrowserCookie
	}
	jar := cookieJar{}
	add := func(name, value string) error {
		if name == "" || strings.ContainsAny(value, ";,\"\\") || strings.ContainsAny(name+value, "\r\n") {
			return errInvalidBrowserCookie
		}
		cookie := http.Cookie{Name: name, Value: value}
		if cookie.Valid() != nil {
			return errInvalidBrowserCookie
		}
		if previous, ok := jar[name]; ok && previous != value {
			return errInvalidBrowserCookie
		}
		jar[name] = value
		return nil
	}
	switch raw[0] {
	case '{':
		var values map[string]string
		if json.Unmarshal([]byte(raw), &values) != nil {
			return nil, errInvalidBrowserCookie
		}
		for name, value := range values {
			if err := add(name, value); err != nil {
				return nil, err
			}
		}
	case '[':
		var values []struct {
			Name, Value, Domain, Path string
			Expires                   float64
			ExpirationDate            float64
		}
		if json.Unmarshal([]byte(raw), &values) != nil {
			return nil, errInvalidBrowserCookie
		}
		now := float64(time.Now().Unix())
		for _, entry := range values {
			domain := strings.TrimPrefix(strings.ToLower(entry.Domain), ".")
			if domain != "doubao.com" && domain != "www.doubao.com" && domain != "accounts.doubao.com" {
				continue
			}
			if entry.Path != "" && entry.Path != "/" || entry.Expires > 0 && entry.Expires < now || entry.ExpirationDate > 0 && entry.ExpirationDate < now {
				continue
			}
			if err := add(entry.Name, entry.Value); err != nil {
				return nil, err
			}
		}
	default:
		values, err := http.ParseCookie(raw)
		if err != nil {
			return nil, errInvalidBrowserCookie
		}
		for _, entry := range values {
			if err := add(entry.Name, entry.Value); err != nil {
				return nil, err
			}
		}
	}
	if len(jar) == 0 || len(jar) > 96 {
		return nil, errInvalidBrowserCookie
	}
	return jar, nil
}

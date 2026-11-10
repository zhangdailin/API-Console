package doubao

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"orchids-api/internal/config"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
)

var (
	ErrLoginPending     = errors.New("doubao QR login is pending")
	ErrLoginUnavailable = errors.New("doubao login endpoint is unreachable")
	ErrLoginRejected    = errors.New("doubao QR login was rejected")
)

type qrLogin struct {
	client                   *Client
	jar                      cookieJar
	token                    string
	base                     string
	target                   string
	expires                  time.Time
	deviceID, webID, teaUUID string
}

func (q *qrLogin) LoginPayload() (string, string, time.Time) {
	if q == nil {
		return "", "", time.Time{}
	}
	png, err := qrcode.Encode(q.target, qrcode.Medium, 360)
	if err != nil {
		return "", q.target, q.expires
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), q.target, q.expires
}

func loginPort() *provider.Login {
	return &provider.Login{UnreachableDetail: "Doubao login endpoint is unreachable", ErrPending: ErrLoginPending, ErrUnavailable: ErrLoginUnavailable, ErrRejected: ErrLoginRejected, BuildVerifies: true,
		Start: startLogin, Exchange: exchangeLogin, Build: buildLogin,
	}
}

func startLogin(ctx context.Context, cfg *config.Config) (provider.LoginChallenge, error) {
	client := NewFromAccount(nil, cfg)
	jar := cookieJar{}
	// The landing-page visit establishes ttwid before passport requests.
	if response, err := client.do(ctx, http.MethodGet, client.baseURL()+"/chat/", nil, jar, false); err == nil {
		jar.absorb(response)
		response.Body.Close()
	}
	qr, err := fetchQR(ctx, client, jar)
	if err != nil {
		return provider.LoginChallenge{}, fmt.Errorf("%w: %v", ErrLoginUnavailable, err)
	}
	deviceID, webID, teaUUID, err := NewIdentity()
	if err != nil {
		return provider.LoginChallenge{}, err
	}
	png, err := qrcode.Encode(qr.target, qrcode.Medium, 360)
	if err != nil {
		return provider.LoginChallenge{}, err
	}
	state := &qrLogin{client: client, jar: jar, token: qr.token, base: qr.base, target: qr.target, expires: qr.expires, deviceID: deviceID, webID: webID, teaUUID: teaUUID}
	return provider.LoginChallenge{DeviceCode: qr.token, VerifyURI: qr.target, VerifyFull: qr.target, QRCode: "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), ExpiresAt: qr.expires, Interval: 1300 * time.Millisecond, Private: state}, nil
}

type qrResponse struct {
	base, token, target string
	expires             time.Time
}

func fetchQR(ctx context.Context, client *Client, jar cookieJar) (qrResponse, error) {
	for _, base := range []string{client.accountsURL(), client.baseURL()} {
		query := passportQuery("")
		response, err := client.do(ctx, http.MethodGet, base+"/passport/web/get_qrcode/?"+query.Encode(), nil, jar, true)
		if err != nil {
			continue
		}
		jar.absorb(response)
		var payload struct {
			Data struct {
				ErrorCode  int    `json:"error_code"`
				Token      string `json:"token"`
				IndexURL   string `json:"qrcode_index_url"`
				ExpireTime int64  `json:"expire_time"`
				ExpireIn   int64  `json:"expire_time_in"`
			} `json:"data"`
		}
		_ = json.NewDecoder(response.Body).Decode(&payload)
		response.Body.Close()
		if payload.Data.ErrorCode == 0 && payload.Data.Token != "" && payload.Data.IndexURL != "" {
			expiry := time.Unix(payload.Data.ExpireTime, 0)
			if payload.Data.ExpireTime < 1_000_000_000 {
				expiry = time.Now().Add(time.Duration(payload.Data.ExpireTime) * time.Second)
			}
			if payload.Data.ExpireIn > 0 {
				expiry = time.Now().Add(time.Duration(payload.Data.ExpireIn) * time.Second)
			}
			if expiry.Before(time.Now()) {
				expiry = time.Now().Add(2 * time.Minute)
			}
			return qrResponse{base: base, token: payload.Data.Token, target: payload.Data.IndexURL, expires: expiry}, nil
		}
	}
	return qrResponse{}, errors.New("no QR code returned")
}

func passportQuery(token string) url.Values {
	query := url.Values{"next": {"https://www.doubao.com"}, "passport_jssdk_version": {"4.4.6"}, "passport_jssdk_type": {"normal"}, "is_from_ttaccountsdk": {"1"}, "aid": {"497858"}, "language": {"zh"}, "account_app_language": {"zh-CN"}, "multi_login": {"1"}, "account_sdk_source": {"web"}, "p_ca": {"4.0.26"}, "p_ca_real": {"1.0.0.1003"}, "device_platform": {"web"}, "os_type": {"1"}, "terminal_type": {"2"}, "ts": {fmt.Sprint(time.Now().Unix())}, "biz_trace_id": {randomHex(4)}}
	if token != "" {
		query.Set("token", token)
	}
	return query
}

func exchangeLogin(ctx context.Context, _ *config.Config, challenge provider.LoginChallenge) (provider.LoginGrant, error) {
	state, ok := challenge.Private.(*qrLogin)
	if !ok || state == nil {
		return nil, ErrLoginRejected
	}
	if time.Now().After(state.expires) {
		fresh, err := fetchQR(ctx, state.client, state.jar)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrLoginUnavailable, err)
		}
		state.token, state.base, state.expires, state.target = fresh.token, fresh.base, fresh.expires, fresh.target
		return nil, ErrLoginPending
	}
	response, err := state.client.do(ctx, http.MethodGet, state.base+"/passport/web/check_qrconnect/?"+passportQuery(state.token).Encode(), nil, state.jar, true)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLoginUnavailable, err)
	}
	defer response.Body.Close()
	state.jar.absorb(response)
	var payload struct {
		Data struct {
			ErrorCode   int             `json:"error_code"`
			Status      any             `json:"status"`
			UserData    json.RawMessage `json:"user_data"`
			RedirectURL string          `json:"redirect_url"`
		} `json:"data"`
	}
	_ = json.NewDecoder(response.Body).Decode(&payload)
	if payload.Data.ErrorCode != 0 {
		return nil, ErrLoginPending
	}
	status := strings.ToLower(fmt.Sprint(payload.Data.Status))
	if status != "confirmed" && status != "success" && status != "3" {
		return nil, ErrLoginPending
	}
	for _, target := range []string{payload.Data.RedirectURL, state.client.baseURL() + "/chat/?from_login=1"} {
		if strings.TrimSpace(target) == "" {
			continue
		}
		if reply, err := state.client.do(ctx, http.MethodGet, target, nil, state.jar, false); err == nil {
			state.jar.absorb(reply)
			reply.Body.Close()
		}
	}
	info, err := state.client.AccountInfo(ctx, state.jar)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLoginRejected, err)
	}
	return loginGrant{state: state, info: info}, nil
}

type loginGrant struct {
	state *qrLogin
	info  AccountInfo
}

func buildLogin(_ context.Context, _ *config.Config, _ string, _ provider.LoginChallenge, grant provider.LoginGrant) (*store.Account, error) {
	value, ok := grant.(loginGrant)
	if !ok {
		return nil, ErrLoginRejected
	}
	return &store.Account{Name: first(value.info.Name, value.info.UserID), AccountType: "doubao", ClientCookie: value.state.jar.encode(), UserID: value.info.UserID, DoubaoDeviceID: value.state.deviceID, DoubaoWebID: value.state.webID, DoubaoTeaUUID: value.state.teaUUID, Enabled: true, Weight: 1, DoubaoVerifiedAt: time.Now(), VerifiedAt: time.Now()}, nil
}

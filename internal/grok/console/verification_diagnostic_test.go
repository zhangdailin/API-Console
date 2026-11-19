package console

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

type diagnosticTransport func(*http.Request) (*http.Response, error)

func (f diagnosticTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func diagnosticResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func TestVerificationFailureClassificationsAreCredentialFree(t *testing.T) {
	for _, tt := range []struct {
		name, reason, stage string
		err                 error
		status              int
	}{
		{"model access", "console_model_access_denied", "models", &StatusError{Status: 403, Message: "synthetic-secret"}, 403},
		{"key discovery", "console_api_key_unavailable", "api_key", withAPIKeyDiagnostic(&StatusError{Status: 403}, errors.New("synthetic-secret")), 403},
		{"key SSO", "sso_rejected", "api_key", withAPIKeyDiagnostic(&StatusError{Status: 403}, ErrUnauthorized), 403},
		{"SSO", "sso_rejected", "models", ErrUnauthorized, 401},
		{"DPoP", "console_auth_failed", "dpop", &verificationStageError{stage: "dpop", err: &StatusError{Status: 403, Message: "synthetic-secret"}}, 403},
		{"rate", "console_rate_limited", "models", &StatusError{Status: 429}, 429},
		{"unavailable", "console_upstream_unavailable", "models", &StatusError{Status: 502}, 502},
		{"timeout", "timeout", "models", context.DeadlineExceeded, 0},
		{"unknown", "console_verification_failed", "models", errors.New("synthetic-secret"), 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := DiagnoseVerification(tt.err)
			if got.Reason != tt.reason || got.Stage != tt.stage || got.HTTPStatus != tt.status || got.Message == "" {
				t.Fatalf("unexpected diagnostic: %+v", got)
			}
			encoded, _ := json.Marshal(got)
			if strings.Contains(string(encoded), "synthetic-secret") {
				t.Fatal("diagnostic exposed raw upstream credentials or prose")
			}
		})
	}
}

func TestVerificationPreservesKeyDiscoveryFailureWithoutAdditionalRequests(t *testing.T) {
	for _, missingKey := range []bool{false, true} {
		t.Run(map[bool]string{false: "key obtained", true: "key discovery failed"}[missingKey], func(t *testing.T) {
			f := newFakeConsole(t)
			client := NewClient(&config.Config{GrokConsoleBaseURL: f.server.URL})
			base := f.server.Client().Transport
			modelCalls, createCalls := 0, 0
			client.httpClient = &http.Client{Transport: diagnosticTransport(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/":
					if missingKey {
						return diagnosticResponse(r, 403, `{"error":"synthetic-key-discovery-secret"}`), nil
					}
				case createAPIKeyPath:
					createCalls++
					t.Error("diagnostic unexpectedly created an API key")
				case "/v1/models":
					modelCalls++
					if (r.Header.Get(apiKeyHeader) == "") != missingKey {
						t.Error("request key behavior changed")
					}
					return diagnosticResponse(r, 403, `{"error":"Access to the models endpoint is denied. synthetic-upstream-secret"}`), nil
				}
				return base.RoundTrip(r)
			})}
			acc := &store.Account{Name: "source", AccountType: "grok", GrokProvider: ProviderConsole, ClientCookie: testCookie}
			before := *acc
			_, err := verifyWith(context.Background(), client, acc)
			var upstream *StatusError
			if !errors.As(err, &upstream) || upstream.Status != 403 {
				t.Fatalf("upstream error chain lost: %v", err)
			}
			got := DiagnoseVerification(err)
			want := "console_model_access_denied"
			if missingKey {
				want = "console_api_key_unavailable"
			}
			if got.Reason != want || modelCalls != 1 || createCalls != 0 || !reflect.DeepEqual(before, *acc) {
				t.Fatalf("diagnostic changed behavior: %+v modelCalls=%d createCalls=%d", got, modelCalls, createCalls)
			}
		})
	}
}

func TestKeyDiscoveryWarningDoesNotRejectSuccessfulUpstream(t *testing.T) {
	f := newFakeConsole(t)
	client := NewClient(&config.Config{GrokConsoleBaseURL: f.server.URL})
	base := f.server.Client().Transport
	client.httpClient = &http.Client{Transport: diagnosticTransport(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/":
			return diagnosticResponse(r, 403, "home refused"), nil
		case "/v1/models":
			return diagnosticResponse(r, 200, `{"data":[{"id":"grok-test"}]}`), nil
		}
		return base.RoundTrip(r)
	})}
	models, err := client.FetchModels(context.Background(), testCookie)
	if err != nil || len(models) != 1 || models[0] != "grok-test" {
		t.Fatalf("diagnostic rejected an existing successful path: %v %v", models, err)
	}
}

func TestDPoPFailureIsNotReportedAsModelPermissionFailure(t *testing.T) {
	f := newFakeConsole(t)
	client := NewClient(&config.Config{GrokConsoleBaseURL: f.server.URL})
	base := f.server.Client().Transport
	modelCalls := 0
	client.httpClient = &http.Client{Transport: diagnosticTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v1/dpop/token" {
			return diagnosticResponse(r, 403, `{"error":"synthetic-dpop-secret"}`), nil
		}
		if r.URL.Path == "/v1/models" {
			modelCalls++
		}
		return base.RoundTrip(r)
	})}
	_, err := client.FetchModels(context.Background(), testCookie)
	got := DiagnoseVerification(err)
	if got.Reason != "console_auth_failed" || got.Stage != "dpop" || modelCalls != 0 {
		t.Fatalf("DPoP failure misclassified: %+v requests=%d", got, modelCalls)
	}
}

func TestAPIKeyRepairFailureRemainsVisible(t *testing.T) {
	f := newFakeConsole(t)
	client := NewClient(&config.Config{GrokConsoleBaseURL: f.server.URL})
	client.apiKeys[sessionKey(testCookie)] = testAPIKeyID
	base := f.server.Client().Transport
	modelCalls := 0
	client.httpClient = &http.Client{Transport: diagnosticTransport(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/":
			return diagnosticResponse(r, 403, "repair refused"), nil
		case "/v1/models":
			modelCalls++
			return diagnosticResponse(r, 403, `{"error":"not the owner"}`), nil
		}
		return base.RoundTrip(r)
	})}
	_, err := client.FetchModels(context.Background(), testCookie)
	got := DiagnoseVerification(err)
	if got.Reason != "console_api_key_unavailable" || got.Stage != "api_key" || got.HTTPStatus != 403 || modelCalls != 1 {
		t.Fatalf("key repair failure was lost: %+v requests=%d", got, modelCalls)
	}
}

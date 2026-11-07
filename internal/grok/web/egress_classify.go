package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"orchids-api/internal/grok/grokbase"
	"strings"
)

// 403/429 classification helpers, ported from grok2api account_block.go plus
// Cloudflare challenge detection. The unified entry point ClassifyUpstreamResponse
// is response-aware (status + headers + body) so header-only signals such as
// CF-Mitigated and WWW-Authenticate are not lost. Precedence:
//
//	DPoP challenge > Cloudflare challenge > explicit account block > 429 > generic forbidden
//
// A generic 403 is deliberately NOT treated as an account block; only explicit
// "blocked-user"/"user is blocked" language marks the account.

// UpstreamErrorKind categorizes an upstream failure for account/egress handling.
type UpstreamErrorKind int

const (
	UpstreamErrorUnknown UpstreamErrorKind = iota
	UpstreamErrorAccountBlock
	UpstreamErrorCloudflareChallenge
	UpstreamErrorDPoPChallenge
	UpstreamErrorGenericForbidden
	UpstreamErrorRateLimited
)

func (k UpstreamErrorKind) String() string {
	switch k {
	case UpstreamErrorAccountBlock:
		return "account_block"
	case UpstreamErrorCloudflareChallenge:
		return "cloudflare_challenge"
	case UpstreamErrorDPoPChallenge:
		return "dpop_challenge"
	case UpstreamErrorGenericForbidden:
		return "generic_forbidden"
	case UpstreamErrorRateLimited:
		return "rate_limited"
	default:
		return "unknown"
	}
}

// ClassifyUpstreamResponse classifies an upstream HTTP response.
func ClassifyUpstreamResponse(status int, header http.Header, body []byte) UpstreamErrorKind {
	if status == http.StatusTooManyRequests {
		return UpstreamErrorRateLimited
	}
	if header != nil {
		if hasCloudflareChallengeHeader(header) {
			return UpstreamErrorCloudflareChallenge
		}
		if hasDPoPChallengeHeader(header) {
			return UpstreamErrorDPoPChallenge
		}
	}
	switch status {
	case http.StatusForbidden:
		if IsDPoPProofRequiredBody(body) {
			return UpstreamErrorDPoPChallenge
		}
		if IsCloudflareChallengeBody(body) {
			return UpstreamErrorCloudflareChallenge
		}
		if grokbase.IsAccountBlockBody(body) {
			return UpstreamErrorAccountBlock
		}
		return UpstreamErrorGenericForbidden
	case http.StatusUnauthorized:
		if IsDPoPProofRequiredBody(body) {
			return UpstreamErrorDPoPChallenge
		}
	}
	return UpstreamErrorUnknown
}

// ClassifyUpstreamError classifies a returned error, preferring the typed
// grokUpstreamError and falling back to the legacy "grok upstream status=.. body=.."
// text format for plain errors.
func ClassifyUpstreamError(err error) UpstreamErrorKind {
	if err == nil {
		return UpstreamErrorUnknown
	}
	if typed, ok := errors.AsType[*grokUpstreamError](err); ok {
		return ClassifyUpstreamResponse(typed.status, typed.header, []byte(typed.body))
	}
	status := parseUpstreamStatus(err)
	return ClassifyUpstreamResponse(status, nil, []byte(upstreamErrorBody(err)))
}

// hasCloudflareChallengeHeader detects header-only Cloudflare challenge signals.
func hasCloudflareChallengeHeader(header http.Header) bool {
	if value := strings.TrimSpace(header.Get("CF-Mitigated")); value != "" {
		return true
	}
	for name := range header {
		if strings.Contains(strings.ToLower(name), "cf-mitigated") {
			return true
		}
	}
	return false
}

// hasDPoPChallengeHeader detects a WWW-Authenticate DPoP challenge. Header keys
// are matched case-insensitively so both canonical and raw keys are recognized.
func hasDPoPChallengeHeader(header http.Header) bool {
	for name, values := range header {
		if !strings.EqualFold(strings.TrimSpace(name), "www-authenticate") {
			continue
		}
		for _, value := range values {
			lower := strings.ToLower(value)
			if strings.Contains(lower, "dpop") &&
				(strings.Contains(lower, "challenge") || strings.Contains(lower, "nonce") || strings.Contains(lower, "proof")) {
				return true
			}
		}
	}
	return false
}

// IsDPoPProofRequiredBody reports a protocol-level DPoP challenge. It
// must not be attributed to an account credential or physical egress node.
func IsDPoPProofRequiredBody(body []byte) bool {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return IsDPoPProofRequiredText(string(body))
	}
	return IsDPoPProofRequiredText(strings.Join(grokbase.JSONStrings(payload), " "))
}

// IsDPoPProofRequiredText normalizes separators and matches the DPoP challenge
// vocabulary used by the xAI CLI proxy.
func IsDPoPProofRequiredText(value string) bool {
	normalized := strings.NewReplacer("-", "_", ":", "_", ".", "_", " ", "_").Replace(strings.ToLower(strings.TrimSpace(value)))
	for _, marker := range []string{
		"unauthorized_dpop_required",
		"dpop_proof_required",
		"dpop_proof_required_error",
		"invalid_dpop_proof",
		"dpop_token_required",
		"invalid_dpop_nonce",
		"use_dpop_nonce",
		"nonce_missing",
	} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

// IsCloudflareChallengeBody detects a Cloudflare interstitial/challenge page.
// These are egress/clearance failures, not account failures.
func IsCloudflareChallengeBody(body []byte) bool {
	lower := strings.ToLower(string(body))
	for _, marker := range []string{
		"cf-mitigated",
		"cf_chl_opt",
		"__cf_chl",
		"cf-chl",
		"challenge-platform",
		"turnstile",
		"just a moment",
		"enable javascript and cookies to continue",
		"verify you are human",
		"attention required",
		"request rejected by anti-bot rules",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

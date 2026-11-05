package ratelimit

import (
	"encoding/json"
	"net/http"
	"orchids-api/internal/httpclient"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Package ratelimit is the upstream rate-limit handling shared by the Grok
// Build and Grok Web planes: parsing the structured resource-exhausted 429,
// team+model cooldowns (process-local and shared through Redis), request
// pacing and rate-limit headers.
//
// What differs per plane stays with the plane: how a request's identity is
// derived, the error it returns for an active cooldown, and its metrics
// namespace.
//
// xAI returns a structured `resource-exhausted` 429 body carrying the team, the
// model, and the RPS/RPM window. Parsing it lets the gateway cool down at the
// team+model granularity instead of a fixed 60s blanket cooldown.

var (
	rateLimitUsagePattern   = regexp.MustCompile(`(?i)\bRequests?\s+per\s+(Second|Minute)\s*\(\s*actual\s*/\s*limit\s*\)\s*:\s*(\d+)\s*/\s*(\d+)`)
	rateLimitTeamPattern    = regexp.MustCompile(`(?i)\bteam\s+([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\b`)
	rateLimitModelPattern   = regexp.MustCompile(`(?i)\bmodel\s+["']?([A-Za-z0-9][A-Za-z0-9._:/-]*)`)
	rateLimitModelTrimChars = ".,;"
	rateLimitResetPattern   = regexp.MustCompile(`(?i)(\d+)\s*([dhms])`)
)

// Scope is the window a rate limit applies to.
type Scope string

const (
	ScopeRPS Scope = "rps"
	ScopeRPM Scope = "rpm"
)

// Metadata is the structured form of a resource-exhausted 429 body.
type Metadata struct {
	Scope      Scope
	TeamID     string
	Model      string
	Actual     int
	Limit      int
	RetryAfter time.Duration
}

// ParseMetadata extracts Team+Model RPS/RPM limit metadata from an
// upstream 429 body. It accepts both Build CLI and Grok Web
// resource-exhausted text shapes.
func ParseMetadata(body []byte) *Metadata {
	for _, text := range rateLimitTexts(body) {
		if metadata := parseRateLimitText(text); metadata != nil {
			return metadata
		}
	}
	return nil
}

// FromResponse derives Metadata from a 429 status, headers,
// and body. When Retry-After is absent but the body contains a reset window,
// the metadata RetryAfter is populated from the body text.
func FromResponse(status int, header http.Header, body []byte) *Metadata {
	if status != http.StatusTooManyRequests {
		return nil
	}
	metadata := ParseMetadata(body)
	if metadata == nil {
		return nil
	}
	if header != nil {
		if headerValue := header.Get("Retry-After"); headerValue != "" {
			if retryAfter := ParseRetryAfter(headerValue, time.Now().UTC()); retryAfter > 0 {
				metadata.RetryAfter = retryAfter
			}
		}
	}
	return metadata
}

// Describe renders a short human-readable summary appended to error text so
// existing string-matching classifiers keep working while adding diagnostics.
func (m *Metadata) Describe() string {
	if m == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("team=" + m.TeamID)
	if m.Model != "" {
		b.WriteString(" model=" + m.Model)
	}
	b.WriteString(" scope=" + string(m.Scope))
	if m.Limit > 0 {
		b.WriteString(" limit=" + strconv.Itoa(m.Actual) + "/" + strconv.Itoa(m.Limit))
	}
	if m.RetryAfter > 0 {
		b.WriteString(" reset=" + formatDurationShort(m.RetryAfter))
	}
	return b.String()
}

func formatDurationShort(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	case d >= time.Minute:
		return strconv.Itoa(int(d.Minutes())) + "m"
	default:
		return strconv.Itoa(int(d.Seconds())) + "s"
	}
}

func rateLimitTexts(body []byte) []string {
	texts := []string{string(body)}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return texts
	}
	collectRateLimitTexts(value, &texts)
	return texts
}

func collectRateLimitTexts(value any, texts *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		if message, ok := typed["message"].(string); ok {
			appendRateLimitText(message, texts)
		}
		if errText, ok := typed["error"].(string); ok {
			appendRateLimitText(errText, texts)
		}
		for _, nested := range typed {
			collectRateLimitTexts(nested, texts)
		}
	case []any:
		for _, nested := range typed {
			collectRateLimitTexts(nested, texts)
		}
	case string:
		appendRateLimitText(typed, texts)
	}
}

func appendRateLimitText(text string, texts *[]string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	*texts = append(*texts, text)
}

func parseRateLimitText(text string) *Metadata {
	match := rateLimitUsagePattern.FindStringSubmatch(text)
	if match == nil {
		return nil
	}
	actual, actualErr := strconv.Atoi(match[2])
	limit, limitErr := strconv.Atoi(match[3])
	if actualErr != nil || limitErr != nil {
		return nil
	}
	scope := ScopeRPM
	retryAfter := time.Minute
	if strings.EqualFold(match[1], "second") {
		scope = ScopeRPS
		retryAfter = 2 * time.Second
	}
	if parsed := rateLimitResetAfter(text); parsed > 0 {
		retryAfter = parsed
		if scope == ScopeRPS && retryAfter < 2*time.Second {
			retryAfter = 2 * time.Second
		}
	}
	return &Metadata{
		Scope:      scope,
		TeamID:     rateLimitTeamID(text),
		Model:      rateLimitModel(text),
		Actual:     actual,
		Limit:      limit,
		RetryAfter: retryAfter,
	}
}

func rateLimitTeamID(text string) string {
	match := rateLimitTeamPattern.FindStringSubmatch(text)
	if match == nil {
		return ""
	}
	return match[1]
}

func rateLimitModel(text string) string {
	match := rateLimitModelPattern.FindStringSubmatch(text)
	if match == nil {
		return ""
	}
	return strings.TrimRight(match[1], rateLimitModelTrimChars)
}

func rateLimitResetAfter(body string) time.Duration {
	index := strings.Index(strings.ToLower(body), "resets in:")
	if index < 0 {
		return 0
	}
	text := body[index+len("resets in:"):]
	var total time.Duration
	for _, match := range rateLimitResetPattern.FindAllStringSubmatch(text, -1) {
		value, _ := strconv.Atoi(match[1])
		switch strings.ToLower(match[2]) {
		case "d":
			total += time.Duration(value) * 24 * time.Hour
		case "h":
			total += time.Duration(value) * time.Hour
		case "m":
			total += time.Duration(value) * time.Minute
		case "s":
			total += time.Duration(value) * time.Second
		}
	}
	return total
}

// ParseRetryAfter reads a Retry-After header (seconds or an HTTP date).
func ParseRetryAfter(value string, now time.Time) time.Duration {
	return httpclient.ParseRetryAfter(value, now, 0)
}

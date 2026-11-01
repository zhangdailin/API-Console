package config

import (
	"regexp"
	"strconv"
	"strings"
)

// Keep the repository's known Build baseline; this is not a latest-version lookup.
const grokCLIClientVersion = "1.0.60"

var grokCLIProductVersion = regexp.MustCompile(`(^|[ \t])grok-(?:shell|pager)/[0-9][0-9A-Za-z.+-]*`)

// NormalizeGrokCLIIdentity migrates recognizable legacy identities on config
// load/save. Future versions and custom product identities remain configurable.
func NormalizeGrokCLIIdentity(c *Config) {
	if c == nil {
		return
	}
	if strings.TrimSpace(c.GrokCLIClientVersion) != "" {
		c.GrokCLIClientVersion = normalizedGrokVersion(c.GrokCLIClientVersion)
	}
	if strings.TrimSpace(c.GrokCLIUserAgent) != "" {
		c.GrokCLIUserAgent = c.GrokCLIUserAgentOrDefault()
	}
}

func normalizedGrokVersion(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || legacyGrokVersion(value) {
		return grokCLIClientVersion
	}
	return value
}

func legacyGrokVersion(value string) bool {
	version, _, _ := strings.Cut(strings.TrimPrefix(value, "v"), "+")
	version, prerelease, _ := strings.Cut(version, "-")
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}
	var numbers [3]uint64
	for i, part := range parts {
		if part == "" || strings.ContainsAny(part, "+-") {
			return false
		}
		n, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return false
		}
		numbers[i] = n
	}
	baseline := strings.Split(grokCLIClientVersion, ".")
	for i, n := range numbers {
		minimum, _ := strconv.ParseUint(baseline[i], 10, 64)
		if n != minimum {
			return n < minimum
		}
	}
	return prerelease != ""
}

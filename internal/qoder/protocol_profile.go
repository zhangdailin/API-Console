package qoder

import (
	"orchids-api/internal/config"
	"strings"
)

// ProfileReference preserves the deployed dialect. ProfileSkillCLI is an
// explicit compatibility experiment, not an assertion of live gateway support.
const (
	ProfileReference     = "reference"
	ProfileSkillCLI      = "skill-cli"
	SkillCLIClientID     = "732aef47-9cf2-46a2-95fe-4cebb5d0d1fa"
	SkillCLIInferenceURL = "https://api3.qoder.sh"
)

type protocolProfile struct {
	name             string
	clientID         string
	inference        string
	businessProduct  string
	tokenlessRuntime bool
	machineTokenIsID bool
}

func resolveProtocolProfile(cfg *config.Config) protocolProfile {
	if cfg != nil && strings.EqualFold(strings.TrimSpace(cfg.QoderProtocolProfile), ProfileSkillCLI) {
		return protocolProfile{ProfileSkillCLI, SkillCLIClientID, SkillCLIInferenceURL, "cli", true, true}
	}
	// Empty and unrecognized values never silently opt into the experimental
	// dialect. This also preserves callers constructing a zero-value Client.
	return protocolProfile{ProfileReference, DefaultClientID, DefaultInferenceURL, sceneBusinessProduct, false, false}
}

func (c *Client) businessProduct() string {
	if c != nil && c.protocol.businessProduct != "" {
		return c.protocol.businessProduct
	}
	return sceneBusinessProduct
}

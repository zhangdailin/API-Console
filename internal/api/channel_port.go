package api

import (
	"sync"
	"time"

	"orchids-api/internal/channel"
	"orchids-api/internal/provider"
)

// The management API reaches a channel only through the port its package
// registered. What a credential is, which fields hold it and which of them may
// leave the server is the channel's knowledge; the handlers here own the HTTP
// surface, the state machines and the response shape.

// adminFor returns the management facet of the channel that owns an account
// type, or nil when no channel does.
func adminFor(accountType string) *provider.Admin {
	spec, ok := provider.Lookup(accountType)
	if !ok {
		return nil
	}
	return spec.Admin
}

// registeredLoginFlow returns the authorization flow a channel registered, or
// nil when it has none.
func registeredLoginFlow(id channel.ID) *provider.Login {
	if admin := adminFor(string(id)); admin != nil {
		return admin.Login
	}
	return nil
}

// resolveLoginFlow is how a login finds its channel's flow. It is a variable so
// a test can drive a flow against a stub upstream. A transaction captures its
// flow when it starts and never resolves it again, so replacing this cannot
// change a login that is already polling.
var (
	resolveLoginFlowMu sync.RWMutex
	resolveLoginFlow   = registeredLoginFlow
)

// loginFlow returns a channel's authorization flow, or nil when it has none.
func loginFlow(id channel.ID) *provider.Login {
	resolveLoginFlowMu.RLock()
	resolve := resolveLoginFlow
	resolveLoginFlowMu.RUnlock()
	return resolve(id)
}

// channelLabel is the name a channel carries in operator-facing messages.
func channelLabel(id channel.ID) string {
	if definition, ok := channel.DefinitionFor(id); ok {
		return definition.Label
	}
	return string(id)
}

// observedUsageWindow is how far back the gateway's own token count has to
// reach to serve every channel's allowance projection. Zero means no channel
// uses the measurement.
func observedUsageWindow() time.Duration {
	var window time.Duration
	for _, spec := range provider.All() {
		if spec.Admin != nil && spec.Admin.ObservedUsageWindow > window {
			window = spec.Admin.ObservedUsageWindow
		}
	}
	return window
}

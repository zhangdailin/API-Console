// Package provider is the port every upstream channel implements.
//
// A channel is one Spec: how to build its inference client, what its catalog
// can answer, how to probe it, how to read its model catalog and how to keep
// one account current. Each channel package registers its own Spec from an
// init function, and every consumer — the shared inference pipeline, the
// refresh scheduler, model discovery, startup — iterates the registry instead
// of switching on a channel name.
//
// This package imports no channel implementation. That is the rule that keeps
// the seam usable by all four channels: the table used to live here together
// with imports of three of them, so a fourth could only be added by repeating
// the dispatch in each consumer.
package provider

import (
	"context"
	"fmt"
	"strings"
	"time"

	"orchids-api/internal/channel"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
)

// HealthRefreshInterval is how often a channel's catalog and allowance
// snapshots are re-read by the background scheduler.
const HealthRefreshInterval = 30 * time.Minute

// Client is the inference contract of a channel that runs through the shared
// pipeline. onMessage receives the channel's stream as protocol-neutral events.
type Client interface {
	SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error
}

// Env is what a control-plane operation may use. Store is nil in a caller that
// only needs a read (a startup probe); an operation that persists must tolerate
// that by skipping the write.
type Env struct {
	Config *config.Config
	Store  *store.Store
}

// Probe is a credential-free reachability check run once at startup, so a
// blocked egress path shows up in the boot log instead of as a per-request 502.
type Probe struct {
	// Subject names the backend in the log line ("WorkBuddy backend").
	Subject string
	// Endpoint is the address reported next to the verdict.
	Endpoint string
	Run      func(ctx context.Context, cfg *config.Config) error
}

// Spec describes one channel. A nil field means the channel does not offer
// that facet, and every consumer must treat it as "nothing to do".
type Spec struct {
	ID channel.ID

	// NewClient builds the inference client for one account and gives it the
	// store, so a credential the upstream rotates during a request is written
	// back. It is nil for a channel that serves inference through its own
	// handler (Grok).
	NewClient func(acc *store.Account, cfg *config.Config, s *store.Store) Client

	// Capabilities answers catalog questions for the shared pipeline.
	Capabilities Capabilities

	// ConversationAffinity keeps successive turns on an eligible account.
	ConversationAffinity bool

	// QueueRefusalAccountProbes is how many untried accounts a request may move
	// to when the upstream answers with a queue refusal, before it waits the
	// refusal out on the account it holds. Zero keeps the wait-in-place policy.
	//
	// A queue refusal is treated as shared by every account, so the refused
	// account records no cooldown either way; probing only matters when the
	// gate is in fact per account (a busy account beside idle ones), where
	// waiting in place leaves the idle accounts unused.
	QueueRefusalAccountProbes int

	Probe *Probe

	// Catalog reads the channel's upstream model catalog.
	Catalog *Catalog

	// Admin serves the management API: account identity, writes, verification,
	// output and login.
	Admin *Admin

	// Maintain brings one enabled account up to date: credential, catalog and
	// allowance, each on the channel's own cadence. It is called on every
	// scheduler tick and decides for itself whether anything is due.
	Maintain func(ctx context.Context, env Env, acc *store.Account)
}

// specs is written only by Register during package initialization and read
// without a lock afterwards; the request path looks a channel up on every
// account selection.
var specs = map[channel.ID]Spec{}

// Register publishes a channel. It must be called from an init function.
//
// A channel that is not in internal/channel, or one registered twice, is a
// wiring mistake that would otherwise surface as a request silently served by
// the wrong implementation, so both stop the process at startup.
func Register(spec Spec) {
	if _, known := channel.DefinitionFor(spec.ID); !known {
		panic(fmt.Sprintf("provider: channel %q is not defined in internal/channel", spec.ID))
	}
	if _, dup := specs[spec.ID]; dup {
		panic(fmt.Sprintf("provider: channel %q registered twice", spec.ID))
	}
	specs[spec.ID] = spec
}

// Lookup returns the channel that owns an account type.
//
// The lookup is case-insensitive because account types are normalized to lower
// case when stored but reach this seam from an operator, an import file, or a
// URL path in whatever case they were written.
func Lookup(accountType string) (Spec, bool) {
	spec, ok := specs[channel.ID(strings.ToLower(strings.TrimSpace(accountType)))]
	return spec, ok
}

// All returns the registered channels in channel-definition order, so a caller
// that iterates them is deterministic.
func All() []Spec {
	definitions := channel.All()
	out := make([]Spec, 0, len(definitions))
	for _, definition := range definitions {
		if spec, ok := specs[definition.ID]; ok {
			out = append(out, spec)
		}
	}
	return out
}

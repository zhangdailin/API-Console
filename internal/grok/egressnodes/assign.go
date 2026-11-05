package egressnodes

import (
	"context"
	"log/slog"
	"sort"
	"sync"

	"orchids-api/internal/store"
)

// AccountRef is one account that routes through a scope, identified by the
// affinity key the egress managers use for it.
type AccountRef struct {
	Scope     string
	Affinity  string
	AccountID int64
}

// AccountLister enumerates the accounts the egress managers route. The Grok
// channel installs it: only it knows how each plane derives an affinity.
type AccountLister func(ctx context.Context) ([]AccountRef, error)

var accountLister struct {
	sync.RWMutex
	fn AccountLister
}

// SetAccountLister installs the account enumeration hook.
func SetAccountLister(fn AccountLister) {
	accountLister.Lock()
	accountLister.fn = fn
	accountLister.Unlock()
}

func listAccounts(ctx context.Context) ([]AccountRef, error) {
	accountLister.RLock()
	fn := accountLister.fn
	accountLister.RUnlock()
	if fn == nil {
		return nil, nil
	}
	return fn(ctx)
}

// pendingBindings counts bindings chosen at request time that the store has
// not yet reflected, so concurrent first requests respect node capacity.
var pendingBindings sync.Map // scope|affinity -> node ID

// AutoBind pins an unbound account to the least-loaded node of scope with
// spare capacity when automatic assignment is on. It returns the chosen node,
// or "" when automatic assignment is off or every node is full. candidates are
// the node IDs currently able to serve.
func AutoBind(scope, affinity string, candidates []string) string {
	r := Default()
	snap := Current()
	if r == nil || affinity == "" || !snap.Operations.AutoAssignEnabled || len(candidates) == 0 {
		return ""
	}
	field := scope + "|" + affinity
	if node, ok := pendingBindings.Load(field); ok {
		return node.(string)
	}
	counts := snap.BoundCount()
	pendingBindings.Range(func(key, value any) bool {
		if _, stored := snap.Bindings[key.(string)]; !stored {
			counts[value.(string)]++
		}
		return true
	})
	best := ""
	for _, id := range candidates {
		node, ok := snap.Node(id)
		if !ok || (node.Capacity > 0 && counts[id] >= node.Capacity) {
			continue
		}
		if best == "" || counts[id] < counts[best] {
			best = id
		}
	}
	if best == "" {
		return ""
	}
	pendingBindings.Store(field, best)
	go func() {
		defer pendingBindings.Delete(field)
		ctx := context.Background()
		if err := r.store.SetGrokEgressBinding(ctx, scope, affinity, best); err != nil {
			slog.Warn("egress: saving account binding failed", "error", err)
			return
		}
		r.Changed(ctx)
	}()
	return best
}

// RebalanceResult summarizes one assignment pass.
type RebalanceResult struct {
	Accounts int `json:"accounts"`
	Assigned int `json:"assigned"`
	Moved    int `json:"moved"`
	Released int `json:"released"`
	Unplaced int `json:"unplaced"`
}

// Rebalance recomputes account bindings. Valid bindings are kept; accounts on
// a removed, disabled or over-capacity node are reassigned; unbound accounts
// go to the least-loaded node with spare capacity. With balance on, accounts
// also move from the busiest node until loads differ by at most one.
func (r *Registry) Rebalance(ctx context.Context, balance bool) (RebalanceResult, error) {
	var result RebalanceResult
	accounts, err := listAccounts(ctx)
	if err != nil {
		return result, err
	}
	snap := Current()
	unhealthy := map[string]bool{}
	if probes, err := r.store.ListGrokEgressProbes(ctx); err == nil {
		for id, probe := range probes {
			unhealthy[id] = probe.Status == "unhealthy"
		}
	}
	next := map[string]string{}
	byScope := map[string][]AccountRef{}
	for _, account := range accounts {
		byScope[account.Scope] = append(byScope[account.Scope], account)
	}
	result.Accounts = len(accounts)
	for _, scope := range store.GrokEgressScopes {
		var nodes []Node
		for _, node := range snap.Candidates(scope) {
			if !node.Pool && !unhealthy[node.ID] {
				nodes = append(nodes, node)
			}
		}
		refs := byScope[scope]
		if len(nodes) == 0 {
			for _, ref := range refs {
				if snap.Binding(scope, ref.Affinity) != "" {
					result.Released++
				}
			}
			continue
		}
		valid := map[string]Node{}
		for _, node := range nodes {
			valid[node.ID] = node
		}
		load := map[string][]string{}
		var unbound []string
		sort.Slice(refs, func(i, j int) bool { return refs[i].AccountID < refs[j].AccountID })
		for _, ref := range refs {
			bound := snap.Binding(scope, ref.Affinity)
			node, ok := valid[bound]
			if ok && (node.Capacity == 0 || len(load[bound]) < node.Capacity) {
				load[bound] = append(load[bound], ref.Affinity)
				continue
			}
			if bound != "" {
				result.Moved++
			}
			unbound = append(unbound, ref.Affinity)
		}
		leastLoaded := func(exclude string) string {
			best := ""
			for _, node := range nodes {
				if node.ID == exclude || (node.Capacity > 0 && len(load[node.ID]) >= node.Capacity) {
					continue
				}
				if best == "" || len(load[node.ID]) < len(load[best]) {
					best = node.ID
				}
			}
			return best
		}
		for _, affinity := range unbound {
			target := leastLoaded("")
			if target == "" {
				result.Unplaced++
				continue
			}
			load[target] = append(load[target], affinity)
			result.Assigned++
		}
		if balance {
			for range 10000 {
				busiest := ""
				for _, node := range nodes {
					if busiest == "" || len(load[node.ID]) > len(load[busiest]) {
						busiest = node.ID
					}
				}
				target := leastLoaded(busiest)
				if busiest == "" || target == "" || len(load[busiest])-len(load[target]) <= 1 {
					break
				}
				moved := load[busiest][len(load[busiest])-1]
				load[busiest] = load[busiest][:len(load[busiest])-1]
				load[target] = append(load[target], moved)
				result.Moved++
			}
		}
		for node, affinities := range load {
			for _, affinity := range affinities {
				next[scope+"|"+affinity] = node
			}
		}
	}
	if err := r.store.ReplaceGrokEgressBindings(ctx, next); err != nil {
		return result, err
	}
	r.Changed(ctx)
	return result, nil
}

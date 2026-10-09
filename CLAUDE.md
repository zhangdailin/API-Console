# CLAUDE.md

Guidance for Claude Code and other coding agents working in this repository.

## What this is

`orchids-api` is a Go multi-channel LLM gateway. It serves Anthropic Messages,
OpenAI Chat Completions and OpenAI Responses clients and forwards each request to
one of four upstream channels: **WorkBuddy**, **Qoder**, **Cline** and
**Grok Build**. The deployed binary is named `orchids-server`; that is a build
artifact name, not a project rename.

Go version comes from `go.mod` (currently 1.26.6). Redis is required at runtime.

## Language convention

- **Code and code comments: English.** Identifiers, log messages, error strings
  visible to clients and comments are English.
- **Docs under `docs/` and the READMEs: Chinese**, with `README_EN.md` as the
  English counterpart.
- Legacy Chinese comments remain in some older files; new and edited comments
  should be English.

## Commands

```bash
go build ./...                          # compile
go test ./... -count=1 -p 1             # full suite (-p 1: tests share ports and Redis state)
go test -race -count=1 -p 1 ./...       # race detector; needs CGO_ENABLED=1
go vet ./...                            # must be clean
go build -o orchids-server ./cmd/server # production binary
```

Web assets are embedded (`//go:embed`) and have their own tests:

```bash
node --test web/*.test.cjs
./scripts/check-provider-registry.sh
# On a confined Windows sandbox add --test-isolation=none: node --test spawns a
# child per file, which the sandbox blocks with EPERM.
```

CI has five jobs: **lint** (gofmt + vet), **test**, **race**, **vulncheck** and
**audit**. See `.github/workflows/ci.yml`.

## Line endings: LF only

`.gitattributes` sets `* text=auto eol=lf` and overrides `core.autocrlf`. Go
tooling rejects CRLF: `gofmt -l` reports a CRLF file as unformatted with a
whole-file diff. If `gofmt -l` suddenly reports hundreds of files, the checkout
was rewritten to CRLF — do not mass-reformat, fix the checkout instead.

## Architecture in one page

```
cmd/server        startup, route registration (routes.go is the only assembly point)
internal/responses     OpenAI Responses protocol: wire types, conversions, SSE,
                       sub-resources, compaction, chat→Responses bridge
internal/chatwire     OpenAI Chat Completions wire types
internal/httpserver   shared HTTP/SSE reading and writing, error envelopes
internal/httpclient   HTTP transport: proxies, client pool, browser fingerprint
internal/handler  shared pipeline for WorkBuddy / Qoder / Cline
internal/grok     Grok Build native Messages / Chat Completions / Responses
internal/channel  provider identity, prefixes, metadata (single source of truth)
internal/provider     provider client construction and channel capabilities
internal/modelrefresh model directory discovery and reconciliation
internal/refresh      scheduled credential and quota refresh
internal/alerting     alert rules, evaluation, snapshot assembly
internal/poolstate    account-pool summaries (console and alert views)
internal/loadbalancer  account selection, cooldowns, connection tracking
internal/store    Redis-backed accounts, models, API keys, config
internal/api      admin HTTP API
internal/middleware  auth, rate limiting, tracing, diagnostics
```

Inference routes require a provider path: **`/{channel}/*`** and
**`/{channel}/v1/*`** bind the same channel and expose the same endpoints.
Prefixes come from `internal/channel` (WorkBuddy, Qoder, Cline and Grok).
Root and unified `/v1` inference endpoints are unregistered and return 404.
The root admin redirect, management APIs and health/metrics routes remain.

Only Grok speaks Responses natively. The other three channels expose only
`/v1/chat/completions`, so their Responses support goes through
`internal/responses`. Capability differences are tabulated
in `docs/coding-protocol-capabilities.md`.

Upstream calls go through one interface
(`internal/handler.UpstreamClient.SendRequestWithPayload`) with a callback that
emits SSE events. Shared streaming emitters live in `internal/upstream/emit.go`.

## Invariants — do not weaken these

These are load-bearing. A change that "simplifies" any of them is a bug.

1. **Inference authentication is always required.** An API key is mandatory on
   `/v1` and `/{channel}/v1` routes. `inference_auth_enabled: false` is advisory
   only and does **not** open the routes. The single exception is a source listed
   in `anonymous_allow_ips`; a malformed entry must make the whole list unusable
   and require a key rather than open the routes
   (`cmd/server/routes.go`, `inferenceAuth`).
2. **Credentials never reach clients or logs.** Not in error bodies, not in
   admin responses, not in logs or diagnostics. Redaction is enforced by
   `internal/errors` public messages, `audit.Redact` and the
   `attempt_diagnostics` scrubber.
3. **The credential encryption key is data.** `data/credential.key` (or
   `ORCHIDS_CREDENTIAL_ENCRYPTION_KEY`) must be backed up together with Redis;
   losing it makes stored credentials undecryptable. It is never written to
   Redis.
4. **Model catalogs are observations only.** Model rows are published only from
   an upstream catalog read for a currently active account. When a channel has no
   active account, or the read fails, **publish nothing** — never a cached,
   compiled-in or locally defaulted list (`internal/modelrefresh`, `noActiveAccountsError`).
5. **Production deployment posture:** strong admin password, `debug_enabled`
   off, `/metrics` and the backend port not publicly reachable, and
   `trusted_proxies` containing only real proxy addresses. `settings:config` in
   Redis (with the configured `redis_prefix`) overrides file config.

## Conventions worth knowing

- Tests use `internal/testutil` helpers (`Equal`, `Falsef`, `MustContain`, …).
  Prefer them over raw `t.Errorf`.
- Tests must not be flaky. Do not assert a property that a random or
  timing-dependent implementation does not actually guarantee — see
  `TestSelectAccountRotatesAcrossEqualAccounts` for the pattern to follow
  (assert the deterministic property, then a wide statistical bound).
- Performance-sensitive code is deliberately written that way: `sync.Pool`
  scratch buffers (`selectionScratch`, `internal/perf`), SWAR bit tricks in SSE
  framing, zero-copy chunk builders. Do not rewrite for "readability" without a
  benchmark.
- Reuse the existing abstractions instead of adding a parallel one:
  `internal/upstream/emit.go`, `internal/api/device_login_registry.go` (generic),
  `internal/api/quota_projection.go` and `account_refresh.go` (map dispatch),
  `internal/util/snapshot.go`, `internal/util/http_body.go`.
- Real-upstream verification records are local operational material and must not
  be committed. `docs/verification/` is ignored by Git. Local mocks
  passing does not mean an upstream accepted a request; when changing request
  construction or streaming, state explicitly what was verified against a live
  upstream and what was not.

## Documentation map

`README.md` · `docs/api-reference.md` · `docs/configuration.md` ·
`docs/architecture.md` · `docs/deployment.md` ·
`docs/coding-protocol-capabilities.md` · `deploy/README.md`

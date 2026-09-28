# API Console

[中文](README.md) | [English](README_EN.md)

A Go proxy for WorkBuddy, Qoder, Cline, and Grok Build OAuth. It exposes Claude Messages, OpenAI Chat Completions, and Responses-compatible APIs. The unified `/v1` prefix routes by model; `/{channel}/v1` selects a channel explicitly.

## Quick start

Requires Go **1.26.6+** (see `go.mod`) and Redis. Start Redis, copy the example config, then run the server:

```bash
docker run -d --name api-console-redis -p 6379:6379 redis:7
cp config.example.json config.json
go run ./cmd/server -config ./config.json
```

Open `http://127.0.0.1:3002/admin/`, sign in, add an account using the channel's official authorization flow, and create an API key. If `admin_pass` is empty, the generated admin password appears in the startup logs. Refresh each channel's model catalog in the admin UI; no models are published without an available account.

```bash
curl http://127.0.0.1:3002/health
curl http://127.0.0.1:3002/v1/models -H 'Authorization: Bearer <API_KEY>'
curl http://127.0.0.1:3002/v1/messages \
  -H 'Authorization: Bearer <API_KEY>' -H 'Content-Type: application/json' \
  -d '{"model":"<MODEL_ID>","max_tokens":256,"messages":[{"role":"user","content":"Hello"}]}'
```

`/v1/messages`, `/v1/chat/completions`, and `/v1/responses` route by model. Channel-specific prefixes expose the same interfaces; `/v1/models` lists available models. Grok uses only the Build OAuth CLI upstream. All four channels offer official authorization through the admin UI.

## Security and operations

- Model and inference endpoints require a managed API key (Anthropic clients may use `x-api-key`). **`inference_auth_enabled=false` does not disable this check**. Only sources explicitly included in `anonymous_allow_ips` can bypass it.
- Redis `settings:config` (under `redis_prefix`) overrides the config file. Back up `data/credential.key` alongside Redis; without the encryption key, existing account credentials cannot be decrypted.
- Set a strong admin password, leave `debug_enabled` off in production, and restrict public access to `/metrics` and the backend port. Trust only your actual reverse proxy IPs.
- Test: `go test ./...`; build: `go build -o orchids-server ./cmd/server`. `orchids-server` remains the current binary name used by the build and deployment tooling, not the new project name.

More: [API and account login](docs/api-reference.md) · [Configuration](docs/configuration.md) · [Host deployment](deploy/README.md).

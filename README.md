<div align="center">

# API Console

**One gateway for WorkBuddy, Qoder, Cline, and Grok Build.**

[![Go](https://img.shields.io/badge/Go-1.26.6%2B-00ADD8?logo=go&logoColor=white)](go.mod)
[![Redis](https://img.shields.io/badge/Redis-required-DC382D?logo=redis&logoColor=white)](config.example.json)
[![CI](https://github.com/zhangdailin/API-Console/actions/workflows/ci.yml/badge.svg)](https://github.com/zhangdailin/API-Console/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/zhangdailin/API-Console)](https://github.com/zhangdailin/API-Console/releases)

English | [简体中文](README_CN.md)

[Getting Started](#deployment) · [API Usage](#api-usage) · [Documentation](#documentation)

</div>

---

## Overview

API Console is a self-hosted AI API gateway written in Go. It connects multiple upstream accounts to a shared API, with a built-in web console for authorization, account management, model discovery, API keys, and operations monitoring.

Applications can use Claude Messages, OpenAI Chat Completions, or Responses endpoints. Every inference URL names a provider. Both `/{provider}` and `/{provider}/v1` serve the same endpoints; root and unified `/v1` inference routes return 404.

## Features

- **Four Upstream Channels** — WorkBuddy, Qoder, Cline, and Grok Build OAuth, with official authorization flows in the console.
- **Compatible API Interfaces** — Messages, Chat Completions, and Responses, including streaming, reasoning, and tool calls where supported by the upstream model.
- **Account Pool Scheduling** — Weighted selection, concurrency limits, credential refresh, cooldowns, and retry policies.
- **Model Discovery** — Refresh catalogs from authorized accounts and manage model visibility and routing.
- **API Key Management** — Create and rotate keys with model permissions, expiry, request limits, concurrency limits, and usage budgets.
- **Operations Dashboard** — Request rates, latency, time to first token, channel and model views, runtime information, and alert rules.
- **Logs and Diagnostics** — Inspect requests, upstream attempts, errors, optional diagnostic captures, and management audit logs.
- **Embedded Web Console** — Responsive light and dark themes, packaged inside the Go binary without a separate frontend build.
- **Release Updates** — Version checks and guarded online upgrades with rollback on eligible Linux systemd deployments.

## Supported Channels

| Channel | Authorization | API prefix |
|---------|---------------|------------|
| WorkBuddy | Official browser authorization | `/workbuddy/v1` |
| Qoder | Official device authorization | `/qoder/v1` |
| Cline | WorkOS device authorization | `/cline/v1` |
| Grok | Build OAuth device authorization | `/grok/v1` |

Grok uses the **Build OAuth CLI upstream**. Available models and capabilities depend on the authorized accounts and their upstream entitlements. Catalog refresh failures preserve previously observed models rather than substituting a built-in model list.

## Tech Stack

| Component | Technology |
|-----------|------------|
| Backend | Go 1.26.6+, HTTP handlers, provider adapters |
| Frontend | Go templates, HTML, CSS, vanilla JavaScript |
| Storage | Redis for accounts, configuration, API keys, response state, and operational data |
| Packaging | Single binary with embedded web assets |
| Host deployment | Linux, systemd, and a reverse proxy such as Caddy |

## Deployment

### Method 1: Run from Source

#### Prerequisites

- Go **1.26.6+**, as specified in [go.mod](go.mod).
- A running Redis instance.
- Network access to the upstream services you intend to use.

#### Installation

```bash
git clone https://github.com/zhangdailin/API-Console.git
cd API-Console

# Optional: start a local Redis instance with persistent storage.
docker run -d --name api-console-redis \
  -p 127.0.0.1:6379:6379 \
  -v api-console-redis:/data \
  redis:7 redis-server --appendonly yes

cp config.example.json config.json
go run ./cmd/server -config ./config.json
```

If Redis is already running, skip the Docker command and set its connection details in `config.json`.

<details>
<summary>Windows PowerShell</summary>

After cloning the repository and starting Redis:

```powershell
Copy-Item config.example.json config.json
go run ./cmd/server -config ./config.json
```

</details>

To build a standalone binary:

```bash
go build -o orchids-server ./cmd/server
./orchids-server -config ./config.json
```

On Windows, use `go build -o orchids-server.exe ./cmd/server` and run `./orchids-server.exe -config ./config.json`.

### Method 2: Use a Release Binary

Download the Linux **amd64** binary, its SHA-256 checksum, and build metadata from [GitHub Releases](https://github.com/zhangdailin/API-Console/releases). Place the binary alongside a copy of [config.example.json](config.example.json), renamed to `config.json`.

```bash
sha256sum -c orchids-server-linux-amd64.sha256
chmod +x orchids-server-linux-amd64
./orchids-server-linux-amd64 --version
./orchids-server-linux-amd64 -config ./config.json
```

Existing build and deployment scripts retain the binary name `orchids-server`. The project is API Console, with the four channels listed above.

For a persistent Linux service, reverse proxy, and backend port protection, follow the [host deployment guide](deploy/README.md). Review the deployment script's options with:

```bash
bash scripts/deploy-orchids.sh --help
```

### Initial Setup

1. Open **http://127.0.0.1:3002/admin/**.
2. Sign in as `admin`. If `admin_pass` is empty, the server prints a generated password in its startup logs.
3. Add an account through the channel's official authorization flow in **Accounts**.
4. Refresh the channel's model catalog in **Models** and select an available model ID.
5. Create an API key in the console and save the complete key when it is shown.
6. Configure your application with the gateway URL, key, and model ID.

The default port and admin path can be changed in the configuration. Upstream login credentials belong to account authorization; client applications use gateway-issued API keys.

### Upgrades

The console can check for release updates. Installing an update or rolling back requires an explicitly enabled, eligible Linux deployment running as root under a persistent systemd service outside a container. The upgrade flow verifies release artifacts and uses a watchdog to check the restarted binary.

See [Online Upgrade](docs/online-upgrade.md) for eligibility, configuration, and recovery steps. Back up persistent data before upgrading; binary rollback does not restore Redis data.

## API Usage

Use `http://127.0.0.1:3002/workbuddy/v1`, or choose `/qoder/v1`, `/cline/v1`, or `/grok/v1`. The corresponding unversioned provider bases expose the same endpoints.

| Method | Path relative to the API prefix | Purpose |
|--------|---------------------------------|---------|
| `GET` | `/models` | List visible models |
| `GET` | `/models/{id}` | Retrieve model information |
| `POST` | `/messages` | Claude Messages-compatible inference |
| `POST` | `/chat/completions` | OpenAI Chat Completions-compatible inference |
| `POST` | `/responses` | Responses-compatible inference |

Grok uses a native Build Responses path; the other channels bridge Responses through Chat Completions. See the [capability matrix](docs/coding-protocol-capabilities.md) for channel and model limitations.

### List Models

```bash
curl http://127.0.0.1:3002/workbuddy/v1/models \
  -H 'Authorization: Bearer <API_KEY>'
```

Replace `<API_KEY>` with a key created in the console and `<MODEL_ID>` below with an ID returned by the model catalog.

### Chat Completions

```bash
curl http://127.0.0.1:3002/workbuddy/v1/chat/completions \
  -H 'Authorization: Bearer <API_KEY>' \
  -H 'Content-Type: application/json' \
  -d '{"model":"<MODEL_ID>","messages":[{"role":"user","content":"Hello!"}],"stream":true}'
```

### Claude Messages

```bash
curl http://127.0.0.1:3002/workbuddy/v1/messages \
  -H 'x-api-key: <API_KEY>' \
  -H 'Content-Type: application/json' \
  -d '{"model":"<MODEL_ID>","max_tokens":256,"messages":[{"role":"user","content":"Hello!"}]}'
```

### Responses

```bash
curl http://127.0.0.1:3002/workbuddy/v1/responses \
  -H 'Authorization: Bearer <API_KEY>' \
  -H 'Content-Type: application/json' \
  -d '{"model":"<MODEL_ID>","input":"Hello!","stream":true}'
```

Response resources, input history, cancellation, token estimation, and compaction are documented in the [API reference](docs/api-reference.md). Local cancellation of an ownership-only response record does not cancel upstream generation.

## Configuration and Operations

- **Configuration precedence** — Redis configuration at `<redis_prefix>settings:config` can override the local configuration file. Check effective values in the console after making changes.
- **Credential backups** — Back up Redis together with the credential encryption key, normally `data/credential.key`. Losing the key prevents existing account credentials from being decrypted.
- **API authentication** — Model and inference endpoints require a managed API key. `inference_auth_enabled=false` does not disable authentication; only explicitly allowed `anonymous_allow_ips` sources are exempt.
- **Network access** — Use a strong admin password, disable `debug_enabled` in production, restrict `/metrics` and the backend port, and trust only your actual reverse proxy addresses.
- **Monitoring scope** — Latency percentiles use retained samples. Collection health and diagnostic coverage are exposed separately so missing data is not presented as healthy zero traffic.

```bash
curl http://127.0.0.1:3002/health
```

## Development

```bash
go test ./...
go vet ./...
node --test web/*.test.cjs
go build -o orchids-server ./cmd/server
```

Node.js is used for frontend tests; the application does not require a Node.js runtime or a frontend build step.

## Documentation

The detailed guides below are currently written in Chinese.

| Guide | Contents |
|-------|----------|
| [API Reference](docs/api-reference.md) | Inference routes, management APIs, authorization, and error responses |
| [Configuration](docs/configuration.md) | Configuration fields, precedence, proxies, and credential storage |
| [Architecture](docs/architecture.md) | Routing, provider adapters, storage, and model discovery |
| [Protocol Capabilities](docs/coding-protocol-capabilities.md) | Channel capabilities and verification boundaries |
| [Host Deployment](deploy/README.md) | Linux examples, reverse proxy, and network protection |
| [Deployment Notes](docs/deployment.md) | Backups, multiple instances, and validation |
| [Online Upgrade](docs/online-upgrade.md) | Release verification, upgrades, rollback, and recovery |

## Contributing

Issues and pull requests are welcome. Include reproduction steps for bug reports, and run the relevant checks before submitting a change. Keep upstream behavior claims separate from local test results.

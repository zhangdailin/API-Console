<div align="center">

# API Console

**统一接入 WorkBuddy、Qoder、Cline 与 Grok Build。**

[![Go](https://img.shields.io/badge/Go-1.26.9%2B-00ADD8?logo=go&logoColor=white)](go.mod)
[![Redis](https://img.shields.io/badge/Redis-required-DC382D?logo=redis&logoColor=white)](config.example.json)
[![CI](https://github.com/zhangdailin/API-Console/actions/workflows/ci.yml/badge.svg)](https://github.com/zhangdailin/API-Console/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/zhangdailin/API-Console)](https://github.com/zhangdailin/API-Console/releases)

[English](README.md) | 简体中文

[快速开始](#部署) · [API 接入](#api-接入) · [项目文档](#项目文档)

</div>

---

## 项目介绍

API Console 是使用 Go 编写的自托管 AI API 网关，将多个上游账号接入统一接口。内置管理后台集中处理官方授权、账号管理、模型发现、API Key 和运维监控。

提供 Claude Messages、OpenAI Chat Completions 与 Responses 兼容接口。推理地址必须明确指定通道，`/{provider}` 与 `/{provider}/v1` 提供相同端点；根路径及统一 `/v1` 推理接口返回 404。

## 功能特性

- **四通道接入** — WorkBuddy、Qoder、Cline 和 Grok Build OAuth，后台集成官方授权流程。
- **兼容接口** — Messages、Chat Completions 与 Responses，按上游模型能力支持流式输出、推理内容及工具调用。
- **账号池调度** — 加权选择、并发限制、凭据续期、冷却与重试策略。
- **模型发现** — 从授权账号刷新真实目录，管理模型可见性和路由。
- **API Key 管理** — 创建与轮换 Key，设置模型权限、过期时间、请求频率、并发上限和用量预算。
- **运维仪表盘** — 请求速率、延迟、首 Token 时间、通道及模型视图、运行资源和告警规则。
- **日志与诊断** — 请求、上游尝试、错误、可选诊断采集及管理操作审计。
- **内嵌管理后台** — 响应式界面、明暗主题，随 Go 二进制发布，无需单独构建前端。
- **发行版更新** — 版本检查；符合条件的 Linux systemd 部署支持带守护校验及回退的在线升级。

## 支持的通道

| 通道 | 授权方式 | API 前缀 |
|------|----------|----------|
| WorkBuddy | 官方浏览器授权 | `/workbuddy/v1` |
| Qoder | 官方设备授权 | `/qoder/v1` |
| Cline | WorkOS 设备授权 | `/cline/v1` |
| Grok | Build OAuth 设备授权 | `/grok/v1` |

Grok 使用 **Build OAuth CLI 上游**。实际模型及能力取决于账号的上游权限；目录刷新失败保留既有观察结果，不以内置列表替代。

## 技术栈

| 组件 | 技术 |
|------|------|
| 后端 | Go 1.26.9+、HTTP 处理器、通道适配器 |
| 前端 | Go 模板、HTML、CSS、原生 JavaScript |
| 存储 | Redis，保存账号、配置、API Key、响应状态及运维数据 |
| 打包 | 单个二进制内嵌前端资源 |
| 主机部署 | Linux、systemd、Caddy 等反向代理 |

## 部署

### 方式一：源码运行

要求 Go **1.26.9+**（以 [go.mod](go.mod) 为准）、可用的 Redis，以及到所需上游的网络连接。

```bash
git clone https://github.com/zhangdailin/API-Console.git
cd API-Console

# 可选：启动带持久存储的本地 Redis。
docker run -d --name api-console-redis \
  -p 127.0.0.1:6379:6379 \
  -v api-console-redis:/data \
  redis:7 redis-server --appendonly yes

cp config.example.json config.json
go run ./cmd/server -config ./config.json
```

已有 Redis 时跳过 Docker 命令，在 `config.json` 中填写实际连接参数。Windows PowerShell 复制配置使用 `Copy-Item config.example.json config.json`。

构建独立二进制：

```bash
go build -o orchids-server ./cmd/server
./orchids-server -config ./config.json
```

Windows 使用 `go build -o orchids-server.exe ./cmd/server` 构建，以 `./orchids-server.exe -config ./config.json` 启动。

### 方式二：发行版二进制

从 [GitHub Releases](https://github.com/zhangdailin/API-Console/releases) 下载 Linux **amd64** 二进制、SHA-256 校验文件和构建元数据。将 [config.example.json](config.example.json) 复制为 `config.json`，与二进制一同放置。

```bash
sha256sum -c orchids-server-linux-amd64.sha256
chmod +x orchids-server-linux-amd64
./orchids-server-linux-amd64 --version
./orchids-server-linux-amd64 -config ./config.json
```

现有构建与部署脚本保留 `orchids-server` 文件名，项目名称为 API Console，支持通道如上表所示。

Linux 常驻服务、反向代理及后端端口保护见 [主机部署指南](deploy/README.md)。部署脚本参数可通过 `bash scripts/deploy-orchids.sh --help` 查看。

### 首次配置

1. 打开 **http://127.0.0.1:3002/admin/**。
2. 使用 `admin` 登录；`admin_pass` 留空时，启动日志会打印生成的密码。
3. 在账号页面通过对应通道的官方授权流程添加账号。
4. 在模型页面刷新该通道目录，选择可用模型 ID。
5. 在后台创建 API Key，并在显示完整 Key 时保存。
6. 为客户端填写网关地址、Key 和模型 ID。

默认端口及管理路径可通过配置调整。上游凭据用于账号授权，客户端使用网关签发的 API Key。

### 升级

后台可以检查发行版更新。安装升级或回退需要显式启用，并满足 Linux、root、非容器及持久 systemd 服务等条件。升级流程校验发行产物，并通过守护程序验证重启后的实际二进制。

详细条件与恢复步骤见 [在线升级](docs/online-upgrade.md)。升级前备份持久数据；二进制回退不会恢复 Redis 数据。

## API 接入

例如 Base URL 为 `http://127.0.0.1:3002/workbuddy/v1`，也可选择 `/qoder/v1`、`/cline/v1` 或 `/grok/v1`。对应的不带 `/v1` 通道地址提供相同端点。

| 方法 | 相对于 API 前缀的路径 | 用途 |
|------|-----------------------|------|
| `GET` | `/models` | 可见模型列表 |
| `GET` | `/models/{id}` | 模型详情 |
| `POST` | `/messages` | Claude Messages 兼容推理 |
| `POST` | `/chat/completions` | OpenAI Chat Completions 兼容推理 |
| `POST` | `/responses` | Responses 兼容推理 |

Grok 使用 Build 原生 Responses 路径，其他通道通过 Chat Completions 桥接。具体能力见 [协议能力矩阵](docs/coding-protocol-capabilities.md)。

### 查询模型

```bash
curl http://127.0.0.1:3002/workbuddy/v1/models \
  -H 'Authorization: Bearer <API_KEY>'
```

将 `<API_KEY>` 替换为后台创建的 Key，以下 `<MODEL_ID>` 替换为目录返回的模型 ID。

### Chat Completions

```bash
curl http://127.0.0.1:3002/workbuddy/v1/chat/completions \
  -H 'Authorization: Bearer <API_KEY>' \
  -H 'Content-Type: application/json' \
  -d '{"model":"<MODEL_ID>","messages":[{"role":"user","content":"你好！"}],"stream":true}'
```

### Claude Messages

```bash
curl http://127.0.0.1:3002/workbuddy/v1/messages \
  -H 'x-api-key: <API_KEY>' \
  -H 'Content-Type: application/json' \
  -d '{"model":"<MODEL_ID>","max_tokens":256,"messages":[{"role":"user","content":"你好！"}]}'
```

### Responses

```bash
curl http://127.0.0.1:3002/workbuddy/v1/responses \
  -H 'Authorization: Bearer <API_KEY>' \
  -H 'Content-Type: application/json' \
  -d '{"model":"<MODEL_ID>","input":"你好！","stream":true}'
```

响应资源、输入历史、取消、Token 估算与压缩接口见 [API 速查](docs/api-reference.md)。只有所有权信息的响应记录在本地取消时，不会取消上游生成。

## 配置与运维

- **配置优先级** — Redis 中 `<redis_prefix>settings:config` 可覆盖文件配置，修改后应核对后台实际值。
- **凭据备份** — Redis 数据与凭据加密密钥（默认 `data/credential.key`）须一起备份；密钥丢失后无法解密既有凭据。
- **API 鉴权** — 模型及推理接口要求托管 Key；`inference_auth_enabled=false` 不会关闭鉴权，只有 `anonymous_allow_ips` 显式允许的来源可免 Key。
- **网络访问** — 设置强管理密码，生产环境关闭 `debug_enabled`，限制 `/metrics` 和后端端口，并只信任实际反向代理地址。
- **监控口径** — 延迟百分位来自保留样本，采集健康及诊断覆盖单独展示，缺失数据不会表示为健康的零流量。

```bash
curl http://127.0.0.1:3002/health
```

## 开发

```bash
go test ./...
go vet ./...
node --test web/*.test.cjs
go build -o orchids-server ./cmd/server
```

Node.js 用于前端测试，程序运行无需 Node.js，也无需单独构建前端。

## 项目文档

| 文档 | 内容 |
|------|------|
| [API 速查](docs/api-reference.md) | 推理路由、管理接口、授权及错误状态 |
| [配置说明](docs/configuration.md) | 配置字段、加载优先级、代理及凭据存储 |
| [架构说明](docs/architecture.md) | 路由、通道适配、存储及模型发现 |
| [协议能力](docs/coding-protocol-capabilities.md) | 通道能力及验证边界 |
| [主机部署](deploy/README.md) | Linux 示例、反向代理及网络保护 |
| [部署说明](docs/deployment.md) | 备份、多实例及验证要求 |
| [在线升级](docs/online-upgrade.md) | 发行校验、升级、回退及恢复 |

## 参与贡献

欢迎提交 Issue 和 Pull Request。问题报告请提供复现步骤，提交改动前运行相关检查，并将真实上游行为与本地测试结果分开说明。

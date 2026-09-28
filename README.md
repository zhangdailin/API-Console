# API Console

[中文](README.md) | [English](README_EN.md)

Go 编写的多通道 API 代理，支持 WorkBuddy、Qoder、Cline 和 Grok Build OAuth。提供 Claude Messages、OpenAI Chat Completions 与 Responses 兼容入口；统一路径 `/v1` 按模型分发，也可使用 `/{channel}/v1` 指定通道。

## 快速开始

要求：Go **1.26.6+**（以 `go.mod` 为准）、Redis。先启动 Redis，例如：

```bash
docker run -d --name api-console-redis -p 6379:6379 redis:7
cp config.example.json config.json
go run ./cmd/server -config ./config.json
```

打开 `http://127.0.0.1:3002/admin/`，登录后通过管理页面的官方授权流程添加账号，再创建 API Key。若 `admin_pass` 留空，启动日志会打印随机管理密码。模型目录可在管理页面按通道刷新；未有可用账号时不会发布该通道模型。

```bash
curl http://127.0.0.1:3002/health
curl http://127.0.0.1:3002/v1/models -H 'Authorization: Bearer <API_KEY>'
curl http://127.0.0.1:3002/v1/messages \
  -H 'Authorization: Bearer <API_KEY>' -H 'Content-Type: application/json' \
  -d '{"model":"<MODEL_ID>","max_tokens":256,"messages":[{"role":"user","content":"你好"}]}'
```

`/v1/messages`、`/v1/chat/completions`、`/v1/responses` 均可按模型路由；各通道也提供对应前缀，模型查询使用 `/v1/models`。实际可用模型以管理端刷新后的目录为准。Grok 仅使用 Build OAuth CLI 上游；四个通道均可从管理端发起官方授权，无需在文档或页面粘贴登录凭据。

## 安全与运维

- 模型和推理接口要求管理端创建的 API Key（Anthropic 客户端也可用 `x-api-key`）；`inference_auth_enabled=false` **不会**关闭鉴权。只有显式配置的 `anonymous_allow_ips` 来源可免 Key，请谨慎限定范围。
- Redis 中的 `settings:config`（带 `redis_prefix` 前缀）会覆盖文件配置。账号凭据加密密钥 `data/credential.key` 必须与 Redis 数据一起备份；丢失后无法解密已有凭据。
- 生产环境设置强管理密码、关闭 `debug_enabled`、限制 `/metrics` 与后端端口的公网访问；配置可信反向代理时仅信任实际代理 IP。
- 验证：`go test ./...`；构建：`go build -o orchids-server ./cmd/server`。`orchids-server` 是当前构建与部署使用的二进制文件名，不代表项目的新名称。

详细信息：[API 与账号登录](docs/api-reference.md) · [配置说明](docs/configuration.md) · [部署文件与主机注意事项](deploy/README.md)。

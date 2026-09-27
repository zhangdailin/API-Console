# Qoder 协议兼容档与上线验证

## 配置

```json
{"qoder_protocol_profile":"skill-cli"}
```

- `reference`（默认）保持既有客户端布局与 api1 默认网关。
- `skill-cli` 使用参考 skill 的公开 clientID、api3 默认网关、四字段无 token runtime、cli business product，以及原账号 machineID 作为 MachineToken。
- 显式 `qoder_client_id`、`qoder_inference_base_url`、`qoder_client_version` 配置优先。
- 切换 profile 会使上游客户端缓存失效并重新派生 runtime；不复用旧 profile 的落库密文。
- COSY 签名继续使用现有 RSA 包裹密钥字段，与实际目录请求验证相符，不将 AES 明文 key 猜测性地替换到签名中。

此兼容档名是本项目的组合名称，不代表官方客户端型号认证。其 clientID 来自参考的 IDE 样本而请求表面来自其 CLI 说明，已完成已有账号的目录/推理验证，但不等于完成新的 OAuth 浏览器注册验证。已有账号保持原 machineID；新登录仍需人工授权及验证。

## 上下文语义

真实目录观测结构如下（非凭证）：

```json
{"context_config":{"1M":{"token_count":1000000},"200K":{"token_count":200000,"is_default":true},"400K":{"token_count":400000}}}
```

- 原始 `context_config` 及同 key 的配置变体都保存在快照中。
- 不从 `1M`、`200K` 标签猜数值，以 `token_count` 为准。
- `max_input_tokens` 保留其原始输入预算，不覆盖成最大档。
- 请求 `parameters.context_length` 选择唯一明确默认档；缺失/冲突时回退输入预算。
- 元数据区分默认输入预算、默认上下文档和最大已声明档；最大档不是授权或硬上限保证。

真实样本：Qwen 输入预算 180000、默认上下文 200000；Ultimate 输入预算 1000000、默认上下文 200000；Performance 默认上下文 272000。回归测试锁定这些区别。

## 验证与部署

本次本地 `go test ./...` 和 `go test -race ./internal/qoder ./internal/handler ./internal/config` 通过。生产部署使用显式 `GOOS=linux GOARCH=amd64 CGO_ENABLED=0` 构建并核对 SHA256，避免开发机架构混淆。

上线前备份二进制、配置、凭据加密密钥和 Redis 快照；原子替换二进制，健康失败自动恢复旧二进制。协议开关通过受认证配置 API 持久化。

已观测真实结果：两套 profile 都能读取目录；同账号直接推理也都曾返回 10605/p3、retryAfterSeconds=30；启用 skill-cli 后标准 Messages 请求曾 200 返回 OK，count_tokens 200 且包含 system 计数。再次请求发生等待超时，故不能宣称上游排队问题已经消失。

独立协议矩阵另外验证了 Messages 流式 200 且收到 message_stop 后正常 EOF、Chat Completions 200、强制工具调用 200 且产生一个 tool_use，以及工具结果回传后续回答 200。Responses 首次及一次独立复测均在客户端 95 秒期限内未完成，不能记为通过；同时段服务日志存在 10605 队列拒绝，但仅凭此不能证明 Responses 的所有延迟都来自队列。临时 API key 均已删除。

API 冒烟不等于真实桌面 Claude Code/Codex 验收；新 OAuth 登录和客户端交互仍需单独执行。上游也可能不严格执行 max_tokens，代理透传不能保证上游服从。

## 回滚

仅切回协议布局时，通过管理配置 API 将 `qoder_protocol_profile` 设为 `reference`，并恢复曾显式修改的覆盖项。

回滚整个二进制时使用上线前备份，原子替换后重启原 systemd 服务并检查 `/health`。不要直接恢复整个 Redis 快照：上线后 refresh token 可能已轮换，旧快照恢复会使已消费凭据重新出现。保留当前 Redis 和凭据加密密钥，除非明确执行一致性灾难恢复。

临时探针、真实账号目录/请求日志、密码、配置备份及API key不应提交到仓库；测试 API key 应在测试结束删除。

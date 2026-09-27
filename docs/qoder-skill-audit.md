# Qoder 实现与 qoder2api skill 对照审计

## 修复进度（后续实施）

以下正文保留原始审计快照，行号对应修复前源码。现已修复 `[DONE]` 提前结束、临时重试身份复用、HTTP 业务错误优先级、模型 key/别名/窗口一致性、生成参数透传、完整请求 token 估算、busy 重试标记及 Qoder 提前探测、盐持久化失败激活问题。正式回归覆盖位于 `internal/qoder/skill_regression_test.go`、`catalog_consistency_test.go`、`fingerprint_persistence_test.go` 及 handler 新增测试。

保留共享 429 映射与底层 30 秒等待安全上限。后续已增加 `qoder_protocol_profile=skill-cli`：整套选择参考 clientID、api3 网关、无 token runtime 和 cli 请求表面，默认 reference 向后兼容。已根据真实目录的 `context_config` 标签映射/`token_count`/`is_default` 实现档位保留与明确默认档声明，不再把默认输入预算当总上下文。OAuth 新设备浏览器授权仍需人工验证。

修复后验证：`go test ./...` 全仓通过；`go test -race ./internal/qoder ./internal/handler ./internal/config` 通过；原始 overlay 中 4 个 `TestSkillAudit` 均由失败转为通过；`git diff --check` 通过。已在用户授权环境备份部署并启用 skill-cli，健康检查及真实普通对话/计数成功；上游同时存在 10605 队列拒绝，不能把一次成功视为稳定性保证。真实桌面 Claude Code/Codex 尚未启动，接口冒烟与桌面冒烟应区分。

## 范围与结论

检查本地 Qoder 上游、登录/认证、目录以及共享 handler。未修改生产代码、未读取真实账号凭证、未请求真实 Qoder 服务。参考并非官方协议规范，版本差异与确定性代码缺陷分开记录。

参考：
- https://github.com/broken-air/qoder2api-skill/blob/main/SKILL.md
- https://github.com/broken-air/qoder2api-skill/blob/main/references/qoder-protocol.md
- https://github.com/broken-air/qoder2api-skill/blob/main/references/api-protocols.md

整体：编码、双层 SSE、原生工具历史、401 单次刷新及凭证轮换已有实现，但还不能按 skill 的端到端标准判定通过。

## 优先问题

### P1：把 [DONE] 当最终结束，丢失尾部 usage

位置：`internal/qoder/stream.go:333-335,401-403`。

裸 `[DONE]` 和信封内 `[DONE]` 都立即结束读取。参考明确 Qoder 的最终标志为 `event:finish`。给定文本 → `[DONE]` → usage → finish，测试实际得到 Usage=nil；后续错误也会被漏读。

建议：区分中间 done 与最终 finish，继续处理尾部 usage/错误；补充中间 done 后 EOF 的策略测试。当前 `stream_test.go:124-137` 反而将 done 单独结束锁定为成功，需一并校正。

### P1：本地临时错误重试复用请求身份

位置：`internal/qoder/client.go:312-317`；对照正确的 401 修复分支 `300-306`。

5xx/provider_error 后直接 continue，正文 request_id 和 is_retry 均未变。模拟服务测试实际捕获第二次 request_id 与第一次相同、is_retry=false。若首次请求已到达网关，可能触发其去重保护；本次未验证真实 403/103。

建议：每次实际重试统一生成新 UUID、重建正文与签名，设 is_retry=true，保留逻辑会话关联。

### P1：HTTP 与流内业务错误分类不对称

位置：`internal/qoder/request.go:818-822`，对照 `internal/qoder/stream.go:365-385`。

HTTP 401 携带 agentLimitResetTime 会直接标为鉴权失败，流内同样业务内容却正确识别为 agent allowance deadline。测试实际确认 isUnauthorized=true。可能无效轮换仍有效的 refresh token，并在后续错误处理中误处置账号。

建议：HTTP 与 SSE 共用先业务码、后 HTTP 状态的分类器；同时覆盖 duplicate request、内容策略和客户端参数错误。

### P2：目录 key 与别名取到不同配置，展示窗口也可能错配

位置：`internal/qoder/catalog.go:212-215,293-304`。

目录全部 append，byKey 后行覆盖，byName 首行保留；同名展示窗口又取所有行的最大值。测试输入同 key/name 的 180000 与 900000 两行，实际 key 解析为 900000、名称解析为 180000。

建议：先归并 key 及档位，再建立一致的名称映射；窗口从实际解析目标读取。不能据内部重复直接断言公开 models 列表必然重复。另 modelEntry 不保留 context_config，无法区分参考中的默认输入预算与上下文档位。

### P2：输出预算等请求参数未贯通

位置：`internal/upstream/types.go:6-34`，`internal/qoder/request.go:200-205`。

共享 UpstreamRequest 无 max_tokens / temperature / top_p / stop 字段，Qoder 正文固定 max_tokens=32768。用户给定的输出预算无法由这条上游链路透传，可能导致额外生成、成本和延迟。

建议：先补共享表示和各入口转换，再按 Qoder 支持的参数透传；明确区分未提供与零值。

### P2：count_tokens 只估最后一条 user 文本

位置：`internal/handler/count_tokens.go:50-51`、`internal/handler/utils.go:111-120`。

计数输入来自 extractUserText，只取最后一条有文本的 user 消息，再加工具定义。忽略顶层 system、之前对话等；这是共享问题，同样影响 Qoder。长系统提示/长历史 + 最后一条 hi 会严重低估上下文。

建议：允许启发式估算，但必须覆盖整个实际请求及工具结果等内容。

## 与 skill 不同的既有策略

- 忙码等待：`internal/handler/handler_helpers.go:720-736` 明确按 1/16、1/4、3/4 提前探测，而非遵守完整 RetryAfter；30 秒提示第一轮基准仅 1.875 秒，另加 jitter。这是现有代码刻意策略，不是遗漏，但与 skill 契约冲突。
- 共享重试会重新进入 SendRequestWithPayload 并生成新 ID，但 `internal/qoder/request.go:242` 仍固定 is_retry=false。
- 最终限流走共享 429 而非 skill 建议给 Claude Code 的 529。429 本身是合法限流响应，是否按协议面改 529 应经客户端验证，不宜全局替换。

## 需要真实客户端/网关证据确认的差异

当前默认 api1 vs 参考 api3；默认 CLI clientID vs 参考 IDE clientID；OAuth 自动随机 machineID vs 参考要求已注册设备；business product=ide vs cli；派生 MachineToken vs machineID。配置和客户端版本不同可能解释差异，不能逐字段照抄混用。

当前生产走 referenceRuntimeFieldsFor，身份包含 access/refresh token；参考描述的是另一套无 token 身份布局。签名第二项当前使用 RSA 密文形式 fields.Key，参考 runtimeKey 术语存在歧义。应固定目标客户端版本，用权威抓包/测试向量核验完整布局，而不是仅凭文章判为鉴权错误。

附加稳定性风险：`internal/qoder/fingerprint.go:83-95` 读取盐失败被视为空值并生成新盐，写入失败仍启用；重启后可能改变全账号派生设备指纹。应区分不存在与读写错误，并在持久化成功前不要替换已有效身份。

## 已有正确实现

- 自定义 base64、补位与首尾三分之一交换；签名覆盖实际编码后发送字节。
- signedPath 去掉 /algo 前缀及 query。
- PKCE、不带 redirect_uri、poll 404 当待授权。
- 401 单次修复预算，修复分支更换 ID；凭证轮换有生产原子 patch 路径。
- SSE 双层解包、文本限流首段缓冲、工具历史与参数拼接、finish_reason 映射。
- 正常 finish_reason 后仍继续读取后续数据；问题特别发生在 [DONE] 分支。

## 验证记录

现有测试命令通过：

```sh
go test ./internal/qoder ./internal/accountpolicy ./internal/errors ./internal/api ./cmd/server
```

新增隔离审计用例，未加入生产包文件：

- `.tools/qoder-skill-audit/audit_test.go`
- `.tools/qoder-skill-audit/overlay.json`（路径绑定当前工作目录）

```sh
go test -overlay=.tools/qoder-skill-audit/overlay.json ./internal/qoder -run '^TestSkillAudit' -count=1 -v
```

四个反例测试均如预期失败，证明当前行为：

1. TestSkillAuditUsageAfterDone：late usage lost，Usage=nil。
2. TestSkillAuditTransientRequestIdentity：reused request ID=true，is_retry=false。
3. TestSkillAuditHTTPAgentLimit：allowance deadline misclassified as authentication error。
4. TestSkillAuditCatalogAliasConsistency：key=900000，alias=180000。

执行过程中初次 /tmp overlay 在 Bash 环境不可见；改为工作区 overlay 后遇到编译缓存只读，获得更宽权限后完成上述复现。反例失败属于产品行为断言失败，不是编译失败。

尚未完成 skill 要求的真实 Claude Code/Codex 对话、工具多轮和流式结束冒烟；不能将 mock/单测通过等同真实网关兼容。优先修复前三项，再验证目录、参数和完整客户端链路。

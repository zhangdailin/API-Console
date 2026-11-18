# Warp 渠道（有界文本与实验性客户端工具）

本实现接入当前 `provider.Spec` 注册体系，不是整体回退历史删除提交。文本协议已通过真实验收；工具声明和受限续接属于实验能力，尚未通过真实工具闭环。

## 支持范围

- 管理后台「账号管理 → Warp → 添加账号」发起官方设备授权。
- 管理端事务接口：`POST /api/warp/login`、`GET /api/warp/login/{id}`、`DELETE /api/warp/login/{id}`，仅管理员会话或管理身份可用，不能用推理 Key 代替。
- 官方设备授权结果在服务端交换 Firebase 访问/刷新凭据；设备私有码、项目配置和令牌不返回浏览器。
- 访问/刷新凭据以 Warp 专用字段加密保存；刷新采用每账号串行门、最新账号读取和乐观条件更新，避免旧快照覆盖已轮换凭据。
- 只发布当前启用账号实际读取的 GraphQL `featureModelChoice` 目录，合并 `agentMode`、`planning`、`coding`、`cliAgent`、`computerUseAgent` 五个平面的有效模型，不提供静态兜底，不发布自动路由或子代理别名。上下文窗口读取模型条目直属的 `contextWindow`，不是 `spec.contextWindow`。
- 推理入口为 `POST /warp/v1/chat/completions`、`POST /warp/v1/messages`、`POST /warp/v1/messages/count_tokens`、`POST /warp/v1/responses` 及 `/warp/v1/responses/{id}` 等资源子路径，`/warp` 前缀同样可用；Messages 与 Responses 由共享 Chat 桥接提供，目录为对应 `/models`。所有推理/目录请求仍须托管 API Key。

## 明确不支持

Selection freshness: routing checks each account's exact model and catalog before
constructing an inference client. Ready accounts take priority. Only when no
ready account can serve the model does selection refresh stale candidates, at
most three accounts within a shared 15-second budget (10 seconds per account).
Refresh reads the catalog and, if needed, rotates authentication; it never calls
generation. A per-account process-local gate re-reads stored state so concurrent
successful refreshes coalesce. Failed refreshes cannot authorize a stale catalog.
Catalog persistence remains credential-generation guarded; a concurrent disable
or new login is authoritative. Background maintenance uses the same gate and
refreshes one minute ahead of the hard 30-minute catalog boundary. An expired
catalog that cannot be refreshed is a capacity failure, not a client-model 400.

Model mismatch diagnostics retain the requested model, fallback flag, mismatch
reason, and event/action/message ordinals. Reported model IDs are recorded verbatim
only if present in the account's observed catalog (or equal to the requested ID);
unknown values retain only SHA-256 and byte length, never arbitrary upstream text.
These fields appear in the private diagnostic bundle and structured warning, not
the public response. Upstream model substitutions and `is_fallback=true` are
accepted, including cross-provider fallbacks, as long as the complete response
passes the remaining protocol, tool, usage, and size checks. The gateway still
sends the requested exact model and does not replay or switch accounts. Public
response model labels, API-key model admission, pricing, and continuation scope
remain tied to the requested model; they are not proof of the actual model used.
Fallbacks can have different capabilities or upstream costs, and missing usage
is not proof of zero charges.

当前接收 `model`、`messages`、`stream`、`tools`、`tool_choice`、`parallel_tool_calls`，并兼容客户端常带的 `system`、生成控制、`metadata.user_id`、`thinking` 和 `output_config.effort`。纯文本 system 可来自独立字段及消息任意位置，按独立字段在先、消息出现顺序在后的顺序合并并保留原文，不再设置 system 累计字节上限，完整保留、不静默截断；system 前缀也不计入非 system 历史的64 KiB预算，避免相同内容在下一层再次被限制。用户新消息仍限4096字节，非system历史最多8轮、64 KiB；通用HTTP请求体及上游模型上下文窗口仍可能限制最终请求大小。客户端函数工具不再设置数量上限，完整保留顺序和定义、不截断列表；单个描述32 KiB、单个schema64 KiB的独立限制仍保留，schema累计总量不再限制；不再限制完整工具定义（描述、名称、schema及JSON封装）累计总量，超过原160 KiB也会完整传递，不静默截断。其他独立上限继续返回固定字段路径及实测/上限字节数。工具定义变大可能增加上游输入用量，不构成免费或硬性费用上限。

assistant 历史可携带 `thinking` 块（`thinking` 字符串和可选 `signature` 字符串），单块及全部思考/签名合计最多64 KiB。它们作为历史兼容元数据接收，不转成普通提示词、工具输入、可见回答或可迁移的提供者签名；纯 thinking 且无文本/工具调用的 assistant 消息仍拒绝。`context_management` 的 keep-all 仅指不在网关执行上下文清理，不保证把原提供者的思考内容/签名转移给 Warp。文本和工具对照关系仍保留；结果无原有归属绑定或已消费时返回409，不自动重放或切号。

生成控制、`thinking.display` 和 `output_config.effort` 只做线路兼容，不转发到 Warp，也不改写精确模型。合法文本块与user角色的`tool_result`外层可携带 `cache_control:{type:"ephemeral",ttl?:"5m"|"1h"}`，`tool_use`和tool_result内部文本项不因此放宽。工具结果正文、调用ID和is_error完整保留；缓存标记及 TTL 不转发、不承诺生效或命中；实际缓存仅以上游用量为准。`context_management.edits` 只接受空列表或单个 `{type:"clear_thinking_20251015",keep:"all"}` 不清理提示，其他上下文编辑拒绝。`safeguards` 仅接受空列表，非空明确拒绝：网关没有可保留其权限/分类语义的上游协议，不能静默移除。回答仍由网关的字节上限约束。不支持的字段（如 conversation_id、response_format、reasoning_effort、include、mcp_servers、图像/文件内容）返回 400，不静默丢弃后收费。Messages 与 Responses 由共享 Chat 桥接提供，`/responses/compact` 可用。

Client compatibility: `store: false` is accepted; `true` is rejected because stored Chat responses are unavailable. Configured diagnostic retention remains in effect. `stream_options` accepts an empty object or a boolean `include_usage`; Chat keeps its existing terminal usage output. Anthropic tool declarations accept a boolean `eager_input_streaming` hint while the gateway continues to deliver complete, validated tool arguments. These hints are not forwarded to Warp. Null, incorrectly typed, and unknown options return 400 before credential refresh or generation.

Text-only `developer` messages join the instruction prefix in message order, after any separate `system` field. Adjacent plain-text `user` messages are joined in order with two newlines as one user turn; user context stays user content. The last original user message retains its 4096-byte limit, while the joined context, labels, and recent dialog share the existing 64 KiB transcript budget. Tool-bearing messages are not joined, and tool-result ownership, consumption, and paid-turn checks remain enforced. Regression fixtures cover both route prefixes, Chat/Messages JSON and SSE, 111 tools, and synthetic 30 KB context fragments; this is local mock evidence, not live upstream validation.

这意味着 Codex（Responses）与 Claude Code（Messages）均可携带客户端工具接入，但不要把本渠道标记为完整 Agent/编程客户端兼容：生成控制和部分编程客户端特性仍不受上游支持。

```http
POST /warp/v1/chat/completions
Authorization: Bearer <managed-api-key>
Content-Type: application/json

{"model":"<本账号目录中的明确模型ID>","messages":[{"role":"user","content":"Reply with OK."}],"stream":false}
```

## 成本保护

- 精确使用目录模型，不把 auto/auto-efficient/auto-genius 等改写为 auto-open；上述自动模型和 CLI/computer-use 子代理路由不发布且拒绝调用。
- 仅携带有界历史和客户端显式声明的函数工具，不注入环境文件或动态时间戳，不自动加载 MCP 服务器。工具续接状态仅在已验证归属的会话中私密保存和回传；不会把不支持的请求改写为另一份收费生成。
- 关闭搜索、网页检索、规划、编排、电脑、文件和命令等能力。不配置额外模型角色。
- An empty protocol tool list permits arbitrary tools: requests without tools retain the passive suggestion fence; requests with tools advertise only `CALL_MCP_TOOL`. Undeclared tools, native execution tools, subagents, and unknown protocol fields still terminate the response. Model substitutions and fallback flags are diagnostic-only and do not terminate it.
- 官方客户端把 `ToolCall.Server` 与 `ServerEvent` 视为无需客户端执行的不透明簿记，新的根任务也会以服务端引导调用开启；这些变体不执行、不解释、不回显且不产生文本，并计入诊断计数。`ToolCallResult.Server` 仅在已创建的同根任务中、与先前已观测的 `ToolCall.Server` 调用ID配对且结果载荷≤64 KiB时兼容；结果必须通过原始任务状态、事务回滚及掩码后的有效消息校验。官方不透明消息会在私密续接状态中原样保存/往返，不作为客户端工具动作。未知、跨任务、未配对及native结果仍拒绝。这不等于上游未做服务端工作或未计费：流中一旦出现搜索/抓取计数或费用、执行类工具、其他代理消息，仍整体失败。
- SSE 事件名不作为安全边界：按官方客户端契约只把 `data` 解码为 protobuf，`event: error` 仍立即失败；事件标签任意但数据必须能严格解码且无未知字段。
- Failures expose only fixed stages (transport / http_status / content_type / stream_protocol / tool_or_subagent / incomplete / finish / limit / usage_invalid) and enum numbers. Private diagnostics retain fixed rejection sites/reasons, event/action/message ordinals, result variants, identity-presence/equality flags, relationship categories, and byte counts; raw IDs, payloads, prompts, credentials, and upstream bodies are not logged. Model discrepancies retain the separately redacted metadata described above, including when another validation fails.
- 单次上游生成，不自动重试、不自动切号；认证刷新不计为生成重试，实际生成遵守共享尝试预算。
- 多轮历史按 8 轮 / 64 KiB 截断，超出部分丢弃最旧内容并标注省略；工具往返每会话最多 6 次，达到上限返回 400，避免代理循环无限计费。
- 客户端工具只以 MCP 工具声明下发，上游只能回调这些工具（`supported_tools` 仅 `CALL_MCP_TOOL`），客户端执行后以结构化 `ToolCallResult` 回传；网关从不自行执行 shell、文件或 MCP 操作。
- 生成最多等待 120 秒；上游流最多 8 MiB、单帧 1 MiB、回答文本 16 KiB。直到确定非空 `Finished.Done` 才下发回答，避免把已拒绝工具流的草稿当结果。

**依然调用 `/ai/multi-agent`，不是已验证的普通聊天端点。** 本地禁用选项、超时和断开连接不构成上游硬性收费上限，也不保证服务器即时停止活动。单轮稳定前缀不等于已验证缓存命中。

上游返回的 token/cache/费用仅是实际观测组成部分，不把缺失用量标成 0 或免费，不以 conversation 累计账单充作单次账单。配额/套餐未观测时在后台显示未知，不编造剩余额度。普通 API Key 的美元价格结算仍依赖管理员模型定价；上游 credits 不自动等同美元计费限额。

### 用量口径

新式 `request_charges` 的普通输入、缓存读取、缓存写入是独立观测桶；原始诊断保留原值。内部审计、账号统计与 Chat/Responses 的输入为三者之和，缓存读取另列在 details 中；Messages 保留普通 input_tokens、cache_read_input_tokens、cache_creation_input_tokens 独立桶。总量只再加输出，不把缓存重复相加。legacy total_input 不按独立桶重新解释，其他渠道沿用其已有口径。

例如普通输入130、cache-read26892、cache-write64、输出506：内部/Chat/Responses输入27086、总量27592；Messages输入130、cache-read26892、cache-write64、输出506。只有各计费贡献项所需字段完整且算术安全的观测才可作为真实用量结算；缺失保持未知、不以估算补齐收费，溢出/非法负费用在交付内容前失败。API Key结算沿用已配置的模型价格，cache-write当前按普通输入价格而非单独的写缓存溢价计算；不是Warp实际积分的换算。历史日志不会追溯重写。

## 额度与模型消耗倍率

账号刷新、登录后的检查及周期维护会读取官方 `GetRequestLimitInfo` 的包含额度。个人积分池与每个团队工作区的共享月度积分池分别保存和展示，不汇总为个人余额；当前不包括额外奖励/预购或 PAYG 余额。零值是实测零，缺失是未知，额度查询失败保留上次值并显示刷新失败。这些显示数据暂不作为路由或停用依据。

模型管理只在上游 `usageMetadata.creditMultiplier` 有有效数值时显示紧凑的近似积分倍率 `~N×`（不一致时可为范围）。未报告真实积分倍率时整块隐藏，不显示“未知”、旧 `requestMultiplier`、来源/账号数量或说明占位；后台仍保留原始观测，不将缺失默认成 `1×` 或免费。

真实验收（2026-11-16）已通过普通 Chat JSON/SSE、system+多轮记忆、Messages JSON 和 Responses JSON/SSE。工具声明请求可返回200，但实测模型未调用所声明的纯回显工具，工具回传仅在本地两步 mock 中通过，不能标记为真实可用。短Haiku请求上游仍报告约1.17万cache-write tokens和约0.81积分，短用户提示并不等于低成本或缓存命中。

倍率来自账号套餐/工作区的目录观测，不是固定每次调用费用、美元单价或 Token 价格；实际费用仍受输入/输出、缓存、时间及平台计费影响。模型目录刷新不会发起生成，倍率展示不修改管理员模型价格或 API Key 预算。

## 配置

### Authentication refresh behavior

Firebase ID tokens refresh five minutes before their recorded expiry. Each authentication exchange has a five-second timeout. With all three endpoints left at their official defaults, a transient Firebase failure may fall back once to Warp's official `/proxy/token` or `/proxy/customToken` endpoint. Explicit custom endpoints never enable this fallback or forward their credentials to production. Redirects are not followed.

Firebase error bodies are decoded independently of HTTP status. `TOKEN_EXPIRED`, `INVALID_REFRESH_TOKEN`, and `MISSING_REFRESH_TOKEN` require a new official login; `USER_DISABLED` and `USER_NOT_FOUND` identify an unavailable upstream account. These terminal verdicts do not use proxy fallback. Their routing status is persisted only if the rejected access token, refresh token, and expiry still match, so an old response cannot invalidate a concurrent login replacement.

Transient authentication failures retain the existing credentials and do not request reauthentication. Clients sharing the same account store within one process share a bounded failure cache: retries start after 30 seconds and back off exponentially to five minutes, with bounded `Retry-After` seconds or HTTP-date hints taking precedence. Success or credential replacement clears the corresponding suppression. Cancellation is not cached. Retry suppression is process-local, not a distributed refresh lock; terminal routing status is durable in Redis. Authentication fallback never replays a generation request or expands its single-generation attempt budget.

默认无需手动配置：服务端从固定官方公开源码提交获取 Warp 客户端的 Firebase 项目配置，严格校验单个配置字段、内存缓存，不执行下载内容，不输出或持久化该值。

可选覆盖环境变量：

```text
ORCHIDS_WARP_FIREBASE_API_KEY=<Warp 对应的 Firebase 项目配置>
```

不从历史代码恢复旧密钥，不通过管理响应回显。Go 内部测试可设置 `Config.WarpFirebaseAPIKey`（不进入 JSON）。该公开项目配置不是账号 Token。固定配置来源读取失败时明确停止，错误不携带内容；自定义端点不自动取得生产默认配置，测试或自定义端点必须显式设置配置，防止把默认值传给其他主机。

以下端点配置可选，默认使用生产服务；测试必须指定本地 mock：

| 配置 | 默认 |
|---|---|
| `warp_base_url` | `https://app.warp.dev` |
| `warp_firebase_base_url` | `https://identitytoolkit.googleapis.com` |
| `warp_token_base_url` | `https://securetoken.googleapis.com` |

配置端点具有接收凭据的能力，应只由可信管理员管理。HTTP 重定向不自动跟随。仅当使用默认官方端点且无显式覆盖时才加载官方公开配置；配置加载失败不会继续发起设备授权。

账号只能通过官方登录创建/更新凭据，不支持粘贴导入，也不导出可恢复的 Warp 凭据。重新登录按身份合并，保留管理员停用、权重等设置，不自动启用已有停用账号。

## 验证与上线边界

本地 mock 回归覆盖正式路由、单轮 JSON/SSE、未知控制拒绝、单次生成尝试、加密/脱敏、旋转凭据恢复、目录失败不回退、重新登录保留管理员设置等。mock 通过不等于上游当前接受协议。

真实设备登录、账号目录/包含额度和文本生成已经验证；上述结果只覆盖验收账号、模型和请求样本，不承诺所有模型及客户端兼容。客户端工具调用/回传尚未真实闭环，不能把 mock 成功或 HTTP 200 当作工具可用的证明。继续验收应限制请求次数、明确工具服务器/调用路径，并在收到真实工具调用后才回传结果。

临时探针、生产账号诊断代码、凭据和部署产物不属于本项目发布源码。回归默认使用本地 httptest 与隔离存储，不自动调用真实上游。

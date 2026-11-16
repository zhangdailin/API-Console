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

当前接收 `model`、`messages`、`stream`、`tools`、`tool_choice`、`parallel_tool_calls` 六个顶层字段，并兼容 Anthropic / OpenAI 客户端常带的 `system`、`max_tokens`、`max_completion_tokens`、`temperature`、`top_p`、`top_k`、`stop`、`stop_sequences`、`metadata`、`thinking`。消息内容必须是纯文本（字符串或 `type=text` 内容块），最多一条 system 指令（≤8192 UTF-8 字节）。支持有界多轮历史（最多 8 轮、64 KiB）以及 assistant `tool_calls` / `tool` 结果；客户端函数工具（OpenAI `type=function` + `function` 或 Anthropic `name` + `input_schema` 形状）作为 MCP 工具转发。

生成控制只做线路兼容，不转发到 Warp（该端点没有对应语义），回答仍由网关的字节上限约束。不支持的字段（如 conversation_id、response_format、reasoning_effort、include、mcp_servers、stream_options、图像/文件内容）返回 400，不静默丢弃后收费。Messages 与 Responses 由共享 Chat 桥接提供，`/responses/compact` 可用。

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
- 协议中空工具列表表示允许任意工具：无工具请求使用被动建议围栏；有工具请求仅声明 `CALL_MCP_TOOL`。收到未声明工具、原生执行工具、子代理、模型替换或不理解的协议字段立即终止。
- 官方客户端把 `ToolCall.Server` 与 `ServerEvent` 视为无需客户端执行的不透明簿记，新的根任务也会以服务端引导调用开启；这些变体只跳转、不执行、不解释、不回显且不产生文本，并计入诊断计数。这不等于上游未做服务端工作或未计费：流中一旦出现搜索/抓取计数或费用、执行类工具、其他代理消息，仍整体失败。
- SSE 事件名不作为安全边界：按官方客户端契约只把 `data` 解码为 protobuf，`event: error` 仍立即失败；事件标签任意但数据必须能严格解码且无未知字段。
- 失败只暴露固定阶段（transport / http_status / content_type / stream_protocol / tool_or_subagent / incomplete / finish / limit / model_substitution / usage_invalid）与枚举编号，不记录提示词、账号凭据或上游正文。
- 单次上游生成，不自动重试、不自动切号；认证刷新不计为生成重试，实际生成遵守共享尝试预算。
- 多轮历史按 8 轮 / 64 KiB 截断，超出部分丢弃最旧内容并标注省略；工具往返每会话最多 6 次，达到上限返回 400，避免代理循环无限计费。
- 客户端工具只以 MCP 工具声明下发，上游只能回调这些工具（`supported_tools` 仅 `CALL_MCP_TOOL`），客户端执行后以结构化 `ToolCallResult` 回传；网关从不自行执行 shell、文件或 MCP 操作。
- 生成最多等待 120 秒；上游流最多 8 MiB、单帧 1 MiB、回答文本 16 KiB。直到确定非空 `Finished.Done` 才下发回答，避免把已拒绝工具流的草稿当结果。

**依然调用 `/ai/multi-agent`，不是已验证的普通聊天端点。** 本地禁用选项、超时和断开连接不构成上游硬性收费上限，也不保证服务器即时停止活动。单轮稳定前缀不等于已验证缓存命中。

上游返回的 token/cache/费用仅是实际观测组成部分，不把缺失用量标成 0 或免费，不以 conversation 累计账单充作单次账单。配额/套餐未观测时在后台显示未知，不编造剩余额度。普通 API Key 的美元价格结算仍依赖管理员模型定价；上游 credits 不自动等同美元计费限额。

## 额度与模型消耗倍率

账号刷新、登录后的检查及周期维护会读取官方 `GetRequestLimitInfo` 的包含额度。个人积分池与每个团队工作区的共享月度积分池分别保存和展示，不汇总为个人余额；当前不包括额外奖励/预购或 PAYG 余额。零值是实测零，缺失是未知，额度查询失败保留上次值并显示刷新失败。这些显示数据暂不作为路由或停用依据。

模型管理只在上游 `usageMetadata.creditMultiplier` 有有效数值时显示紧凑的近似积分倍率 `~N×`（不一致时可为范围）。未报告真实积分倍率时整块隐藏，不显示“未知”、旧 `requestMultiplier`、来源/账号数量或说明占位；后台仍保留原始观测，不将缺失默认成 `1×` 或免费。

真实验收（2026-11-16）已通过普通 Chat JSON/SSE、system+多轮记忆、Messages JSON 和 Responses JSON/SSE。工具声明请求可返回200，但实测模型未调用所声明的纯回显工具，工具回传仅在本地两步 mock 中通过，不能标记为真实可用。短Haiku请求上游仍报告约1.17万cache-write tokens和约0.81积分，短用户提示并不等于低成本或缓存命中。

倍率来自账号套餐/工作区的目录观测，不是固定每次调用费用、美元单价或 Token 价格；实际费用仍受输入/输出、缓存、时间及平台计费影响。模型目录刷新不会发起生成，倍率展示不修改管理员模型价格或 API Key 预算。

## 配置

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

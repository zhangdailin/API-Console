# 协议能力与兼容边界

## 1. 判断原则

此文档针对网关实现，不承诺完整复刻官方 API。协议可解析、字段已映射、上游接受请求、模型遵守约束、完整客户端可用，是五个不同层次。

当前没有运行完整编程客户端的全模型认证结果；历史 HTTP 协议探针不能扩展成所有版本客户端均兼容的声明。

## 2. 路由矩阵

| 能力 | WorkBuddy / Cline | Grok Build |
|---|---|---|
| Chat / Messages | 共享 Handler，通道分别构造上游请求 | 转换为 Build 请求再投影 |
| Responses | Chat 桥接 | Build 原生 Responses |
| 文本 SSE | 共享流状态机与 Responses 事件转换 | 原生转发或 Chat / Messages 转换 |
| 函数工具与结果续轮 | 标准协议转换，名称与参数保留 | 原生声明与调用透传 |
| store / previous_response_id | 共享响应存储，展开历史 | 绑定 Key、原账号与原生资源策略 |
| GET / DELETE 响应 | 本地存储资源 | 所有权验证后原生 / 记录路径 |
| cancel / input_items | 共享所有权接口 | 共享记录接口；不承诺取消上游 |
| compact | 网关摘要引用 | 密封网关摘要或原生路径，按配置决定 |
| 精确 Token 计数 | 无；本地估算 | 无；本地估算 |
| WebSocket 服务 | 未实现 | 未实现 |

三个通道的 `/{provider}` 与 `/{provider}/v1` 均提供上述已实现能力及模型目录；根路径和统一 /v1 推理入口已删除。路径能力一致不代表各上游协议能力相同。

## 3. 字段与降级

| 控制 | 当前行为 |
|---|---|
| text.format / response_format | 转换为对应 Chat 结构；Build 按原生适配 |
| strict:true | 共享渠道补 schema 提示并校验已支持规则；不匹配失败 |
| prompt_cache_key | WorkBuddy / Cline 按适配传递 |
| text.verbosity | WorkBuddy / Cline 接受但不转发（这些上游无对应字段，`text` 本就不发送）：它是回答长度提示而非契约，Codex 每轮都会携带，拒绝会使 Codex 在这些渠道不可用；不声称模型遵守 |
| text 中其他无等价字段 | 共享传输校验拒绝，不声称模型遵守 |
| include:reasoning.encrypted_content | Chat 桥接消费该输出投影请求并返回兼容警告，不伪造密文 |
| 其他 include | 无等价共享传输时拒绝 |
| 桥接 web_search / preview | 删除托管搜索并提示不可用；强制搜索且无法满足时拒绝 |
| Anthropic 服务端搜索工具（如 Claude Code WebSearch 的 `web_search_20250305`） | WorkBuddy / Cline 在选号前以 400 拒绝并提示改用 `/grok` 路径：这些上游不能执行搜索，此前降级为普通函数会让模型“调用”一个无人执行的工具而静默无结果。名为 web_search 的客户端普通函数（`function` / `custom`）不受影响；Grok 原生支持 |
| 其他托管工具 / MCP | 共享桥接拒绝；Build 使用原生格式，能力取决于上游 |
| 外部加密推理历史 | Chat 桥接拒绝；Build 按原生状态策略 |
| background=true | 不提供网关任务队列语义；桥接无等价能力，原生仍取决上游 |
| stop | 支持的路径实行停止规则；Grok 转换 stop 可跨帧处理 |
| max_completion_tokens / max_tokens | 显式值保留；前者优先；各通道有预算转换 |

兼容警告可能使用历史名称 `X-Grok2API-Compatibility-Warnings`，不能从 Header 名判断当前存在 Grok Web 通道。

WorkBuddy 将 `11128` 渠道校验拦截与 `11140` 账号受限分开处理。提示词兼容层参考 [issue #680](https://github.com/ardeyouxipianyi/workbuddy2api-hub/issues/680)，仅在 system/developer 和 assistant 正文中，将独占一行的 DSH 固定句 `Tokens prefixed with @ are paths the user explicitly referenced.` 改写为 `Tokens prefixed with @ are paths that the user explicitly referenced.`；用户正文、工具参数及工具结果不改写。这是窄范围协议兼容，不保证上游继续接受该客户端。

`11140` 保存独立的 `workbuddy_restricted_until`，沿用原有 24 小时冷却。期间聊天、后台目录/额度刷新和手动验证均跳过该账号；凭据刷新、账号编辑或目录接口成功不能清除限制，重复触发也不会延长当前窗口。旧 `403/11140` 记录从原 `last_attempt` 推导截止时间，无需重新起算。客户端收到 `403 account_restricted`，区别于会话失效的 401；本地冷却结束不代表上游已解除限制。

Anthropic 请求的 `thinking: {"type":"disabled"}` 视为显式关闭推理，在 WorkBuddy、Cline 上按 `reasoning_effort: "none"` 处理，优先于 `output_config.effort`（与 Grok 原生路径一致）；OpenAI 字段 `reasoning_effort` 仍最优先。WorkBuddy 收到 `none` 时省略该字段；Cline 原样发送，上游拒绝后去掉该字段重试一次。

Cline 在尚未向客户端输出前，对三类可修复的上游拒绝各本地重放一次：历史 assistant 缺 `reasoning_content`（GLM 系）时补占位符 `"."`；`reasoning_effort` 被拒时去掉该字段；网关默认 `max_tokens` 被拒时降到 32000（客户端显式值不改）。超过 64 字符的工具名按"前 55 字节 + sha256 前缀"确定性缩短，返回时还原。`/auth/refresh` 拒绝（400/401/403）时先用 WorkOS `refresh_token` 续期并经 `/auth/register` 换回 Cline 凭据，两者都拒绝才要求重新登录。

2026-10-31 用生产账号对真实上游复核：`reflection/beam-free` 对缺 `reasoning_content` 的历史返回 200 + 首帧 `stream_initialization_failed` 错误（报错原文前有约 250 字符包装，修复按未截断原文判断），补 `"."` 后正常；65 字节工具名同样以 200 + 错误帧拒绝，64 字节通过；网关客户端端到端（缺推理历史、`none`、超长工具名）成功并还原原名。当时四个免费模型都接受 `reasoning_effort: "none"` 和 `max_tokens: 128000`，这两类修复暂未被触发，仅作兜底保留；模型目录不返回 `contextWindow`。WorkOS 续期会轮换生产账号的 refresh token，未在生产账号上实测。

共享流式响应的保活心跳与正文使用相同的下游写入预算，心跳写入前刷新截止时间，避免上游暂停正文期间沿用过期截止时间而中断连接；此操作不延长上游语义空闲预算。

Grok 原生 Chat/Responses 在成功提交 SSE 响应后，空闲时发送 15 秒保活注释；Messages 在下游发送 Anthropic `ping` 事件。心跳与正文写入串行化，只在完整 SSE 帧之间插入，不参与生成用量、质量判定和首 Token 统计，也不重置上游语义空闲计时。质量暂存期间仍不提前提交 HTTP 状态或正文；释放后才启用心跳。下游心跳写入或刷新失败会关闭当前上游流，结束处理时等待心跳任务退出。

Build 明确的内容策略拒绝在 Chat、Responses、Messages 上返回 `403 content_policy_violation` 和脱敏提示，不切换账号、不标记账号异常，也不携带重试等待。普通权限不足和明确封号仍保留各自处理；版本升级要求使用 `503 upstream_upgrade_required`，由管理员处理客户端身份配置。

流式响应的开头帧延迟到第一个上游事件再写，以便早期的排队拒绝仍能返回真实 HTTP 状态；但静默超过 45 秒后网关会主动打开流并开始心跳，避免前置代理（如 Cloudflare 约 100 秒的源站等待上限）以自己的 504 结束请求。此后的失败以流内错误事件报告；重试与换号只取决于是否已产出内容，不受影响。Anthropic 格式的心跳是 `event: ping` 事件（与 Anthropic API 一致），因为 Claude Code 按事件而非注释判断流是否空闲，只有注释心跳的流会在约 120 秒无事件后被其放弃；OpenAI 格式保持 `: keep-alive` 注释。

## 4. 严格结构化输出

共享渠道在选账号前建立严格输出约束。提示作为 system 补充，不替代客户端 schema；完成后提取 JSON 校验，不匹配使用 schema_mismatch / 502。

校验器是受限子集，包含实现支持的 type、enum、const、required、additionalProperties、items、数值约束、格式及本地引用等。未知关键词、过深结构或无法编译的部分可能被宽松处理，因此不能称为完整 JSON Schema 合规保证。

strict:false 和 json_object 不执行相同强制校验。答案围栏与外围文字可在提取阶段去除；这也意味着验证对象是提取后的 JSON，而非所有原始输出字节。

流已经开始后校验失败通过错误终止表达，HTTP 200 本身不代表最终成功。上游是否遵守与网关是否检测并拒绝是不同能力。

## 5. 工具声明与历史

工具名必须唯一，tool_choice 引用存在的声明，required 需要可执行工具。历史调用与结果按 call_id 配对，函数名不能替代身份。不同媒体内容必须提供 URL / data URI 等合法结构，不能直接塞裸 base64。

三通道的 Responses 支持 Codex namespace 中的 function：将分组身份编码为稳定的上游函数名，并在 JSON、SSE 和响应资源中还原 namespace 与原始名称；tool_choice 和续轮历史使用同一映射，参数 JSON 不改写。Grok Build 保持原生 Responses 传输，映射随 Key 所有权和 TTL 保存，并保留推理回放中的分组身份。Build 函数声明的 object/null 根类型规范为 object；对象根的 anyOf/oneOf 移入 allOf，保留分支、其他约束、引用及嵌套 nullable 字段。工具声明在实际调用前就会被上游校验。Grok Build 原生 Responses 将 Codex 扩展工具降级为普通函数并在返回时还原：`custom` 变为带 `input` 字符串参数的函数（grammar 定义写入描述，无法强制），`apply_patch` 变为带 V4A `operation` 参数的函数，返回给客户端时分别还原为 `custom_tool_call` / `apply_patch_call`（JSON、SSE、续轮和响应资源一致；流式时不转发函数参数增量，完整条目随 `output_item.done` 给出）；`tool_search`（服务端执行）改为预先加载全部 `defer_loading` 函数；客户端执行的 `tool_search`（Codex Desktop 0.167 起声明，`execution: "client"`）降级为名为 `tool_search` 的函数（保留客户端的描述与参数结构），模型调用时还原为带参数对象的 `tool_search_call`，续轮中的 `tool_search_call` / `tool_search_output` 回放为函数调用与列出已加载工具的结果，`tool_search_output.tools` 中的定义（去掉 `defer_loading`、同名只声明一次）加入本轮工具声明供模型调用；`local_shell` 仍拒绝。Codex 0.167 起的两类历史条目同样处理：无 `call_id`、以 `name` / `namespace` 标识的未配对 `function_call_output`（客户端主动推送的工具输出，如插件通知）转为注明来源的 user 消息，不伪造工具调用；`additional_tools` 条目中的工具定义并入本轮声明（同名只声明一次），条目本身不转发。WorkBuddy、Cline 以及 Grok Web 的桥接路径（Responses→Chat）使用同一降级与还原：上游只看到普通函数，续轮中的 `custom_tool_call` / `apply_patch_call` 及其输出按函数调用与工具结果回放，返回时还原为客户端的条目类型（JSON 与 SSE 一致，流式同样不转发函数参数增量）；`local_shell` 仍以 400 拒绝。2026-11-01 用生产 Grok 账号复核：Build 对原生 `custom`、`apply_patch`、`local_shell` 返回 422（unknown variant），对 `tool_search` + `defer_loading` 返回 403（仅 alpha 用户可用），因此降级与预先加载是必需的；降级后的 `apply_patch` 与 `custom` 函数被模型按预期调用，回传结果后续轮正常。客户端版本 1.0.60 与新请求头被接受，`/responses` 响应带 `x-models-etag`（`/models` 不带）。Build OAuth token 在 xAI 平面（`api.x.ai`）可认证，但免费账号返回 402/403 额度不足；当前无付费账号，故未实现 xAI 平面回退。

Build 执行原生 x_search 时会在输出中插入已完成的内部子调用（`custom_tool_call`，名称如 `x_keyword_search`、`x_user_search`、`x_semantic_search`，call_id 以 `xs_call` 开头），原生 Responses 在 JSON 与 SSE 中移除这些条目及其输入流事件，并重排后续 `output_index`；客户端 custom 工具在发往上游前已降级为函数，因此上游的 `custom_tool_call` 不会是客户端自己的工具。Chat Completions / Messages 路径本就只转换 `function_call`，不受影响。同日实测：无工具对话本身即可命中 prompt 缓存（第二轮 6912/6959），无需注入搜索工具改走缓存路由；整数参数按整数输出（`60000`），无需修正。文本中的工具标记仅作为文本输出，不转换为可执行调用；工具结果不读取客户端本地文件。

Grok Build 的 Chat、Messages 和 Responses 请求声明 function 参数 schema 时，正文、推理等事件到达即下发，网关只暂存各函数调用的参数增量，待该调用完成（`function_call_arguments.done`、`output_item.done`、终止事件或流结束）后一次性下发修复后的完整参数。修复仅两项：删除 schema 以 `additionalProperties: false` 明确禁止的属性（例如 Claude Code `WebSearch` 不接受的 `num_results`），以及把 schema 要求整数的 `1000.0`、`1e3` 写成 `1000`；含 `allOf`/`anyOf`/`oneOf`/`patternProperties` 的对象不删属性，超出精确整数范围的数字不改写。已合法的参数保留原始字节，不补造缺失的值。修复后仍不符合 schema 的参数按上游原样下发并记录警告，由执行工具的客户端把错误反馈给模型，网关不再为此重新生成或返回 502。单个调用参数超过 1 MiB 或同一响应累计超过 4 MiB 时，该调用改为原样透传。续接请求（`previous_response_id`）不携带工具 schema，参数原样透传。非流式 JSON 响应对 `output` 中的函数调用做同样修复。

公共模型目录只发布模型管理中实际启用且通过可见性检查的记录，不再凭推理档位生成额外模型 ID；档位通过能力元数据提供。CC Switch 使用选定 Key 的渠道公共目录，因此仍会受 Key 模型权限和停用状态限制。Build 首轮与 previous_response_id 续轮均按实际账号目录应用推理能力，不支持的档位返回 reasoning.effort 参数错误。

桥接不能无损接受任意未知 item、外部密文或原生托管工具。工具参数增量应聚合后再执行；缺 finish / DONE、参数错误和截断不能伪装完整成功。

Codex 多代理任务支持两种输入：显式 `type: "agent_message"`，以及 `role: "user"` 消息中先出现 `Message Type:`、后出现 `Payload:` 的文本信封。仅在这些任务格式里，将嵌套 `type: "encrypted_content"` 块中的明文转为 `input_text`，保留文字、分块顺序和图片/文件引用；不限定模型名。显式 agent_message 降为普通 user 消息，不向上游转发 author、recipient 等代理内部字段，不提升为 system/developer 指令。三渠道的流式和非流式 Responses，以及摘要压缩路径使用相同转换；已有 namespace 函数映射继续生效。

该兼容转换不具备解密能力，不修改 `reasoning` 或 `compaction` 项里的真实密文。Chat 桥接遇到无法识别为任务的嵌套 encrypted_content 会返回 400，避免继续成功调用却丢失正文；Grok 原生路径对这些非任务内容保留透传，由上游判定支持情况。空白、非字符串的任务块和不支持的 agent 内容明确报错，错误中不回显任务文字，也不新增任务正文日志。存储的原始输入不因转换而被原地修改。

## 6. Responses 生命周期

桥接输出 sequence_number 逐事件递增。它只保证输出顺序标识，当前未实现持久事件回放和基于 Last-Event-ID 的自动断线恢复。

completed、incomplete、failed、cancelled 需分开；length / content_filter 可映射 incomplete。没有明确结束事件的 EOF 是失败。响应持久化与成功终止相关，不能用只收到部分流建立完整成功历史。

Key 所有权与 TTL 决定资源可访问性；匿名豁免的所有者隔离粒度不等同每个用户独立 Key。生产客户端优先使用独立 Key。

## 7. 压缩两种状态

### Chat 桥接

使用无工具摘要请求，只对成功完成且非空摘要签发 object:response.compaction。encrypted_content 中的 bridge_compact_v1 引用是网关存储 ID，不是可携带至官方上游的加密推理。

引用绑定 Key、模型和入口渠道；跨实例要求共享 Redis，过期、状态丢失或不匹配明确失败。续轮展开为摘要历史，保持后续用户输入。摘要是有损的，消耗模型请求用量。

### Grok 网关密封摘要

由凭据主密钥派生独立 AES-GCM 域密钥，密封网关持有状态。丢失主密钥会影响已有压缩状态；不能在更名时顺手改加密域。无法解密自有状态明确失败，不能返回空摘要继续。

压缩摘要生成有自己的成功、空摘要、截断与重试规则，并非普通客户端文本去重。

## 8. 重复内容、质量与空闲

当前不再使用连续重复输出次数中止 Grok 文本 / 推理；重复用户请求和文本也允许透传。缓存回放避免重复注入客户端已有项只是内部历史补全，不会主动删除用户重复输入。

仍保留 stop、最大输出预算、请求期限、语义空闲、协议完成校验和质量暂存。Grok 原生 Responses 也检查推理证据，已判异常的正文在安全重试耗尽后返回 `502 quality_degraded`；Chat/Messages 保留原有耗尽策略。纯函数调用轮次不因没有正文而被拒绝；包含托管副作用工具或账号绑定续接的请求不能跨账号重复生成。

原生推理历史仅保留 Build 支持的字段，删除 `content:null` 等空值及输出态元数据，原样保留密文和合法推理文本；缺失的 `summary` 补为空数组。生成前发生明确密文解码拒绝时，在同一账号恢复；可读摘要使用 `Prior model reasoning summary:` 标记，不作为未标记的历史答案回放。客户端 compaction 密文不被该恢复流程改写。

## 9. 验证边界

2026-10-02 的历史真实上游验证仅覆盖 WorkBuddy、Cline、Grok 的指定样本；原始报告属于本地验证材料，不随源码发布。目录读取与生成不可混为一谈。

后续本地回归覆盖多工具索引、严格输出、压缩引用隔离、终止状态、资源所有权和重复增量透传。这些测试不能证明当前生产已部署，更不能证明全部模型遵守 schema 或命中真实缓存。

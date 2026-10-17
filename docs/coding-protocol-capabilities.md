# 协议能力与兼容边界

## 1. 判断原则

此文档针对网关实现，不承诺完整复刻官方 API。协议可解析、字段已映射、上游接受请求、模型遵守约束、完整客户端可用，是五个不同层次。

当前没有运行完整编程客户端的全模型认证结果；历史 HTTP 协议探针不能扩展成所有版本客户端均兼容的声明。

## 2. 路由矩阵

| 能力 | WorkBuddy / Qoder / Cline | Grok Build |
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

四个通道的 `/{provider}` 与 `/{provider}/v1` 均提供上述已实现能力及模型目录；根路径和统一 /v1 推理入口已删除。路径能力一致不代表各上游协议能力相同。

## 3. 字段与降级

| 控制 | 当前行为 |
|---|---|
| text.format / response_format | 转换为对应 Chat 结构；Qoder 放到 parameters；Build 按原生适配 |
| strict:true | 共享渠道补 schema 提示并校验已支持规则；不匹配失败 |
| prompt_cache_key | WorkBuddy / Cline 按适配传递；Qoder 无等价字段时省略，不因此拒绝生成 |
| text.verbosity 等无等价字段 | 共享传输校验拒绝，不声称模型遵守 |
| include:reasoning.encrypted_content | Chat 桥接消费该输出投影请求并返回兼容警告，不伪造密文 |
| 其他 include | 无等价共享传输时拒绝 |
| 桥接 web_search / preview | 删除托管搜索并提示不可用；强制搜索且无法满足时拒绝 |
| 其他托管工具 / MCP | 共享桥接拒绝；Build 使用原生格式，能力取决于上游 |
| 外部加密推理历史 | Chat 桥接拒绝；Build 按原生状态策略 |
| background=true | 不提供网关任务队列语义；桥接无等价能力，原生仍取决上游 |
| stop | 支持的路径实行停止规则；Grok 转换 stop 可跨帧处理 |
| max_completion_tokens / max_tokens | 显式值保留；前者优先；各通道有预算转换 |

兼容警告可能使用历史名称 `X-Grok2API-Compatibility-Warnings`，不能从 Header 名判断当前存在 Grok Web 通道。

## 4. 严格结构化输出

共享渠道在选账号前建立严格输出约束。提示作为 system 补充，不替代客户端 schema；完成后提取 JSON 校验，不匹配使用 schema_mismatch / 502。

校验器是受限子集，包含实现支持的 type、enum、const、required、additionalProperties、items、数值约束、格式及本地引用等。未知关键词、过深结构或无法编译的部分可能被宽松处理，因此不能称为完整 JSON Schema 合规保证。

strict:false 和 json_object 不执行相同强制校验。答案围栏与外围文字可在提取阶段去除；这也意味着验证对象是提取后的 JSON，而非所有原始输出字节。

流已经开始后校验失败通过错误终止表达，HTTP 200 本身不代表最终成功。上游是否遵守与网关是否检测并拒绝是不同能力。

## 5. 工具声明与历史

工具名必须唯一，tool_choice 引用存在的声明，required 需要可执行工具。历史调用与结果按 call_id 配对，函数名不能替代身份。不同媒体内容必须提供 URL / data URI 等合法结构，不能直接塞裸 base64。

四通道的 Responses 支持 Codex namespace 中的 function：将分组身份编码为稳定的上游函数名，并在 JSON、SSE 和响应资源中还原 namespace 与原始名称；tool_choice 和续轮历史使用同一映射，参数 JSON 不改写。Grok Build 保持原生 Responses 传输，映射随 Key 所有权和 TTL 保存，并保留推理回放中的分组身份。Build 函数声明的 object/null 根类型规范为 object；对象根的 anyOf/oneOf 移入 allOf，保留分支、其他约束、引用及嵌套 nullable 字段。工具声明在实际调用前就会被上游校验。custom、apply_patch、local_shell、tool_search 等扩展仍明确拒绝。文本中的工具标记仅作为文本输出，不转换为可执行调用；工具结果不读取客户端本地文件。

公共模型目录只发布模型管理中实际启用且通过可见性检查的记录，不再凭推理档位生成额外模型 ID；档位通过能力元数据提供。CC Switch 使用选定 Key 的渠道公共目录，因此仍会受 Key 模型权限和停用状态限制。Build 首轮与 previous_response_id 续轮均按实际账号目录应用推理能力，不支持的档位返回 reasoning.effort 参数错误。

桥接不能无损接受任意未知 item、外部密文或原生托管工具。工具参数增量应聚合后再执行；缺 finish / DONE、参数错误和截断不能伪装完整成功。

Codex 多代理任务支持两种输入：显式 `type: "agent_message"`，以及 `role: "user"` 消息中先出现 `Message Type:`、后出现 `Payload:` 的文本信封。仅在这些任务格式里，将嵌套 `type: "encrypted_content"` 块中的明文转为 `input_text`，保留文字、分块顺序和图片/文件引用；不限定模型名。显式 agent_message 降为普通 user 消息，不向上游转发 author、recipient 等代理内部字段，不提升为 system/developer 指令。四渠道的流式和非流式 Responses，以及摘要压缩路径使用相同转换；已有 namespace 函数映射继续生效。

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

2026-10-02 的历史真实上游验证仅覆盖 WorkBuddy、Cline、Grok 的指定样本；原始报告属于本地验证材料，不随源码发布。Qoder 当时因排队拒绝未完成生成验证；目录读取与生成不可混为一谈。

后续本地回归覆盖多工具索引、严格输出、压缩引用隔离、终止状态、资源所有权和重复增量透传。这些测试不能证明当前生产已部署，更不能证明全部模型遵守 schema 或命中真实缓存。

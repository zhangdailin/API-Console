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

工具名称、schema 和参数不按客户端重写。namespace、custom、apply_patch、local_shell、tool_search 等客户端扩展格式明确拒绝；使用标准 function 声明。文本中的工具标记仅作为文本输出，不转换为可执行调用；工具结果不读取客户端本地文件。

桥接不能无损接受任意未知 item、外部密文或原生托管工具。工具参数增量应聚合后再执行；缺 finish / DONE、参数错误和截断不能伪装完整成功。

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

仍保留 stop、最大输出预算、请求期限、语义空闲、协议完成校验和质量暂存。Grok 质量暂存默认有有限尝试与等待预算，耗尽策略可交付最后结果；包含副作用工具时不能无条件重复生成。

## 9. 验证边界

2026-10-02 的历史真实上游验证仅覆盖 WorkBuddy、Cline、Grok 的指定样本；原始报告属于本地验证材料，不随源码发布。Qoder 当时因排队拒绝未完成生成验证；目录读取与生成不可混为一谈。

后续本地回归覆盖多工具索引、严格输出、压缩引用隔离、终止状态、资源所有权和重复增量透传。这些测试不能证明当前生产已部署，更不能证明全部模型遵守 schema 或命中真实缓存。

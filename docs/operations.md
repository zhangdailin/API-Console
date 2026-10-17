# 运维监控、日志与告警

## 1. 实际采集链

middleware 跟踪请求身份、首 Token、上游尝试与最终结果；opsagg 写 Redis 分钟桶，audit 写 Redis Stream；管理 API 合并窗口、延迟样本与账号池；页面展示趋势、渠道模型矩阵和日志下钻；alerting.loop 构造证据并周期评估告警。

关键源码：[trace](../internal/middleware/trace.go)、[opsagg](../internal/opsagg/opsagg.go)、[audit](../internal/audit/audit.go)、[API](../internal/api/api_ops.go)、[告警循环](../internal/alerting/loop.go)。历史 cmd/server/alerts.go 不再是当前实现位置。

## 2. 指标口径

| 指标 | 来源与单位 | 边界 |
|---|---|---|
| 请求数 / 成功失败 | 最终请求结果 | 与上游重试尝试数分开 |
| RPM | 业务请求数 / 窗口分钟 | 分母不是非空桶数量 |
| TPS | 总 Token / 窗口秒数 | 不是生成瞬时吞吐 |
| TTFT | 首次真实生成内容时间 | 角色、控制、keepalive 不计；未知不补零 |
| 请求耗时 | 最终请求计时 | 与各尝试网络时延分开 |
| P95 | 保留样本的最近秩分位数 | 不等同全请求精确 P95 |
| Token / 费用 | 上游用量、估算和价格表 | 真实、估算、未计价分别判断 |
| 账号可用数 | 指定账号池判定口径 | 不证明所有模型可生成 |
| runtime | 当前进程与宿主采样 | 不代表集群总量 |

实时流量拓扑通过 `/api/ops/flow` SSE 展示当前实例通过鉴权、并发与费用准入后的生成请求。QPS、输入与输出 TPS 固定使用最近 5 个一秒桶；上游用量事件到达时才计入 Token。节点与顶部输出 TPS 明确标为上游上报；范围内有路由尚未上报时显示「待上报」，不补零，也不解释为逐字生成速度。

首生成、可见首字、非流式正文 TTFB 与请求总时长固定使用最近 15 分钟内结束的请求，每个渠道 × 协议最多保留最近 256 个延迟样本。渠道、协议与网关分位数按保留原始样本合并计算，不平均各路由的 P95。点击节点后在侧边抽屉的「详细指标」分别查看各自分布与样本数，两类数据不混合。首生成仅计 SSE 文本、推理或工具进展，可见首字仅计文本，正文 TTFB 不代表真实首生成。无近期样本会明确提示，可能是请求尚未结束、样本已过期或没有有效的输出时延。实时接口用 `latency_window_seconds=900` 标记延迟窗口。

失败率、错误计数、重试、账号切换、等待和 HTTP 阶段仍固定使用最近 60 个一秒桶内结束的请求（接口 `window_seconds=60`），覆盖全部结束请求，不受延迟样本上限影响。延迟样本数可能与这些计数不同。HTTP 200 后的流中断、超时、取消及处理器 panic 计失败，最终 429/529 单独列出；重试后成功仍为成功。尚未输出 / 已开始输出及最长进行时长为当前状态，非流式开始写正文后也归已开始输出。

账号池通过独立的只读 `/api/ops/pools` 每 15 秒采样，不依赖历史概览，30 秒后失效。账号状态与容量复用现有计算口径；分母为启用且未被账号级策略暂停的渠道账号有效并发槽位，分子为这些账号的已占槽位；暂停账号上的旧请求仍计入活跃请求，不占用此分母。该容量未扣模型目录、绑定、模型冷却等限制，不保证某模型可用，也不是上游真实队列长度。Redis 占用读取失败时容量明确不可用，不能显示为空闲。共享 Redis 账号槽位可覆盖其他实例，拓扑流量和延迟始终为当前实例。

拓扑固定呈现四渠道 → 网关 → 三接口，节点详情使用抽屉。内部渠道筛选与总览同步，主图、顶部指标、抽屉和短时趋势使用同一渠道范围；历史时间窗口与模型筛选不改变拓扑口径。全屏内可筛选渠道，退出后恢复原位置；手机端按渠道、网关、接口纵向分组。可关闭动画，也自动尊重系统减少动态效果设置。

拓扑默认使用紧凑卡片。小笔记本在 1366×768、1280×720、1024×768 等视口保留三列图；高度不超过 700px 时进一步收紧布局，账号池占用保留文字，省略重复的占用条。渠道的上报 TPS 在窄卡片中移到节点详情，顶部汇总仍保留。手机使用双列渠道卡片、横排网关及并列短时趋势，允许纵向滚动查看全部内容，不强行缩小整张桌面图。

光轨由接收与完成计数的增量驱动，右向左表示入站，左向右表示输出或完成活动；它表示汇总活动，不是逐请求或逐 Token 回放。活跃连接持续发光，空闲连接停止。超过 5 秒没有新快照或连接中断时，保留最后数据与时间、明确标记过期并停止动画。页面后台暂停 SSE、账号池轮询与动画，返回后重新同步。

底部趋势仅保留本页面最近 60 秒、每秒一个采样点。断线或后台期间留空，实例重启或切换渠道清空旧趋势；刷新页面不会恢复旧采样。不使用历史聚合补造实时点。

probe / http 基础设施不进入全部通道业务汇总。已 HTTP 200 后 SSE 错误单列流失败，不当成成功。

## 3. 窗口、采样和保留

- 分钟桶默认保留 8 天，趋势查询最多 24 小时；不是完整历史无限查询。
- 查询按整分钟边界，边缘桶可能包含所选秒级窗口外请求。
- 每渠道分钟桶最多保留最近 5000 个延迟 / TTFT 样本，每模型最多 2000 个。
- 延迟查询本身也有有界样本读取；高流量下较早样本会被裁掉。
- 审计 Stream 初始化约保留 10000 条并近似裁剪，真实时间覆盖由实际记录决定。
- 日志分页、尝试关联和覆盖扫描各有上限；一次返回空页不证明所有历史不存在。

不能平均各桶 P95 作为全窗口 P95。保留天数、当前已有记录和完整采集覆盖是不同事实。

## 4. 采集健康

审计队列上限 256 条、单条 32 KiB、序列化预算 8 MiB；入队前生成快照，避免后续修改待写内容。写失败与丢弃不转为无限重试队列。

| 字段 | 意义 |
|---|---|
| queue / queued_bytes | 当前进程待写量 |
| written | 本进程累计成功写入 |
| dropped | 队列预算、事件大小等导致丢弃 |
| write_failed | 累计存储写失败 |
| last_success / last_failure | 最近采集结果时间 |
| closed | 采集器生命周期状态 |

指标聚合和审计健康独立。上述计数是进程生命周期数据，重启重置，不是集群总数、当前失败率或历史完整度。

总览读取有 8 秒预算；Redis 读取失败应明确不可用 / 503，而非空流量。查询失败后的旧健康数据标为未更新。零流量、关闭采集、样本缺失和故障分别展示。

## 5. 日志分类与关联

请求日志记录最终结果，尝试日志记录上游过程，操作日志记录管理员修改，系统日志记录后台与告警事件。通过服务端 request ID 关联，客户端 trace 可跨重试重复，不能据此认定网关去重了请求。

使用 /api/journal/records 选择时间、通道、结果和分页游标；关联尝试可能超出当前页面范围，API 只进行有界回溯。对高流量窗口，应缩小时间范围，不能把关联不全解释为从未请求上游。

## 6. 诊断内容

诊断采集受开关、采样、并发、保留时间和存储成功约束。没有正文时 available:false，不说明模型生成了空内容。默认采集约 24 小时且最多 512 个请求束；不应依赖它作无限请求档案。

正文按纯文本展示并脱敏，业务内容仍可能敏感。开启详细诊断前明确采样需求，完成排障后恢复合适配置。

阶段时延包括本地、DNS、连接、TLS、上游与写出等。复用连接缺阶段不能补成测量到的零；complete 仅表示读取 EOF，仍需判断协议终止与成功状态。

## 7. 告警规则与恢复

通常每 60 秒评估最近 30 分钟窗口；账号通道即使无流量也可纳入账号池状态。规则具有样本门槛、迟滞和恢复语义，不只按单个瞬时值报警。

账号池、桶或样本读取失败时跳过整轮并保留状态，不生成假恢复。规则存 Redis，当前触发集合在进程内；持久规则不等于重启不改变触发状态。

## 8. 页面操作顺序

1. 确认窗口、单位和采集可用状态，再读取 KPI。
2. 选择渠道；模型筛选主要用于矩阵，不表示宿主资源变成该模型独占资源。
3. 通过失败指标下钻日志并保留窗口条件。
4. 查看上游尝试，区别入口拒绝、排队、切号和最终输出。
5. 诊断缺失时先看开关 / 采样 / TTL，再判断是否需要新的受控请求。

## 9. 多副本与验收

Redis 桶可汇总多个写入者，但 runtime、采集健康、进程准入及告警当前状态有本地边界。每副本读到的管理页面不自动成为整个集群完整状态。

验收时模拟读取失败、写丢弃、零样本与空流量，确认页面分别表达；TTFT 未知不能是 0，缺 P99 不能拿 P95 替代。真实浏览器和生产数据范围需另行验证，本次没有执行这些在线检查。


## 输出完整性与延迟证据

HTTP 成功、协议完成、可见输出与预算截断是不同口径。正常 `length` / `max_tokens` 终止保持 HTTP 成功及原结束原因，日志新增 `output_truncated`；只产生推理而没有文本或工具时标记 `reasoning_only`。运维概览提供 `output_evidence_samples`、`visible_answers`、`reasoning_only`、`output_truncated`，旧记录没有输出证据，不补造为完整答案。

请求日志的 `first_token_ms` 为首生成（包括推理和工具；非流式采用正文首写的 TTFB 近似），`first_visible_token_ms` 是流式文本首字。另有 `retry_wait_ms`、`queue_wait_ms`、`upstream_attempts`、`account_switches`。普通日志也采集 `upstream_http_phase_samples` 及 `upstream_http_phases_sum` 的 DNS、TCP、TLS、取连接、响应头耗时。各阶段可能重叠，累计值不能相加当作请求总耗时；复用连接及自定义传输可能缺阶段，缺失不能解释为零。不会保存请求正文、凭据、代理地址或模型输出。

这些数值测量源站内的链路。公网客户端的 DNS、TLS、CDN 等时间须另测，并按 `X-Orchids-Request-ID` 关联 Caddy 与请求日志。源站之外的额外等待不能仅凭一个样本归因模型或网关。

## Grok 质量检查的日志与流水

Grok 质量检查分两半：暂存（`quality_hold.go` 编排、`quality_hold_policy.go` 判定、`quality_hold_buffer.go` 缓冲）决定一轮回答能否交付，护栏（`quality_guard.go`）决定产生该回答的凭据是否停用。过程和结论写入流水，动作名均以 `grok_quality_` 开头。

日志中心直接列出开始检查、通过或中断、拦截、换号重试和最终处理阶段，不需要专门的质量过滤控件。检查开始即写入记录，不必等整个请求结束；刷新日志中心即可看到已发生的阶段。打开请求或质量事件详情，按时间查看关联过程、原因、账号、字符与 token 计数。缺少结论时显示「客户端交付结果未记录」，缺少逐次记录时显示实际找到的明细数量。系统日志中的质量护栏记录仅描述凭据处理，不能据此推断客户端最终收到了什么。

| 流水动作 | 类型 | 何时写入 | `status` | 关键字段 |
|---|---|---|---|---|
| `grok_quality_check` | request | 每轮开始检查、正常通过或异常中断 | `checking` / `passed` / `error` | `attempt`、`metadata.mode`、暂存与重试策略、计数及耗时 |
| `grok_quality_retry` | request | 拦截后确实选中了下一账号 | `retrying` | `attempt`、`metadata.from_account_id`、`metadata.to_account_id` |
| `grok_quality_degraded` | request | 每一轮被暂存拒绝的尝试 | `degraded` | `attempt`、`details` / `metadata.reason`（命中的检测器）、`metadata.mode`（stream / collected）、`metadata.guard`（护栏对该凭据的处理）、各项字符与 token 计数 |
| `grok_quality_resolution` | request | 出现过被拒尝试的请求结束时，一次 | `retried` / `delivered_degraded` / `rejected` / `undeliverable` | `metadata.client_received_degraded`、`details` / `metadata.stopped`（停止重试的原因）、`metadata.withheld_attempts` |
| `grok_quality_guard` | system | 护栏改变凭据状态时 | `parked` / `disabled` / `cleared` | `account_id`、`metadata.failures`、`metadata.cooldown_until`、`metadata.account_enabled` |

被拒尝试自身的 `grok_request` 行以 `status=quality_degraded` 记录，并带 `metadata.quality_withheld` 与 `metadata.quality_reason`；`metadata.finish_reason` 保留上游原始结束原因。

`reason` 取值：`burst_dump`、`cipher_drool`、`fake_encrypted_dump`、`fast_reasoning_ratio_dump`、`no_reasoning`。`stopped` 取值：`attempts_exhausted`、`account_budget_exhausted`、`replay_unsafe`、`client_gone`、`no_other_account`。

判断客户端是否收到了降级回答，只看 `grok_quality_resolution`：`delivered_degraded` 表示按 fail-open 交付了最后一轮被拒内容，其余三种状态客户端都没有收到降级内容。`grok_quality_degraded` 描述的是客户端从未见到的尝试。

启用了质量检查的正常回答也写入开始与通过两条过程记录；未启用或不适用的请求不伪造检查过程。只在阶段变化时记录，不逐个 SSE 块记录。这些行只包含计数、耗时、策略和标识，不包含回答正文或凭据。`retried` 表示后续尝试未再被质量拦截，不保证请求最终成功，实际请求结果以请求日志的状态为准。

Chat 请求省略推理强度时，质量检查采用本次选中账号实际发送的模型默认推理强度；换号后重新采用下一账号的默认值，不修改客户端原始请求。非流式响应（`metadata.mode=collected`）无法观测上游出字时序，`metadata.first_visible_ms=-1` 表示未知，不以本地 JSON 解析耗时触发极速输出检测。开始检查、尚无正文的阶段同样不伪造首字时间；非流式仍保留不依赖出字时序的质量判断，流式请求继续使用实际观测的时序。

账号管理直接显示 Grok 的质量冷却、连续退化次数、原因与冷却截止时间，侧栏异常统计采用相同的冷却判断。质量原因单独保存为 `quality_reason`，不借用 HTTP 状态说明，也不因普通账号刷新清空；历史记录没有原因时显示「原因未记录」。冷却到期不再显示为正在冷却，但保留失败计数供复检；账号禁用时不承诺自动恢复。页面读取这些字段不恢复账号、不清除质量处罚。

对应的服务日志：`grok quality hold: turn withheld`（WARN）、`grok quality hold: request resolved`（重试成功为 INFO，其余为 WARN）、`grok quality guard: credential parked / disabled`（WARN）、`grok quality guard: credential cleared`（INFO），均带 `request_id` 与 `account_id`。

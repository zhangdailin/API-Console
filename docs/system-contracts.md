# 系统关键合同与认知验收

本文用于把文件级职责串成可复用的系统认知。证据是当前源码、已有架构文档和回归测试；它不代表本次已运行这些测试，也不证明生产或供应商当前行为。

## 1. 系统位置与修改边界

API-Console 是 Go 网关与内嵌 Go 模板、原生 JavaScript 管理后台。Redis 保存主要业务状态，凭据主密钥来自文件或环境变量。Node 用于前端测试，不是服务运行依赖。

WorkBuddy、Qoder、Cline 走共享 Handler、提供者工厂和能力适配；Grok Build 走独立处理器，上游生成使用 Responses，面向客户端提供 Chat / Messages 转换。所有推理入口通过路径绑定通道，通道根前缀与带 /v1 前缀能力一致；统一推理入口已删除。响应子资源依赖已存所有权，取消和输入历史显式注册共享接口。

共享 Chat 线格式放在 `internal/chatwire`，Responses 协议放在 `internal/responses`，通用 HTTP 逻辑放在 `internal/httpclient` / `httpserver`。调用方直接使用所属包的实现。

证据：[架构](architecture.md)、[启动装配](../cmd/server/main.go)、[路由](../cmd/server/routes.go)、[提供者能力装配](../internal/provider/capabilities_wire.go)。

## 2. 必须连起来理解的链路

| 链路 | 核心顺序 | 修改时的边界 |
|---|---|---|
| 启动配置 | 文件配置 → 凭据主密钥 → Redis 初始化 → 保存配置覆盖 → 处理器装配 | 主密钥和初始 Redis 连接先确定；保存配置不能反向改变此前初始化事实 |
| 生成请求 | 进程/通道准入 → Key 鉴权 → Key 并发 → 预算预留 → 分发/账号选择 → 上游 → 流完成判断 → 结算/最终日志 | 不同子资源有独立包装；不能把生成链机械套到 GET、DELETE、cancel |
| 账号变更 | 持久提交 → store 通知 → 账号事件总线 → 池缓存/客户端缓存/刷新唤醒 | 只在本进程传播；异步合并通知不等于跨副本实时一致 |
| 模型发现 | 刷新租约 → 账号目录观察 → 账号快照 → 聚合能力 → 管理模型协调 | 部分失败保留旧证据并限制负向裁剪；目录不是生成探针 |
| 运维与告警 | 最终请求跟踪 → 分钟桶/审计 Stream → API 窗口合并 → 页面 → 告警快照评估 | 最终请求与上游尝试分开；缺失、零值、采样和故障分别解释 |
| 在线升级 | 发行验证 → 下载及构建身份校验 → 备份 → 独立守护 → 替换/重启 → 运行映像验证 → 必要时回退 | 健康 200 不足；运行进程摘要和版本提交也须匹配 |

证据：[路由](../cmd/server/routes.go)、[持久通知](../internal/store/account_changes.go)、[进程事件](../internal/accountevents/accountevents.go)、[目录协调](../internal/modelrefresh/reconcile.go)、[运维](operations.md)、[升级守护](../internal/selfupdate/watchdog.go)。

## 3. 防止错误修改的不变量

1. **凭据与数据一起恢复。** 加密账号读取不能靠删除密钥修复；错误密钥、坏密文与加密模式下的明文记录须明确失败，不自动迁移。压缩状态派生域 `orchids:secureblob:v1:` 与展示名无关，不能因改品牌机械更名。
2. **轮换凭据不能覆盖更新结果。** 专用凭据补丁核对预期 refresh token；相同新令牌允许幂等重入，陈旧身份拒绝写入。普通账号编辑和计数器各有写入职责。
3. **已交付输出限制重放。** 共享 Handler 发生部分输出后不重新生成；Grok 质量重试先暂存输出，并检查安全重放与预算，有副作用工具请求受保护。
4. **流的 HTTP 成功不等于协议成功。** 各提供者与桥接层有自己的终止合同；不得把 EOF、keepalive 或已发出的 200 自动解释为成功完成。
5. **资源访问要检查所有者。** 响应 GET、DELETE、续接和子资源沿 Key 指纹查询所有权；匿名请求使用共同的 `anonymous` 身份，不能声称匿名调用彼此隔离。默认进程内 Store 不是 Redis 故障替身；多副本需注入共享 Store。
6. **额度、权益和凭据分开判定。** 模型拒绝、排队、内容拒绝与永久 OAuth 拒绝不能互相替代；限流头不等于余额，目录成功不等于所有模型可生成。
7. **采集失败不能制造健康零数据。** 聚合未启用、读取失败、样本不足与真实零流量分别表达；概览读取失败会返回 5xx，不保证每个失败分支都是 503。P95 来自保留样本，不是全请求精确分位数。
8. **共享 Redis 不代表所有状态共享。** 账号事件、刷新 Hub、部分缓存、进程/通道准入、runtime 与告警触发集合有本进程边界。告警规则持久化不等于触发状态持久化。
9. **配置先持久化再发布。** 已发布快照不可原地修改；文件、Redis 覆盖和管理保存均校验代理。无效 Redis 代理覆盖回退文件配置，而非把无效地址静默当成直连。
10. **部署证据独立于源码。** 发行、容器与 systemd 在线升级路径分别验收；二进制回退不还原 Redis 数据。历史探针和本地 mock 都不能证明当前生产已更新。

证据：[凭据密文](../internal/store/credential_cipher.go)、[凭据补丁](../internal/store/credential_patch.go)、[压缩密钥](../internal/secureblob/cipher.go)、[共享推理](../internal/handler/handler.go)、[质量暂存](../internal/grok/quality_hold.go)、[资源合同](../internal/responses/store.go)、[错误策略](../internal/accountpolicy/policy.go)、[概览](../internal/api/api_ops.go)、[配置保存](../internal/api/api_config.go)。

## 4. 首批关键测试的认知范围

下面 18 个文件被选择用于正式认知范围扩充。是否已经生效，以当前 AOCI Scope / Guide 状态为准；本表不代替批准收据或测试执行结果。

| 测试证据 | 需要理解的合同 |
|---|---|
| [Responses 路由端到端](../cmd/server/responses_protocol_e2e_test.go) | 鉴权路由与 Responses 行为如何组合 |
| [响应子资源路由](../cmd/server/responses_subresource_routes_test.go) | cancel / input_items 的注册与分发 |
| [预算与结算](../internal/middleware/billing_test.go) | 预留、释放、失败关闭与并发幂等结算 |
| [Key 并发](../internal/middleware/key_concurrency_test.go) | 准入与释放、Key 与账号计数身份隔离 |
| [可信代理](../internal/middleware/trusted_proxy_test.go) | 真实地址只来自可信代理链 |
| [提供者凭据补丁](../internal/store/provider_credential_patch_test.go) | 陈旧轮换拒绝与并发字段保留 |
| [凭据加密](../internal/store/credential_cipher_test.go) | 明文拒绝、启动不改写、错误密钥与解密失败 |
| [账号持久通知](../internal/store/account_events_test.go) | 持久后通知、异步分发与合并 |
| [Key 权威鉴权](../internal/store/api_key_auth_test.go) | 存储策略与限流判定的权威来源 |
| [客户端缓存事件](../internal/handler/client_cache_events_test.go) | 轮换退役旧客户端，最后租约释放后关闭 |
| [Qoder 原生工具](../internal/qoder/tool_protocol_test.go) | 原生函数参数保留、工具文本不执行、终止与用量处理 |
| [Grok 质量暂存](../internal/grok/quality_hold_test.go) | 暂存、重试、预算与安全重放 |
| [共享 SSE 接入](../internal/grok/responses_sse_adoption_test.go) | SSE 原始帧与共享实现边界 |
| [会话状态存储](../internal/store/grok_session_store_test.go) | 亲和及推理回放的隔离、TTL 与身份 |
| [Linux 升级](../internal/selfupdate/update_linux_test.go) | 替换、幂等锁、守护验证及失败回退 |
| [分钟聚合](../internal/opsagg/opsagg_test.go) | 窗口分母、保留样本与读取/写入失败 |
| [告警证据](../internal/alerting/loop_test.go) | 部分证据或取消不能形成完整快照 |
| [压缩状态](../internal/responses/compaction_blob_test.go) | 网关自有密文与外部状态的处理边界 |

## 5. 跨模块认知验收

验收应先独立作答，再核对实现与断言。每题需说明条件、定位决定行为的源码，并给出已有测试证据或明确未覆盖项。只报文件名不算掌握调用链；阅读测试不等于执行通过。

| 问题 | 核对方向 |
|---|---|
| 一个请求在哪里占并发，在哪里预留预算？子资源是否相同？ | routes → session / concurrency → billing |
| 令牌轮换后，旧客户端为何不会打断正在运行的请求？ | credential_patch → account_changes → client_cache |
| 第二个副本能否即时收到账号变更？ | accountevents 的进程边界与缓存 TTL |
| 收到 200 后流中途失败，能否重新生成并记作成功？ | handler / 提供者终止 → trace / observability |
| 知道 response ID 能否用另一把 Key 读取或取消？匿名例外是什么？ | OwnerHash → response Store → resource / subresource |
| 一个账号目录刷新失败时，哪些模型可以被裁剪？ | discovery → reconcile → redis_models |
| P95 的分母、样本、时间桶边界及保留限制是什么？ | opsagg → api_ops → operations.md |
| Redis 桶或账号读取失败，会不会触发告警恢复？ | alerting BuildSnapshot → StartLoop → Engine |
| 升级后 /health 正常，为什么仍需验证运行进程？ | platform_linux runningHash → watchdog verifyReady |
| 修改项目展示名，哪些加密域、模块名和服务路径不能顺手改？ | secureblob / credential_cipher → buildinfo / release / deployment |

各题分别记录通过、失败或缺证据；不把题目通过率混成模型自评掌握度，也不承诺固定百分比。源码和测试改变后，稳定收尾时维护受影响 AOCI 条目；上下文压缩后按仓库合同重新加载完整认知。

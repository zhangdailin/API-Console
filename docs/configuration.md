# 配置完整指南

## 1. 文件与加载方式

```bash
cp config.example.json config.json
go run ./cmd/server -config ./config.json
```

Windows 用 `Copy-Item config.example.json config.json`。JSON 是推荐格式：YAML 解析器只支持扁平 key:value，不是完整 YAML 实现，不能依赖嵌套对象或列表语法。

不带 `-config` 时依次寻找 config.json、config.yaml、config.yml；均不存在时启动失败。字段以 [Config 实现](../internal/config/config.go) 为准，示例文件不是所有内部默认值的完整副本。

## 2. 配置优先级与生效时间

| 阶段 | 行为 |
|---|---|
| 文件加载 | 解码、默认值及边界归一化 |
| 密钥与 Redis 初始化 | 使用初始化时的配置建立存储和凭据加密 |
| Redis 配置恢复 | `<redis_prefix>settings:config` 中已保存设置可覆盖文件 |
| 管理保存 | 增量补丁，持久化成功后发布新快照 |
| 请求读取 | 支持动态配置的组件读取当前快照 |

不要把所有字段都理解为热更新。监听端口、路由注册的 admin_path、Redis 连接和已建立的加密主密钥等初始化属性，修改后需要重启并确认真实运行值。尤其 admin_path 页面会继续使用启动时注册的路径。

文件配置和 Redis 保存值不一致时，先查看管理端实际值；不能通过反复改文件解决 Redis 覆盖。管理列表不返回 admin_token 明文，管理保存也不接受修改它。

管理页保存时，`admin_password` / `admin_pass` 的空字符串、纯空白或 `null` 表示保留现有密码；后端禁止保存空管理员密码。普通配置修改保留部署管理的 `admin_token`。修改管理密码或用户名会原子更新 Redis 会话版本，所有旧会话随即失效，需要重新登录。Redis 不可用时不接受缓存会话，也不创建临时本地会话；恢复后未撤销且未过期的持久会话仍可使用。登录、Basic Auth 和密码请求头共用每 IP 的失败预算（15 分钟补充 5 次）；正常会话访问和成功的密码认证不消耗预算。

## 3. 基础、安全与存储字段

密码登录、Basic Auth 和密码请求头每次读取 Redis 中的权威管理凭据；共享配置读取失败、损坏或撤权后丢失时返回 503，不回退到副本启动时的旧密码。旧副本保存配置时会原子核对管理凭据，凭据已在其他副本变更则拒绝保存，避免普通设置修改复活旧密码。此时需要重启旧副本以重新加载配置。

使用会话 Cookie 的 POST、PUT、PATCH、DELETE 等写请求必须带与当前服务协议和 Host 一致的 `Origin`，有正文时必须使用 `application/json`；读取请求不受此限制。命令行可以使用显式管理令牌或 Basic Auth。反向代理应传递正确的 Host，并在 `trusted_proxies` 中声明实际代理，HTTPS 终止代理需设置 `X-Forwarded-Proto: https`。

`/api/login` 只接受一个 JSON 文档，正文最多 8 KiB，用户名最多 256 字节、密码最多 4096 字节；超限正文返回 413，字段或 JSON 格式错误返回 400，非 JSON 类型返回 415。进程同时最多解析 32 个登录请求，容量满返回 503；失败预算耗尽时不读取正文。

| 字段 | 默认 / 示例 | 说明 |
|---|---|---|
| `port` | `3002` | 字符串端口，监听 `:port`，不是只绑定 loopback |
| `admin_user` | `admin` | 管理登录名 |
| `admin_pass` | 空值生成 16 位随机密码 | 启动日志提示；生产应显式持久配置 |
| `admin_path` | `/admin` | 页面路径，改动需重启 |
| `admin_token` | 无默认公共令牌 | 兼容管理身份；不等同推理 API Key |
| `credential_encryption_key_file` | `data/credential.key` | 相对路径按配置文件目录解析 |
| `redis_addr` / `redis_password` / `redis_db` | 示例 `127.0.0.1:6379` / 空 / 0 | 连接和认证 |
| `redis_prefix` | `orchids:` | 兼容旧数据命名，改变会切换逻辑数据空间 |
| `redis_pool_size` | 实现按配置处理 | Redis 连接池，不是 HTTP 账号并发 |
| `response_store_ttl_hours` | 720 小时 | 响应、续接和桥接压缩状态的保留配置 |
| `deployment_instance_id` | 示例为空 | 多实例设置不同身份 |
| `media_dir` | `data/tmp` | 保留的运行目录字段，不证明存在媒体生成接口 |
| `trusted_proxies` | 空 | 只填实际代理 IP / CIDR |
| `debug_enabled` | false | 诊断开关；pprof 路由还受启动注册和管理鉴权约束 |
| `verbose_diagnostics` | false | 仅 debug_enabled 同时为 true 才生效 |

## 4. 凭据主密钥

环境变量 `ORCHIDS_CREDENTIAL_ENCRYPTION_KEY` 优先于密钥文件。支持 32 字节主密钥对应的 base64、hex 或原文表示；不要把任意长度密码当成合法主密钥。

首次可独占创建密钥文件；并发创建时读取已存在文件。Redis 账号凭据使用 AES-GCM，配置密钥后拒绝旧明文记录，不自动迁移或改写；已有 `enc:v1` 密文但缺密钥或解密失败会报错，不能当成空凭据继续运行。

备份必须包含 Redis、有效密钥来源和配置。多实例共享相同密钥。更名时不能随意更改密钥、加密派生域或 Redis 前缀。

## 5. 并发与连接容量

生成期限、输出缺省及重试预算已固化，见本文末尾。以下保留部署容量参数：

| 字段 | 默认 / 行为 |
|---|---|
| `concurrency_limit` | 100，最大 1000000；进程满时立即拒绝 |
| `provider_concurrency_limits` | 四通道各自的本进程入口容量，非正关闭该限制 |
| `stream_flush_interval_ms` | 默认 2 毫秒、最大 20；负值关闭合并刷新 |
| `upstream_max_conns_per_host` | HTTP 连接配额；浏览器 HTTP/2 同时限制在途请求 |
| `upstream_max_idle_conns_per_host` | 空闲连接复用容量 |

Key 和账号并发限制与入口容量叠加。上游 HTTP、流空闲、首生成、父上下文及边缘代理期限互相独立，最先触发者结束请求；固定预算不保证边缘代理可以等待同样长的时间。

## 6. 自动刷新：不能混淆两个周期

`TokenRefreshInterval` 固定为 1 分钟，`AutoRefreshToken` 固定开启，`LoadBalancerCacheTTL` 固定为 5 秒；这些字段用 json:"-"，不应指导用户通过 JSON 配置修改。

1 分钟是调度检查周期，**不等于每分钟对所有账号读取完整目录和额度**。账号目录 / 套餐 / 额度观察依据到期状态与通常 30 分钟的新鲜度门槛决定；独立模型目录循环启动延迟 30 秒、随后每 30 分钟执行。手工检查可额外验证凭据，且与后台共享本进程账号刷新租约。

| 通道 | 刷新侧重点 |
|---|---|
| WorkBuddy | 令牌、目录、信用额度 |
| Qoder | 设备凭据、runtime、目录和权威额度 |
| Cline | OAuth、套餐与免费 / ClinePass 推荐目录，不提供统一数值余额 |
| Grok | OAuth、Build 身份、账单与目录观察 |

观察读取失败保留旧快照，不能补造余额或把认证成功等同于额度恢复。WorkBuddy 无时区额度时间按 UTC 解释，仅作参考。

Cline 套餐只读取官方 `/users/me/plan` 的 `success=true`、`data.plan` 结构，使用 `displayName` 或 `name`；旧的扁平套餐返回格式不再支持。账号维护与模型目录观察都会先同步套餐，再更新账号的可用目录。确认有付费套餐后自动加入上游 `clinePass` 分类并保留免费模型；未确认付费时仅发布 `free` 分类。订阅到期或明确无套餐后撤销 Pass 路由资格，旧快照中的付费条目也不能继续选中。套餐读取失败保留已确认的状态；ClinePass 订阅不作为 `clineCloud` 按量计费模型的授权依据。

## 7. 各通道连接参数

| 通道 | 字段组 | 说明 |
|---|---|---|
| WorkBuddy | `workbuddy_base_url`、`workbuddy_http2_enabled` | 默认国际服务；HTTP/2 是传输设置 |
| Qoder | `qoder_oauth_base_url`、`qoder_openapi_base_url`、`qoder_inference_base_url` | 浏览器、控制面和生成主机各自配置 |
| Qoder | `qoder_client_id`、`qoder_client_version`、`qoder_http2_enabled` | 使用 reference 协议；旧 qoder_protocol_profile 字段被忽略 |
| Cline | `cline_api_base_url`、`cline_workos_client_id`、`cline_workos_authorize_url`、`cline_workos_token_url`、`cline_http2_enabled` | API 与 WorkOS 授权分别配置 |
| Grok | `grok_cli_base_url`、`grok_cli_user_agent`、`grok_cli_client_version`、`grok_cli_client_identifier` | 默认 Build CLI 网关和 CLI 身份 |
| Grok | `grok_cli_oauth_client_id`、`grok_cli_oauth_device_url`、`grok_cli_oauth_token_url` | 默认 auth.x.ai 的设备授权与交换端点 |

Grok 默认 base 为 `https://cli-chat-proxy.grok.com/v1`，CLI 版本当前默认 1.0.40。替换主机前确认协议、账号区域与授权匹配，不能只把字符串域名换掉就宣称区域兼容。

## 8. Grok 请求与出口

| 字段 | 生效方式 |
|---|---|
| `grok_build_rps` | 非正 / 非有限关闭节奏；正值夹取 0.01–1000，按账号 / 团队范围执行 |
| `grok_egress_enabled` | 默认未启用，开启后没有健康节点则失败，不静默直连 |
| `grok_egress_nodes` | name、url、weight、scope、proxied；URL 支持 http / socks5 / socks5h，scope 用 cli / all |

Grok 返回上游提供的思考内容：Chat 使用 `reasoning_content`，Messages 使用 `thinking` 内容块，原生 Responses 保留 reasoning 事件与输出项。思考与正文分别传输；思考项身份、`encrypted_content` 和 `reasoning_tokens` 保留，用于续接与计费。

Grok 语义空闲按有效生成事件衡量，keepalive 不延长时钟；下游背压不应计成上游读取空闲。它与其他通道的字节空闲监视不同。

## 9. 代理与缓存

`proxy_url` 为通用代理，`proxy_http` / `proxy_https` 为分离配置，另有 proxy_user / proxy_pass / proxy_bypass。实际优先级与环境代理回退由 [proxy 实现](../internal/httpclient/proxy.go) 决定；不要以 UI 的“直连”文字替代真实请求路径证据。代理字段在文件加载、Redis 配置恢复和管理端保存时校验，无效值会被拒绝，不会静默直连；HTTP、HTTPS、SOCKS5 与 SOCKS5H 均可作为通用上游代理，Grok browser transport 也支持 HTTPS CONNECT 代理。

`cache_strategy` 默认 mix，用于请求中的真实上游缓存提示。它不提供本地答案缓存，不保证上游命中，不删除重复用户输入。Qoder 没有等价缓存 key 字段时省略该提示。

已废弃字段：inference_auth_enabled、grok_cli_model_ids，以及 enable_token_cache、token_cache_ttl、token_cache_strategy、cache_token_count、cache_ttl。旧鉴权开关不能关闭 Key；管理保存显式提交本地模拟缓存字段返回 400，旧 token-cache 管理端点不存在。

## 10. 诊断配置与排查

`diagnostics_sample_every`、`diagnostics_max_concurrent` 控制诊断采样与并发。采样意味着不是每个请求都有正文；无采集时 API 明确 unavailable，而非返回空正文证明上游没有输出。

改配置后按顺序核对：保存 HTTP 和管理 code 信封 → 当前有效值 → 是否需重启 → 重启后进程值 → 账号观察及真实请求。更多见 [故障排查](troubleshooting.md)。


## 固定生成策略

配置页只保留后台凭据和代理控件；免鉴权来源及生成预算与延迟控件已删除。所有渠道的模型、推理、压缩和响应资源入口均要求托管 API Key，包括 loopback、IPv6 和可信代理转发的请求。`anonymous_allow_ips` 及旧 `inference_auth_enabled=false` 不能放行请求；管理令牌不能替代推理 Key。

固定值集中在 [inference_limits.go](../internal/config/inference_limits.go)：

| 策略 | 固定值 |
|---|---:|
| 跨重试首生成期限 | 180 秒 |
| 入口及上游完整请求期限 | 7200 秒 |
| WorkBuddy / Qoder / Cline 语义流空闲 | 300 秒 |
| Grok 语义流空闲 | 120 秒 |
| 共享拒绝排队睡眠预算 | 60 秒 |
| Qoder 已分类队列闸门探测间隔 | 5 秒 |
| 共享生成重试上限 | 20 次重试，认证重放占同一尝试预算 |

首生成包括文本、推理和工具进展，不等同可见首字；生成进展后停止该计时。成功非流 JSON 的响应头释放流式看门狗，完整正文仍受总期限约束。心跳、角色和纯用量不刷新语义空闲，父取消始终有效。内部目录读取保留更短的操作期限，不开放为用户配置。

Qoder 队列闸门仍在同一个账号上有界等待，不因共享队列拒绝切号。60 秒只计排队睡眠，网络往返和其他上游处理时间仍消耗首生成及完整期限。普通账号限流保留上游 Retry-After 与账号冷却逻辑。

Grok 的传输切号总上限固定为 20；质量暂存固定开启，最多 6 次、每轮 30 秒、8 MiB。Chat/Messages 沿用耗尽后交付最后结果的策略；原生 Responses 对已判异常的输出返回 `502 quality_degraded`，未验证缓冲超限时也不会放行正文。原生 Responses 的传输、推理解码恢复和质量重试共用调用预算。仅尚未交付且无上游副作用的请求可重放；`previous_response_id` 续接固定原账号，托管工具请求禁止跨账号重放。这些限制不能被配置文件或管理 API 覆盖。

客户端显式 `max_tokens`、`max_completion_tokens`、Responses `max_output_tokens` 及协议停止序列继续按原协议处理，未将客户端参数改成固定值。

WorkBuddy 请求未提供输出上限时，自动采用所选账号上游模型目录公布的最大输出 Token；目录缺失或没有有效的正上限时，不发送 `max_tokens`，采用上游默认行为，不设置固定兜底值。推理与答案共享这一预算。目录随既有后台刷新更新，此策略不增加可自定义字段，也不在生成时额外查询上游目录。

旧文件与 Redis 中的预算、延迟、重试及质量配置字段会被忽略，下一次正常配置保存不再写回它们；管理 API 显式提交这些字段或 `anonymous_allow_ips` 返回 400。不需要为了升级删除 Redis 或凭据主密钥。

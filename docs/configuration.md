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

## 3. 基础、安全与存储字段

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
| `anonymous_allow_ips` | 空 | 默认全部推理客户端需 Key；名单损坏时关闭匿名豁免 |
| `debug_enabled` | false | 诊断开关；pprof 路由还受启动注册和管理鉴权约束 |
| `verbose_diagnostics` | false | 仅 debug_enabled 同时为 true 才生效 |

## 4. 凭据主密钥

环境变量 `ORCHIDS_CREDENTIAL_ENCRYPTION_KEY` 优先于密钥文件。支持 32 字节主密钥对应的 base64、hex 或原文表示；不要把任意长度密码当成合法主密钥。

首次可独占创建密钥文件；并发创建时读取已存在文件。Redis 账号凭据使用 AES-GCM，旧明文记录会按迁移路径处理；已有 `enc:v1` 密文但缺密钥或解密失败会报错，不能当成空凭据继续运行。

备份必须包含 Redis、有效密钥来源和配置。多实例共享相同密钥。更名时不能随意更改密钥、加密派生域或 Redis 前缀。

## 5. 请求、重试与并发

以下为源码默认与上限，单位不可互换。非正整数通常选择默认值，并非禁用。

| 字段 | 默认 | 上限 / 行为 |
|---|---:|---|
| `max_retries` | 3 | 20 |
| `retry_delay` | 1000 毫秒 | 60000 毫秒 |
| `account_switch_count` | 20 | 100 |
| `request_timeout` | 7200 秒 | 86400 秒 |
| `concurrency_timeout` | 已归一化 request_timeout | 86400 秒；入口请求期限 |
| `retry_429_interval` | 60 秒 | 3600 秒 |
| `shared_refusal_wait_budget_ms` | 60000 毫秒 | 86400000 毫秒；共享拒绝等待预算 |
| `qoder_queue_retry_interval_ms` | 0 | 0 保留上游提示，正值覆盖间隔 |
| `concurrency_limit` | 100 | 1000000；进程总准入，满时立即拒绝 |
| `provider_concurrency_limits` | 未配置 | 显式通道入口的本进程并发；非正关闭该限制 |
| `stream_flush_interval_ms` | 有效值 2 毫秒 | 最大 20；负值关闭合并刷新 |
| `upstream_max_conns_per_host` | 由传输层处理 | HTTP 连接配额，不等同账号槽 |
| `upstream_max_idle_conns_per_host` | 由传输层处理 | 空闲连接复用 |

另有 Key 并发和账号并发限制，它们与进程准入叠加。总超时、入口超时、上游 HTTP 期限、流空闲及边缘代理超时是不同层次，最终由最先触发者决定。

共享拒绝预算较大不保证边缘代理愿意一直等待。上游提示的等待、网络尝试时间和抖动也影响真实墙钟耗时。

## 6. 自动刷新：不能混淆两个周期

`TokenRefreshInterval` 固定为 1 分钟，`AutoRefreshToken` 固定开启，`LoadBalancerCacheTTL` 固定为 5 秒；这些字段用 json:"-"，不应指导用户通过 JSON 配置修改。

1 分钟是调度检查周期，**不等于每分钟对所有账号读取完整目录和额度**。账号目录 / 套餐 / 额度观察依据到期状态与通常 30 分钟的新鲜度门槛决定；独立模型目录循环启动延迟 30 秒、随后每 30 分钟执行。手工检查可额外验证凭据，且与后台共享本进程账号刷新租约。

| 通道 | 刷新侧重点 |
|---|---|
| WorkBuddy | 令牌、目录、信用额度 |
| Qoder | 设备凭据、runtime、目录和权威额度 |
| Cline | OAuth、免费推荐目录及套餐观察，不提供统一数值余额 |
| Grok | OAuth、Build 身份、账单与目录观察 |

观察读取失败保留旧快照，不能补造余额或把认证成功等同于额度恢复。WorkBuddy 无时区额度时间按 UTC 解释，仅作参考。

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
| `grok_build_timeout_seconds` | 正值优先，最大 86400；未设时回退 request_timeout；示例文件显式为 600 |
| `grok_build_stream_idle_seconds` | 优先于旧 grok_stream_idle_seconds；有效范围 30–600 秒，缺省 120 |
| `grok_egress_enabled` | 默认未启用，开启后没有健康节点则失败，不静默直连 |
| `grok_egress_nodes` | name、url、weight、scope、proxied；URL 支持 http / socks5 / socks5h，scope 用 cli / all |
| `quality_hold_enabled` | 指针字段保留显式 false，与省略不同 |
| `quality_hold_max_attempts` / `quality_hold_timeout_ms` / `quality_hold_on_exhausted` | 质量暂存尝试、等待与预算耗尽策略；详见协议文档 |

Grok 语义空闲按有效生成事件衡量，keepalive 不延长时钟；下游背压不应计成上游读取空闲。它与其他通道的字节空闲监视不同。

## 9. 代理与缓存

`proxy_url` 为通用代理，`proxy_http` / `proxy_https` 为分离配置，另有 proxy_user / proxy_pass / proxy_bypass。实际优先级与环境代理回退由 [proxy 实现](../internal/httpclient/proxy.go) 决定；不要以 UI 的“直连”文字替代真实请求路径证据。

`cache_strategy` 默认 mix，用于请求中的真实上游缓存提示。它不提供本地答案缓存，不保证上游命中，不删除重复用户输入。Qoder 没有等价缓存 key 字段时省略该提示。

已废弃字段：inference_auth_enabled、grok_cli_model_ids，以及 enable_token_cache、token_cache_ttl、token_cache_strategy、cache_token_count、cache_ttl。旧鉴权开关不能关闭 Key；管理保存显式提交本地模拟缓存字段返回 400，旧 token-cache 管理端点不存在。

## 10. 诊断配置与排查

`diagnostics_sample_every`、`diagnostics_max_concurrent` 控制诊断采样与并发。采样意味着不是每个请求都有正文；无采集时 API 明确 unavailable，而非返回空正文证明上游没有输出。

改配置后按顺序核对：保存 HTTP 和管理 code 信封 → 当前有效值 → 是否需重启 → 重启后进程值 → 账号观察及真实请求。更多见 [故障排查](troubleshooting.md)。

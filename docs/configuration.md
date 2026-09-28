# 配置速查

从仓库根目录执行 `cp config.example.json config.json`，然后 `go run ./cmd/server -config ./config.json`。完整字段及示例值以 [`config.example.json`](../config.example.json) 和 [`internal/config/config.go`](../internal/config/config.go) 为准；不要把凭据提交到 Git。默认也会依次查找 `config.json`、`config.yaml`、`config.yml`（YAML 仅支持扁平键值）。

## 常用字段

| 字段 | 作用 |
|---|---|
| `port` | 监听端口，示例为 `3002` |
| `admin_user`、`admin_pass`、`admin_path` | 管理端账号、密码和访问路径；留空密码在启动日志中随机生成 |
| `store_mode`、`redis_addr`、`redis_password`、`redis_db`、`redis_prefix` | Redis 连接与 key 前缀；当前存储模式为 Redis |
| `credential_encryption_key_file` | 账号凭据主密钥文件，示例为 `data/credential.key` |
| `trusted_proxies` | 可信反向代理 IP/CIDR，勿信任任意客户端可访问的地址 |
| `anonymous_allow_ips` | 明确允许免 API Key 访问推理接口的来源 IP；默认空数组 |
| `debug_enabled` | 收集诊断内容，生产环境保持 `false` |
| `response_store_ttl_hours` | stored Response 记录保留小时数，示例为 `720` |
| `deployment_replicas`、`deployment_instance_id`、`deployment_cluster_id` | 多副本标识，同集群共享 Redis |
| `proxy_http`、`proxy_https` | 出站代理 |

模型与推理接口默认始终要求管理端创建的 API Key；需要免 Key 的受控来源必须显式配置 `anonymous_allow_ips`。历史配置中的 `inference_auth_enabled` 已废弃并会被忽略。Build 模型经显式路由或 OAuth 账号动态能力发现，不使用历史 `grok_cli_model_ids` 列表。

## 生效顺序与备份

启动时加载配置文件及默认值，Redis 中若已有 `<redis_prefix>settings:config`，其保存的设置会覆盖文件；部分历史字段还会被代码固定默认值覆盖。修改配置请优先使用管理页面，重启后确认实际生效值，不要只修改本地文件。

首次启动生成的凭据加密密钥**必须和 Redis 一起持久化与备份**。也可通过 `ORCHIDS_CREDENTIAL_ENCRYPTION_KEY` 提供密钥；已有账号后切勿随意更换或删除，否则无法解密账号凭据。多副本共享 Redis 和同一密钥，并为各副本设置唯一实例 ID。

生产环境还应限制服务端口及 `/metrics` 的访问范围。部署注意事项见 [部署说明](../deploy/README.md)。

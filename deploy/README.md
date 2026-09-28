# 主机部署示例

本目录是 Linux + systemd + Caddy 的**示例**，不是通用安装要求。通用启动方式见 [README](../README.md)，配置见 [配置速查](../docs/configuration.md)。部署前请将 `<HOST>`、域名、目录及服务名替换为实际值。

| 文件 | 用途 |
|---|---|
| [`Caddyfile`](Caddyfile) | 反向代理配置，应用前核对站点域名与已有站点 |
| [`orchids-guard.nft`](orchids-guard.nft) | 限制后端端口仅由本机访问 |
| [`orchids-3002-loopback.service`](orchids-3002-loopback.service) | 启动防火墙规则的 systemd 单元 |
| [`../scripts/deploy-orchids.sh`](../scripts/deploy-orchids.sh) | 校验发布包、重启并健康检查，失败时回滚 |

发布工作流 [`.github/workflows/release.yml`](../.github/workflows/release.yml) 生成 Linux/amd64 二进制与 SHA256 校验文件；上传前核对目标架构、校验和与目标主机的服务路径。按脚本帮助确认参数：

```bash
bash scripts/deploy-orchids.sh --help
```

生产环境注意：

1. 服务监听 `:3002`，必须用防火墙或等效网络策略阻止绕过反向代理直接访问。合并 Caddy 配置时不要覆盖其他站点。
2. `trusted_proxies` 只信任本地真实代理 IP；不要把任意互联网来源设为可信代理。
3. Redis 中 `<redis_prefix>settings:config` 可覆盖 `config.json`；修改配置后检查管理端实际值。备份 Redis 及凭据加密密钥，避免更新或回滚后无法解密账号。
4. 部署后检查 `/health`，再用托管 API Key 检查 `/v1/models`。限制 `/metrics` 的外部访问。

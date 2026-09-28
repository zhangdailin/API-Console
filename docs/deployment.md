# 部署注意事项

本地启动步骤见 [README](../README.md)；配置字段见 [配置速查](configuration.md)。构建需符合 `go.mod` 中的 Go 版本（当前为 **1.26.6**），使用 Redis 保存账号及配置。以下沿用当前部署工具使用的 `orchids-server` 二进制文件名；项目展示名称已改为 **API Console**，此处不是二进制改名。

```bash
go test ./...
go build -o orchids-server ./cmd/server
./orchids-server -config ./config.json
```

- 显式设置强管理密码；`debug_enabled` 保持关闭。账号凭据的 `data/credential.key`（或环境变量密钥）必须与 Redis 一同备份和恢复。
- Redis 的 `<redis_prefix>settings:config` 可覆盖文件配置，升级或修改参数后通过管理端核对实际值。
- 后端默认监听所有网卡的端口；在防火墙或反向代理上阻止直接公网访问，尤其注意 `/metrics`。`trusted_proxies` 仅填写实际代理地址。
- 多副本共享 Redis、密钥和集群 ID，并为每个副本配置不同的 `deployment_instance_id`。
- 启动后检查 `/health`，再使用 API Key 请求 `/v1/models`；必要时在管理端按通道刷新模型。回归测试执行 `go test ./...`。

仓库附带 [Caddy 与 systemd 主机部署手册](../deploy/README.md) 和 [`scripts/deploy-orchids.sh`](../scripts/deploy-orchids.sh)；这是特定主机环境的示例，应用前请核对地址、路径及防火墙规则。

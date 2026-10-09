# 部署、备份与运行手册

## 1. 部署前确认

| 项目 | 要求 |
|---|---|
| Go | 源码构建按 go.mod，当前 1.26.6 |
| Redis | 保存账号、配置、Key、响应状态与监控；需可达且持久化 |
| 平台 | 本地可构建支持平台；当前发布流程产物为 Linux amd64 |
| 管理入口 | 强密码、HTTPS、受控访问 |
| 上游 | 四通道对应授权与生成主机可达 |
| 备份 | Redis、凭据主密钥、配置与必要运行状态 |

项目正式名称为 API-Console。当前模块、二进制和运维环境变量仍保留部分旧命名，不要凭展示名称自行改 systemd ExecStart、Redis 前缀或发行产物。

## 2. 源码启动与构建

```bash
cp config.example.json config.json
go test ./...
go build -o orchids-server ./cmd/server
./orchids-server -config ./config.json
```

Windows 使用 Copy-Item 与 orchids-server.exe。Go 二进制嵌入网页，不要求在主机安装 Node.js 或从运行目录读取 web 文件。

普通 go build 默认为 dev / unknown / source 身份；仅修改文件名不能形成正式发行版。发布流程注入 version、commit、built_at、build_type 并用 --version 检查。

## 3. 发行产物校验

从项目 Release 下载同一版本的二进制、sha256 和 build-info 文件：

```bash
sha256sum -c orchids-server-linux-amd64.sha256
file orchids-server-linux-amd64
chmod +x orchids-server-linux-amd64
./orchids-server-linux-amd64 --version
```

校验目标架构、版本、commit 与下载摘要。不要用本地打包名称或网页最新版本替代运行进程身份。

### Docker 镜像

`.github/workflows/docker.yml` 构建 Linux amd64 镜像，检查容器版本、Redis 连接下的启动、健康接口、登录页和凭据密钥文件创建，再将同一个镜像发布至 `ghcr.io/zhangdailin/api-console`。PR 只构建检查，不登录或推送 GHCR；手动运行也会构建并发布。

| 触发 | 镜像标签 |
|---|---|
| push main | `main`、`sha-<12位提交号>` |
| push v1.2.3 等正式标签 | `v1.2.3`、`latest`、`sha-<12位提交号>` |
| push v1.2.3-rc.1 等预发布标签 | 原始版本标签、`sha-<12位提交号>`，不更新 `latest` |
| 手动运行 | 提交标签；源码 ref 为 main 或版本标签时还生成对应标签 |

手动运行可填写 `build_ref` 指定源码分支、版本标签或提交；留空使用所选工作流 ref。已有版本需要用新版工作流重新构建时，选择 main 的工作流并把 `build_ref` 填为该版本标签，镜像仍记录标签对应的源码提交。

发布使用自动提供的 `GITHUB_TOKEN` 和 `packages: write`，不需要额外配置 Docker Hub 密钥。已有 GHCR 包需允许此仓库的 Actions 写入；匿名拉取需在包设置中启用 Public。工作流中的容器检查不替代原有 Go / Web 测试工作流，也不证明真实上游生成可用。

首次发布后可拉取 `main`；`latest` 要等正式版本标签构建成功才存在。生产部署建议记录镜像 digest 并固定它：

```bash
docker pull ghcr.io/zhangdailin/api-console:main
docker run --rm ghcr.io/zhangdailin/api-console:main --version
```

以下为 Linux Docker 示例。先复制 `config.example.json` 为 `config.json`，把 `redis_addr` 改为 `api-console-redis:6379`，设置强 `admin_pass`，保持 `credential_encryption_key_file` 为 `data/credential.key`。配置文件需让容器 UID 10001 可读，例如将文件属主设为 10001 并使用 0600 权限。

```bash
docker network create api-console
docker run -d --name api-console-redis --network api-console \
  --restart unless-stopped -v api-console-redis:/data \
  redis:7-alpine redis-server --appendonly yes
docker run -d --name api-console --network api-console \
  --restart unless-stopped -p 127.0.0.1:3002:3002 \
  -v "$PWD/config.json:/app/config.json:ro" \
  -v api-console-data:/app/data \
  ghcr.io/zhangdailin/api-console:main
```

镜像以非 root 用户运行，网页已嵌入二进制；配置不打包进镜像。`api-console-data` 保存凭据主密钥，必须与 Redis 一起备份和保留。使用已有 Redis 时填写容器可达地址，容器内 `127.0.0.1` 不指向宿主机。替换为宿主目录挂载时，确保数据目录可由 UID/GID 10001 写入。

本地构建使用 `docker build -t api-console:local .`；更新 Go 版本时同步 Dockerfile 的默认 `GO_VERSION`，CI 自动从 go.mod 读取。容器升级通过拉取目标镜像后重建容器完成，保留上述配置和数据卷；不使用面板中的 systemd 在线二进制替换功能。

## 4. 常驻服务与反向代理

[deploy 目录](../deploy/README.md) 提供 Caddy、nftables 和部署脚本示例，需要核对域名、目录、服务名和现有站点。以下仅说明结构，不代表已创建服务：

```ini
[Unit]
Description=API-Console
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=/opt/api-console
ExecStart=/opt/api-console/orchids-server -config /opt/api-console/config.json
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
```

使用匹配目录权限的运行用户。普通运行不需要因在线升级功能强制使用 root；只有选用当前在线替换机制时才需满足其资格门槛。

程序监听 :3002，必须以防火墙 / 网络策略阻止绕过反向代理直连。TLS 终止后正确传递协议，trusted_proxies 仅信任真实代理；不可信来源转发头会被清理。

/metrics 默认未包管理认证，不能仅靠管理页密码保护。pprof 在 debug 启动条件下注册，生产尽量关闭诊断。

## 5. 部署脚本的实际范围

`scripts/deploy-orchids.sh` 当前默认为旧部署目录和服务名；使用前显式核对 --install-dir、--service、--binary-name 等参数。

它检查 Linux amd64 ELF、可选校验和、备份、同目录替换、重启及失败回退。**校验和参数是可选的**，不能描述为无条件强制完整发行验证。页面 200 / 301 / 302 与 service active 只证明 HTTP 响应，不校验实际运行映像和业务生成。

部署脚本不等同在线升级守护机制。需要额外完成以下验收，不因脚本显示 healthy 就宣布业务可用。

## 6. 部署后验收

1. systemctl status 确认服务、MainPID、实际 ExecStart。
2. 核对磁盘文件摘要和 `/proc/<PID>/exe` 的运行映像摘要，避免替换了磁盘但旧进程仍在运行。
3. 读取二进制 --version 和 /health 的 build，核对 version / commit / build_type。
4. 本机与公网分别请求 /health，确认代理和防火墙路径。
5. 登录管理端检查当前配置、账号观察、模型目录和日志采集健康。
6. 用托管 API Key 请求 /workbuddy/v1/models，再按需要执行一个受控生成请求。
7. 对流式请求检查终止事件、工具参数和最终状态，而不只看首字节。

真实生成可能消耗额度，需要选择明确账号 / 模型和请求预算。此次文档更新没有执行任何真实生成或部署。

## 7. 备份与恢复

| 对象 | 原因 |
|---|---|
| Redis | 账号密文、Key 策略、模型、配置、账本和状态 |
| 凭据主密钥文件 / 环境来源 | 解密账号；派生网关压缩状态密钥 |
| 文件配置与服务定义 | 连接信息、启动路径和权限 |
| 发行二进制与元数据 | 确定可恢复版本及平台 |
| 升级操作目录 | 回退备份、状态与故障审计 |

先确认当前实际密钥来源，不要备份一个未被使用的默认路径就认为完整。导出账号文件包含凭据，按秘密保管；它不是完整 Redis / Key 账本备份。

恢复时先恢复一致的数据与密钥，检查解密与有效配置，再开放流量。更换 redis_prefix 会让程序读取另一组键；不能把“看不到账号”直接当成数据丢失。二进制回退不自动回退数据库与配置。

## 8. 多实例

共享 Redis、前缀与凭据主密钥，每个副本设置独立 deployment_instance_id。响应历史和桥接压缩续接需要共享存储；内存响应后备不是 Redis 故障替代。

| 状态 | 跨实例情况 |
|---|---|
| 账号 / 模型 / Key / 配置 / 响应 | Redis 持久化 |
| 账号与 Key 并发槽 | 可用 Redis 租约协调 |
| Grok 部分节奏与冷却 | Redis 共享，故障时部分回退本进程 |
| 账号事件通知 / 本地客户端缓存 | 单进程 |
| 进程总并发 / 显式通道准入 | 单进程 |
| 告警当前触发集合 / 采集健康计数 | 单进程，重启影响状态 |
| 在线升级互斥 | 当前主机文件锁，不是集群滚动发布 |

不能用单副本 runtime 或 written 计数表示整个集群。配置变化后也要验证每副本实际值。

## 9. 升级与回退

先备份，选择不可变发行版本，校验产物，再更新目标副本。线上验收失败时保留新旧摘要和日志，再决定回退。在线升级资格与恢复详见 [升级手册](online-upgrade.md)。

旧名称迁移需协调源码 import、release ldflags、发行资产、升级选择器、服务路径、脚本和文档。数据 namespace、会话哈希和加密派生域需兼容设计，不做全仓机械替换。

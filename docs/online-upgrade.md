# 发行发现、在线升级与回退

## 1. 功能边界

管理后台“版本与升级”提供检查、安装、回退与操作进度。检查更新不等于安装，POST 返回 202 也只表示接受异步任务，不表示已重启成功。

默认来源由 internal/buildinfo.Repository 固定为 zhangdailin/API-Console；浏览器不能传任意仓库或下载 URL。成功检查缓存 20 分钟；强制刷新也必须遵守 1 分钟冷却时间。失败按 1、2、4、8、16、20 分钟退避；GitHub 返回更晚的有效 `Retry-After` 或匿名额度重置时间时，以更晚时间为准。该状态由单个服务进程内所有页面和后台检查共享，重启后重置。

检查响应的 `warning` 和 `check_error_code` 区分限流、认证失败、普通 403 和无可访问发行版；`next_check_at` 给出下次允许主动检查的时间。页面冷却结束只重新启用按钮，不自动向 GitHub 重试。网络失败不作为“已是最新”的证据；此前成功结果仅在原有 20 分钟有效期内保留。

匿名 GitHub REST API 额度按出口 IP 共享。可通过服务进程的专用环境变量 `ORCHIDS_UPDATE_GITHUB_TOKEN` 提供最小只读权限的 GitHub Token，避免匿名额度限制；无此变量时仍支持公开仓库检查。不要将 Token 写入仓库、浏览器或 URL，生产使用受保护的 systemd `EnvironmentFile` 等方式注入。仅发行元数据请求发送 Token；跨到非 `api.github.com` 的可信重定向会删除认证头，发行资产下载不携带 Token。配置无效 Token 会报告认证失败，不自动绕回匿名请求。

## 2. 发行要求

当前 release 工作流构建 linux/amd64，注入 Version / Commit / Date / BuildType=release 并检查 --version：

- orchids-server-linux-amd64
- orchids-server-linux-amd64.sha256
- orchids-server-linux-amd64.build-info.txt

产物名仍为兼容旧名，自动升级选择器也按该名称匹配。源码更名或改产物时要同时修改两端，不能只改 README。手动 workflow_dispatch 默认 dry_run；标签与实际发布结果才决定 Release 是否可供下载。

版本按 SemVer 比较，忽略构建元数据，正确处理预发行顺序；自动发现稳定发行。候选需比当前更新，并与实际最新发行信息一致。

## 3. 安装资格

| 条件 | 原因 |
|---|---|
| Linux、root、非普通 Docker | 当前平台替换实现依赖 systemd 和进程检查 |
| 显式启用 ORCHIDS_UPDATE_ENABLED=true | 运维 opt-in |
| ORCHIDS_UPDATE_SERVICE 指向当前持久 .service | 防止重启错误服务 |
| INVOCATION_ID / MainPID / ExecStart 匹配 | 验证当前进程归属与真实可执行路径 |
| Restart=always 或 on-failure | 故障恢复基础 |
| systemd-run 可用、二进制目录可写 | 独立守护与同目录替换 |
| 有可比较构建版本 | 不能把普通 dev 构建当正式升级基线 |

不符合资格仍可查看版本和检查发行，安装按钮禁用并显示原因。Windows 与前台 source 运行不提供该在线替换能力。

示例 drop-in（服务名按实际设置）：

```ini
[Service]
Environment=ORCHIDS_UPDATE_ENABLED=true
Environment=ORCHIDS_UPDATE_SERVICE=api-console.service
```

主应用不要设为失败后自动回收的 transient unit。守护单元是独立临时单元，以便主服务停止时仍能恢复。

## 4. 管理接口

| 方法 | 路径 | 用途 |
|---|---|---|
| GET | /api/system/version | 当前构建及安装能力 |
| GET | /api/system/check-updates | 发行发现及更新情况 |
| GET | /api/system/operation | 当前 / 最后操作状态 |
| POST | /api/system/update | `{ "version": "<TARGET_RELEASE>" }` |
| POST | /api/system/rollback | `{}`，使用可用本地回退备份 |

动作要求管理员身份、同源请求、JSON 正文（最多 4096 字节）和 Idempotency-Key。页面刷新应查询状态，不重复启动 POST。网络中断并不自动取消下载；状态和幂等信息写入本地磁盘。

## 5. 下载与替换链

1. 检查资格、版本、幂等和文件锁。
2. 读取可信 GitHub 发行元数据并匹配平台资产。
3. 受限下载二进制、摘要与 build-info；不是任意 URL 下载器。
4. 校验 SHA-256、ELF 架构、版本与 commit 等构建信息。
5. 在当前二进制目录的 .orchids-updates 中保存私有状态和旧程序副本。
6. 先启动独立回退守护，再以同文件系统 rename 覆盖可执行文件。
7. 守护重启 systemd 服务并检查目标身份。

二进制上限 150 MiB，摘要与构建信息各 64 KiB，下载有独立时间预算。HTTPS 来源和重定向受限；不会解压任意归档或执行远程安装脚本。

文件操作有原子替换与同步措施，但不是多个状态文件之间的完整崩溃事务。不能以“原子升级”概括所有数据与状态。

## 6. 成功与自动回退

守护最长 90 秒核对 systemd MainPID、`/proc/PID/exe` SHA-256，以及 /health 的 status、version、commit。健康接口 200 不是唯一判据。

| 结果 | 含义 |
|---|---|
| complete | 新运行程序身份与健康检查通过 |
| rolled_back | 新版本失败，已恢复旧程序并验证 |
| recovery_failed | 恢复也失败，需要人工处理 |
| 非终态 | 查询进度，不能先宣布升级成功 |

重启前处理 failed / start-limit 状态。回退仅恢复程序，不恢复 Redis、配置或未来不兼容迁移。操作备份不会因本次成功自动全部删除，应按审计与空间需求管理。

## 7. 人工恢复步骤

1. 读取 /api/system/operation 和对应守护日志，保存错误及新旧摘要。
2. 根据状态中的 backup 路径核对 previous_sha256，不凭目录中“最近文件”猜测。
3. 停止目标服务，确认备份平台、版本与配置兼容。
4. 将已校验备份复制到目标同目录临时文件，设置执行权限后 rename 替换。
5. reset-failed、restart，核对 MainPID、运行映像哈希、/health 和受控业务请求。
6. 保留失败操作目录，不用数据清空代替程序恢复。

守护单元名称仍为 orchids-update-<operation-id>。历史命名不代表有 Orchids 通道。

## 8. 本文验证边界

这是一份当前源码运行手册，没有再次下载、安装、重启或访问生产。既有测试覆盖 SemVer、来源、大小、摘要、幂等、锁、替换失败、守护验证及回退；测试存在不等于当前生产通过。

原文中的固定 IP、旧版本与部署哈希属于历史快照，不应放在当前安装指南中作为“当前线上”事实。部署验收请按 [部署手册](deployment.md) 重新取得证据。

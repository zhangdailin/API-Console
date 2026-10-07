# API-Console 文档中心

> 本轮源码核对日期：2026-10-04。本文档描述当前工作区的实现，不证明 GitHub、发行包或生产服务已同步。详细文档默认使用中文，项目默认首页为英文。

## 推荐阅读顺序

| 目标 | 文档 |
|---|---|
| 首次了解项目 | [架构与运行链路](architecture.md) |
| 理解关键不变量、跨模块链路与认知验收 | [系统关键合同](system-contracts.md) |
| 配置 Redis、账号及代理 | [配置完整指南](configuration.md) |
| 应用接入与管理接口 | [API 参考](api-reference.md) |
| 判断协议与模型能力 | [协议能力与兼容边界](coding-protocol-capabilities.md) |
| 部署、备份与多实例 | [部署运行手册](deployment.md) |
| 发行更新与故障回退 | [在线升级手册](online-upgrade.md) |
| 指标、日志与告警 | [运维监控](operations.md) |
| 请求失败或状态异常 | [故障排查](troubleshooting.md) |
| 开发检查及验证层级 | [开发与验证](development.md) |

## 当前产品范围

API-Console 支持 WorkBuddy、Qoder、Cline、Grok Build OAuth 四个通道。统一 `/v1` 按模型分发；固定通道使用 `/workbuddy/v1`、`/qoder/v1`、`/cline/v1`、`/grok/v1`。提供 Messages、Chat Completions、Responses 兼容接口和管理后台。

当前不包含 Warp、Orchids 通道、Grok Web / Console、公开 Grok 对话页、图像或视频生成产品入口。源码中的旧模块名 `orchids-api`、二进制 `orchids-server`、`ORCHIDS_*` 环境变量及部分 `orchids:` 数据命名仍存在，它们不是新增通道。此次更新只刷新文档，没有执行模块、数据或服务迁移。

## 文档中的证据等级

1. **源码实现**：可以定位到处理器、持久化和调用链，说明程序具备该逻辑。
2. **本地验证**：单元测试、mock 或隔离启动验证，不代表真实供应商接受全部字段。
3. **真实上游验证**：指定时间、账号、模型和请求样本的结果，不覆盖所有模型与客户端。
4. **部署验证**：需要运行进程版本、二进制摘要、服务与业务接口证据，不能由源码或 `/health` 单独替代。

不把模型目录可读取等同于生成可用，不把字段透传等同于模型遵守，不把历史报告当作当前生产状态。

## 维护方式

修改路由时同步 API 与能力矩阵；修改配置时同步字段、默认值、单位和是否需重启；修改统计时说明数据来源、分母、窗口、采样与保留范围。历史报告追加范围说明，不将原始证据改写成一次新的验证。

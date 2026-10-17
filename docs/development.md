# 开发与验证指南

## 1. 环境与结构

Go 版本按 go.mod，当前 1.26.9。前端是 Go 模板与原生 JavaScript，Node.js 用于 *.test.cjs 测试，不是应用运行依赖。Redis 用于真实启动；不少单元测试使用隔离替身。

源码尚使用 orchids-api 模块路径，release ldflags 也依赖它。当前项目展示名为 API-Console，命名审查不代表模块迁移已完成。

## 2. 基础检查

```bash
go test ./...
go vet ./...
node --test web/*.test.cjs
go build -o orchids-server ./cmd/server
```

按变更范围先运行相关包，再运行项目要求的检查。文档、样式与低风险变更不必制造只复述实现的测试；协议、并发、所有权和错误策略需行为回归。

竞态检查需要支持 CGO 与编译器的平台：

```bash
go test -race -count=1 -p 1 -timeout 15m ./...
```

当前 CI 还包括格式、静态分析、通道注册表和依赖安全检查；完整流程以 [.github/workflows/ci.yml](../.github/workflows/ci.yml) 为准。

## 3. scripts 目录用途

| 脚本 | 定位 |
|---|---|
| check-provider-registry.sh | CI 与发布依赖的生成一致性校验，-check 不重写文件 |
| check-file-size.sh | CI 非测试 Go 文件大小门槛 |
| check-file-size.ps1 | Windows 本地对应检查 |
| provider-performance.ps1 | 本地基准参数包装，恢复环境变量 |
| deploy-orchids.sh | 旧主机部署示例，不是完整业务验收 |
| verify-coding-protocols.py | 特定主机绑定的历史真实上游验证工具 |
| preview-flow.go | 仅监听 loopback 的实时拓扑模板预览，不连接 Redis 或上游 |
| verify-flow-screen.cjs | Playwright 驱动真实浏览器，检查拓扑布局、交互和异常状态并输出截图 / 视频 |
| flow-screen-fixture.cjs | 供浏览器验收与离线演示使用的显式模拟流量 |

上次仅评估了移出生产验证脚本的建议，当前文件仍在仓库。不能通过删整个 scripts 目录精简仓库而不同时修 CI / release 引用。

文件大小检查遍历 git ls-files，跳过工作树中已删除的文件。新增文件需纳入 Git 后才能被此门槛检查。豁免是整文件跳过，不限制其增长；缺失或已降至 700 行以内的豁免会报错，拆分后须同步移除 PowerShell 与 Shell 两份清单。

## 4. 测试证据层级

| 验证 | 可以证明 | 不能证明 |
|---|---|---|
| 单元 / mock | 映射、状态机、隔离与失败行为 | 供应商当前接受协议 |
| 前端 VM | DOM 逻辑、错误展示、请求顺序 | 真实浏览器布局、OAuth 成功 |
| 本地启动 | 路由与配置装配可运行 | 所有模型可生成 |
| 真实 HTTP 探针 | 某账号模型请求结果 | 完整编程客户端及全部模型 |
| 部署验收 | 指定运行映像和服务行为 | 下一次升级或历史数据完整性 |

报告应写清命令、平台、数据源、通过 / 失败 / 未运行，保留上游排队等未完成项。不要把测试数量的历史值写成当前固定事实。

### 实时拓扑浏览器验收

先启动 `go run scripts/preview-flow.go`，另一个终端执行 `node scripts/verify-flow-screen.cjs`。需要可用的 Playwright 包、Chromium 和 Playwright 的 FFmpeg；可通过 `PLAYWRIGHT_MODULE` 指向现有 Playwright 安装，通过 `CHROME_BIN` 指定 Chrome 路径。此预览仅监听 `127.0.0.1:3099`，不提供真实管理 API。静态资源使用 Go embed，修改资源后须重启预览进程。

验收脚本向真实浏览器注入明确的模拟 SSE 与账号池响应，覆盖无账号、等待输出、持续输出、突发、失败、断线、实例重启和池读取失败，以及主题、全屏、键盘、减少动态效果、1920 / 1440 / 4K / 390px 布局。单元测试另检查后台清理、计时器边界、趋势缺口和池数据过期。

默认产物保存到被 Git 忽略的 `docs/verification/flow-screen-20261017/`，可用 `FLOW_ARTIFACTS` 指定其他目录。包含 `browser-report.json`、各尺寸截图、`flow-screen-demo.webm` 和可离线打开的 `interactive-demo.html`。模拟演示验证展示与交互，不证明真实上游性能。服务器部署与 GitHub 发布需另行安排。

## 5. 协议变更重点

检查显式零与字段省略、工具身份与参数聚合、多工具索引、推理签名、SSE 终止、流后错误、跨 Key 所有权、历史 TTL、压缩状态隔离以及不能重放的副作用请求。

共享实现放在 chatwire / responses / httpclient / httpserver；Grok 的生产适配保持兼容薄层，纯测试适配放在 *_test.go。错误策略应追到 accountpolicy 与持久 writer，不能只改公开文案。

### 清理前的证据

可用 `staticcheck ./cmd/... ./internal/... ./web` 和 `deadcode -test ./cmd/... ./internal/... ./web` 定位候选，再核对源码、调用方及平台构建。只在生产不可达、但仍由测试验证的协议转换可移入 `*_test.go`；不能因此删除协议断言。登录失败预算、凭据双重脱敏、费用幂等结算、兼容数据读取和加密域名不能仅凭重复或旧名删除。

Windows 本地检查使用 `GOARCH=amd64`。跨平台静态分析 Linux 代码时同时设 `GOOS=linux`、`CGO_ENABLED=0`；这只能证明分析和编译，不代表在 Linux 执行了测试。工作区的 `docs/verification/` 是被忽略的本地材料，默认检查使用上述正式包范围，避免历史临时 Go 文件混入。

浏览器指纹客户端仍通过 `golang.org/x/net/http2.Transport` 接入 uTLS 握手。静态检查的弃用提示需要结合 TLS 指纹、ALPN、代理与流期限合同评估，不能直接替换成标准 transport 来消除提示。

## 6. 性能验证

provider-performance.ps1 驱动本地 BenchmarkProviderLocal，可选择通道、开环、预热、GC 与目标门槛。固定 cpu=18 等参数是该基准配置，不是所有机器的合理运行配置。

本地子路径吞吐不包含完整公网、Redis、代理、真实模型耗时与计费成本；不能声称网关实际达到相同 RPS。历史 overlay 与当前结构可能不兼容，按当前脚本参数使用。

## 7. 真实上游探针

部分 live 测试有 build tag 与显式环境开关，默认 CI 不运行；路径可能随分层重构变化，运行前以 rg 查找当前测试函数及构建标签，不照搬历史命令。

```bash
rg -n 'go:build live|func TestLive|QODER_PROBE|WB_LIVE|TLS_PROBE' internal
```

历史 verify-coding-protocols.py 绑定配置目录、服务名、13002 端口及 Linux /proc，会读取生产进程密钥并复制账号快照。它不是即插即用的公开测试入口；运行可能消耗真实额度，需隔离命名空间、明确账号和精确清理对象。

## 8. 文档与交付

修改后更新对应运行指南，给历史报告标明范围。交付时分开报告本地检查、Git 提交、push、部署和在线验证。源码更新不自动意味着发布或生产更新。

受管理文件稳定后维护 AOCI；不能用扫描路径或符号列表自动拼出索引语义。仅文档更新也需同步相关索引，但不需要为了文档运行真实生成探针。


## 真实流式 API 性能测试

`go run ./cmd/apibench -base https://gateway/workbuddy/v1 -model fast-model -n 100 -c 2 -max-tokens 1024 -output bench.json` 从环境变量 `API_BENCH_KEY` 读取推理 Key。报告不保存 Key、提示词或输出正文，默认 1 次预热排除、20 次测量、并发 1。每请求默认 90 秒总截止时间，可用 `-timeout` 调整；`-effort low` 可显式比较推理档位。真实请求消耗上游用量，工具不自动创建 Key 或修改网关配置。

报告分别给 HTTP 成功、DONE 与结束原因均存在的协议成功、可见答案、仅推理、输出预算截断。P90/P95/P99 使用最近秩，首生成/首字只统计协议成功且真正测到该事件的样本，E2E 包含失败。`visible_delta_gap` 是 SSE 文本增量间隔，不是 Token ITL。上游报告的输出吞吐包含推理、排队及预填充，不是纯解码速度。闭环并发吞吐不是极限容量；不足 100 个请求的 P99 只作探索，跨模型、预算、推理档位和并发不能混合比较。

配合日志中的请求 ID 对照源站和公网时间。账号、凭据刷新和模型排队的差异需更多样本支持，不能因单次尾延迟禁用账号或破坏会话亲和。

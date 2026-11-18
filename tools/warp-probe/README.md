# Warp dev 最小低消耗测试版

独立 Go 模块，不注册网关渠道，不改主仓库依赖，不接 Redis，不部署线上，不读取 Warp 本机账号文件。

## 默认行为与成本边界

| 模式 | 实际行为 | 自动生成 |
|---|---|---|
| 无参数 | 离线检查配置是否存在 | 否 |
| `--auth-start-only` | 申请设备授权后退出，不批准此模式的设备码 | 否 |
| `--oauth-login` | 等待官方浏览器批准，确认 OAuth 返回授权结果后丢弃内存凭据 | 否 |
| `--live` | 登录交换并读取账号启用模型目录 | **否** |
| `--live --generate --model <精确ID>` | 对当次目录实际列出的明确模型执行一次固定文本测试 | **是，须显式授权** |

`--live` 不再自动生成，也不选择默认模型调用。`auto`、所有 `auto-*`、CLI/computer-use 自动子代理别名被拒绝；不把省钱别名重写成另一条自动路由。目录列表不是价格表，不能由名字推断哪个模型最便宜。

**仍使用 `/ai/multi-agent` protobuf 接口。** 当前没有已验证的普通聊天替代端点；禁用客户端能力不代表上游绝不会产生内部活动或平台费用，不承诺按普通聊天计费。

## 最小请求

- 只发送一次固定提示 `Reply exactly OK.`，不接受任意 prompt、历史、文件或工具定义。
- 模型必须来自本次账号目录，并按原 ID 原样发送；不静默回退。响应若披露模型替换则失败。
- 不填 CLI/computer-use/planning/coding 子代理模型，不携带 MCP、会话历史、文件内容或动态时间。
- 所有能力布尔项显式关闭：搜索、网页检索、文件/命令、研究、编排、电脑操作及文件历史保留等。
- 工具列表不能空置，因为协议将空列表解释为任意工具。使用单项被动建议围栏，同时关闭建议能力；收到任何工具事件（包括 server/suggest）、搜索、技能或代理活动就立即失败，不执行、不续轮。
- 未理解的消息类型、跨任务迁移、额外会话启动与不透明服务端数据更新失败关闭；事务只支持单层 begin/commit/rollback，回滚文本不计为成功。
- 本地生成期限 30 秒、回答 4 KiB、SSE 行/事件 1 MiB、完整流 8 MiB。无自动重试、无切号、无第二轮。
- 成功仅指一次完整非空文本回答与明确 `Finished.Done`，不是协议全面兼容。提示要求 OK，但探针不记录正文，也不宣称检查了精确内容。

本地取消只会关闭请求，**不保证上游立即停止或停止扣费**。输出和期限上限不是服务端 token/credit 硬上限。

## 构建与测试

```bash
cd tools/warp-probe
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go test ./... -count=1
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go test -race ./... -count=1
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go vet ./...
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go build -o warp-probe .
./warp-probe
```

只使用已缓存的固定版本官方 protobuf 与依赖；离线命令不会下载依赖。

## 先验证 OAuth 登录

```bash
./warp-probe --oauth-login
```

打开本次程序输出的官方 Warp 授权地址，确认设备码后登录批准。此模式无需 Firebase 项目配置，但只验证设备授权，不证明最终推理凭据、目录或模型可用。结束后凭据丢弃。不要使用先前已过期的链接，也不要回传密码、Cookie 或 Token。

## 登录与只读模型目录

完整 Firebase 交换需要 Warp 对应的 `ORCHIDS_WARP_FIREBASE_API_KEY` 项目配置。程序不内置该值；不能拿任意 Firebase Key、密码或其他 Token 代替。

在自己的交互 Bash 终端私密输入；不要发送到聊天、命令参数或提交中：

```bash
cd /home/zhangdailin/Documents/Orchids-2api/tools/warp-probe
(
  read -rsp 'Warp Firebase project configuration: ' ORCHIDS_WARP_FIREBASE_API_KEY
  printf '\n'
  export ORCHIDS_WARP_FIREBASE_API_KEY
  ./warp-probe --live
)
```

成功返回 `catalog_only_passed`。不携带生成授权，因此不会调用 `/ai/multi-agent`。进程结束后放弃内存 Token；子 Shell 不保留配置。

## 单次生成测试（可能扣费）

确认目录及账号实际价格后，将上例最后一条命令替换为：

```bash
./warp-probe --live --generate --model '<本次账号目录中的明确模型ID>'
```

每次运行重新授权、重新读取目录；不跨进程保存 Token。不指定精确模型或使用自动路由别名会在联网前拒绝。

可附加：

```bash
--max-reported-credits 1
```

它只是**响应后的观测告警**：

- 仅累计 `StreamFinished` 中实际存在的本请求数值；优先 `request_charges`，无新格式时读取旧格式，不叠加两份，也不使用累计 conversation usage。
- 输出输入/输出 token、缓存读写 token、推理及平台 credits/cents；未报告的字段保持未知，不标成免费，不将部分值伪装完整账单，不换算单位。
- 返回值可能跨 Warp、BYOK、自定义端点，代表被报告的组成部分，不是完整扣费凭证。
- 超过阈值会失败；低于阈值仍不能证明账单完整，会返回“预算未验证”。不承诺零超支或收费前切断。
- 用量通常在结束时才到达，工具拒绝、超时或中途断流时可能无法取得费用，应明确按未知处理。

## 凭据与验证状态

所有凭据只在进程内，不输出 ID/custom/refresh Token、不保存账号 JSON、不写 Redis；HTTP 重定向不跟随，错误不附上游正文、请求 URL 或认证头。标准环境代理可以使用，但不自动切换出口。

本地单元/竞态测试验证了最小请求、明确模式、重定向拒绝、工具/代理活动拒绝、事务/流边界及账单未知与零的区别。官方设备授权申请曾实测成功；此前 OAuth 等待用户批准时过期，**尚未证明真实登录、目录或生成通过，更未证明真实费用降低或缓存命中**。

当前实现是可运行的最小测试版，不是已恢复的生产 Warp 渠道。

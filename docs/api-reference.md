# API 参考

## 1. 地址与身份

接口注册以 [routes.go](../cmd/server/routes.go) 为准。默认服务地址 http://127.0.0.1:3002，管理页面 /admin/。

| 前缀 | 路由规则 |
|---|---|
| `/v1` | 根据请求 model 选择通道 |
| `/workbuddy/v1` | WorkBuddy 账号池 |
| `/qoder/v1` | Qoder 账号池 |
| `/cline/v1` | Cline 账号池 |
| `/grok/v1` | Grok Build OAuth 账号池 |

推理和模型接口使用管理后台创建的 Key：`Authorization: Bearer <API_KEY>` 或 `x-api-key: <API_KEY>`，同时提供时 Bearer 优先。管理接口使用管理员会话或既有管理 token，不接受推理 Key 替代管理员。

anonymous_allow_ips 空值要求所有来源使用 Key，名单配置损坏也要求 Key。inference_auth_enabled=false 不会关闭鉴权。

### 1.1 编程客户端与 CC Switch

四个固定渠道都提供 OpenAI Chat Completions、Responses（Codex）及 Anthropic Messages（Claude Code）。OpenAI / Codex Base URL 使用上表含 `/v1` 的地址；Claude Code 和 Anthropic SDK 使用站点根地址或 `/{channel}`，由客户端追加 `/v1/messages`。其他客户端是否需要 `/v1` 取决于其追加路径方式。

管理后台「配置管理 → API Key 管理」每条密钥提供复制与 CC Switch 导入：点击该密钥的导入按钮，选择 Codex 或 Claude Code、渠道和目录模型，再点击导入。弹窗按需读取完整密钥并自动读取默认 WorkBuddy 目录；切换渠道重新读取。目录使用该 Key 请求渠道公开 `/models`，因此遵守公开可见性与 Key 模型权限，不使用管理列表代替。Key 或渠道改变后必须重新读取；没有目录结果时不能导入。

导入使用 [CC Switch 官方深链接协议](https://github.com/farion1231/cc-switch/blob/main/docs/user-manual/en/5-faq/5.3-deeplink.md) `ccswitch://v1/import`，要求本机已安装并注册协议的 CC Switch，浏览器允许打开应用。应用中的确认和启用仍由用户完成，网页不能检测导入成功。Claude Code 的默认模型及 Haiku / Sonnet / Opus 别名均映射到所选模型；Codex 使用 Responses，CC Switch 默认生成 `model_reasoning_effort="high"`，不支持 high 的模型需在启用前按目录能力调整或移除此字段。

完整 Key 不写浏览器存储，仅用于同源模型请求与本机协议导入；深链接本身含密钥，不应分享。新建或轮换的 Key 可从列表按需复制和导入；旧版仅存哈希的 Key 继续有效，需要手动轮换后才支持复制和导入，列表中的掩码不可使用。旧教程地址自动转到 API Key 子标签。该功能不自动测试真实生成，也不承诺全部模型支持视觉、搜索或完整编程工具。

## 2. 推理与目录接口

下表路径相对于前缀：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/models` | 可见模型目录，具体响应还可能包含编程客户端能力投影 |
| GET | `/models/{id}` | 单模型详情 |
| POST | `/messages` | Anthropic Messages 兼容输入输出 |
| POST | `/chat/completions` | Chat Completions 兼容输入输出 |
| POST | `/responses` | Responses，Grok 原生 Build，其他通道 Chat 桥接 |
| POST | `/messages/count_tokens` | 本地 Token 估算；统一、WorkBuddy、Qoder、Cline 入口，Grok 专属前缀未注册 |
| POST | `/responses/compact` | 网关摘要 / Build 压缩路径；四个固定通道及统一前缀均有相应分派 |

模型应从当前目录选择，不要把示例字符串当可用白名单。目录正常不证明账号拥有生成额度；目录失败保留旧观察。

### 2.1 模型与聊天

```bash
curl http://127.0.0.1:3002/v1/models \
  -H 'Authorization: Bearer <API_KEY>'

curl http://127.0.0.1:3002/v1/chat/completions \
  -H 'Authorization: Bearer <API_KEY>' \
  -H 'Content-Type: application/json' \
  -d '{"model":"<MODEL_ID>","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

stream 省略时可能采用服务默认，需 JSON 非流式结果时显式 stream:false。max_completion_tokens 在 Chat 请求中优先于 max_tokens；各上游可能进一步规范化预算和推理档位。

### 2.2 Messages

```bash
curl http://127.0.0.1:3002/v1/messages \
  -H 'x-api-key: <API_KEY>' -H 'Content-Type: application/json' \
  -d '{"model":"<MODEL_ID>","max_tokens":256,"messages":[{"role":"user","content":"你好"}]}'
```

工具声明、tool_use / tool_result 及多模态内容必须满足格式与配对要求。count_tokens 是本地估算，不是官方 tokenizer 的精确结果，也不是实际计费用量。

### 2.3 Responses 与续接

```bash
curl http://127.0.0.1:3002/v1/responses \
  -H 'Authorization: Bearer <API_KEY>' -H 'Content-Type: application/json' \
  -d '{"model":"<MODEL_ID>","input":"你好","stream":false,"store":true}'
```

保存成功后，使用返回的 response ID 和同一 Key 续接：

```json
{"model":"<MODEL_ID>","previous_response_id":"<RESPONSE_ID>","input":"继续解释","stream":false,"store":true}
```

不应假定所有原生响应默认 store:true。Grok 适配有自己的默认与资源策略；桥接保存终止对象，上游原生正文和本地所有权记录也不同。

## 3. 响应资源

| 方法 | 相对路径 | 语义 |
|---|---|---|
| GET | `/responses/{id}` | 获取响应；桥接来自网关存储，Build 按原生所有权及上游策略处理 |
| DELETE | `/responses/{id}` | 删除当前调用方可访问的响应记录 / 对应资源 |
| POST | `/responses/{id}/cancel` | 取消网关记录或合成取消投影，不保证停止上游生成 |
| GET | `/responses/{id}/input_items` | 返回已保存输入；旧记录没有输入字段时为空列表 |

所有权绑定 Key 摘要，跨 Key 或未知 ID 返回 404；存储失败返回 503。Response TTL 默认 720 小时。只有所有权、无本地正文的记录不能当作完整上游内容缓存。

### 压缩

```json
{"model":"<MODEL_ID>","input":[{"role":"user","content":"需要压缩的历史"}],"stream":false}
```

将该请求发送至 `/v1/responses/compact`。桥接返回 object:response.compaction 与网关摘要引用；不是可在任意上游复用的推理密文。用同一 Key、模型和入口渠道续接，并在 TTL 内使用。压缩会调用模型并消耗实际用量，不执行用户工具。详情见 [协议能力](coding-protocol-capabilities.md)。

## 4. 流式结果与失败

SSE 以事件和 data 交付。客户端应解析对应协议的终止状态，不应把 HTTP 200、EOF 或某个文本 delta 当作成功完成。流开始后错误可能通过 SSE 事件输出，已提交的 HTTP 状态不能改成 502。

Chat 桥接写递增 sequence_number，便于客户端识别顺序；当前没有因 Last-Event-ID 自动回放历史事件的实现承诺。重复文本与推理 delta 不会因次数中止。工具参数应聚合完整后再执行，避免把中间 JSON 当可执行调用。

## 5. 管理接口

管理响应并非全都使用同一信封；部分为数组 / 对象，配置等使用 `{code,data,msg}`，部分错误为纯文本。客户端应同时检查 HTTP 状态、Content-Type 与业务 code，不能只看 200。

| 路径 | 方法 / 用途 |
|---|---|
| `/api/login` | POST username/password，成功设置 session_token cookie |
| `/api/logout` | 清除当前会话与 cookie |
| `/api/providers` | GET 当前提供者注册表 |
| `/api/accounts` | GET 列表、POST 创建；各通道创建限制以授权路径为准 |
| `/api/accounts/{id}` | GET / PUT / DELETE；账号编辑及刷新分支以处理器为准 |
| `/api/keys` | GET 列表、POST 创建 |
| `/api/keys/{id}` | PATCH 策略、DELETE 删除 |
| `/api/keys/{id}/secret` | GET 管理员按需读取完整秘密，禁止缓存；旧 Key 返回 409 |
| `/api/keys/{id}/rotate` | POST 轮换秘密，旧 Key 失效 |
| `/api/keys/{id}/reset-usage` | POST 重置账期用量，区别于正常账期滚动 |
| `/api/models` | GET 列表、POST 创建；分页参数存在时响应形态与裸列表不同 |
| `/api/models/{id}` | 更新 / 删除模型 |
| `/api/models/refresh` | POST 按通道刷新目录 |
| `/api/config/list` | GET 当前配置的管理投影 |
| `/api/config/save` | POST 增量配置补丁 |
| `/api/export` / `/api/import` | 账号导出 / 恢复；导出文件包含可恢复凭据 |
| `/api/ops/overview` | GET 指标、窗口、渠道模型视图、健康与覆盖信息 |
| `/api/ops/runtime` | GET 进程与宿主运行指标 |
| `/api/ops/alerts/rules` | GET / PUT 告警规则 |
| `/api/journal/records` | GET 请求 / 操作 / 系统日志及关联尝试 |
| `/api/journal/diagnostics` | GET 已采集请求诊断 |
| `/api/journal/diagnostics/settings` | 读取 / 更新诊断设置 |
| `/api/system/version`、`check-updates`、`operation` | GET 版本、发行发现和升级状态 |
| `/api/system/update`、`rollback` | POST 异步系统操作，见升级手册 |

API Key 创建和轮换时返回完整秘密，并使用主密钥派生的独立加密用途保存密文；认证继续使用哈希。管理员可通过 secret 接口读取，列表仅返回掩码及 secret_available。加密能力未配置、密文损坏或解密失败时明确报错，不回退明文。轮换原子替换密文、哈希及索引，使旧 Key 立即失效；列表掩码不能用于调用。前端创建默认消费不限额，编辑不提交预算字段，已有后台预算策略继续生效。支持模型白名单、请求限速、有效期、并发及预算；空模型白名单兼容解释为不限模型。费用预留和结算用整数 USD ticks，1 USD 为 100 亿 ticks。

### 管理登录示例

```bash
curl -c admin.cookies http://127.0.0.1:3002/api/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"<ADMIN_PASSWORD>"}'

curl -b admin.cookies http://127.0.0.1:3002/api/models/refresh \
  -H 'Content-Type: application/json' -d '{"channel":"qoder"}'
```

cookie 文件按管理秘密保管。授权启动要求同源 HTTPS 或 loopback；建议直接用后台页面完成流程，避免遗漏 Origin 与事务状态。

## 6. 官方授权事务

| 通道 | 启动路径 |
|---|---|
| WorkBuddy | POST `/api/workbuddy/login` |
| Qoder | POST `/api/qoder/login` |
| Cline | POST `/api/cline/login` |
| Grok Build | POST `/api/grok/device-auth` |

启动后根据返回的事务和官方 URL 完成浏览器授权；GET 同路径 `/{id}` 查询，DELETE `/{id}` 取消。持久 token、device code、PKCE verifier 等保留服务端，浏览器仅接收授权进度。每通道有事务准入上限，过期或取消后不能拿旧轮询结果覆盖新事务。

Qoder / Cline / Grok 不提供手填个人 token 的通用创建方式；WorkBuddy 凭据导入用于迁移。账号管理投影脱敏，不表示导出文件也没有凭据。

### 6.1 账号导出与恢复

账号管理提供「导出全部（含凭据）」与「导入账号」。导出涵盖全部渠道，不受当前渠道筛选或选中行限制，格式为 `version:1`、`export_at`、`accounts`。文件包含本渠道的明文访问令牌、刷新令牌及设备/身份、账号设置和已有目录快照；剔除其他渠道凭据，旧通用凭据尽可能规范到对应渠道字段。下载响应禁止缓存，文件本身不是加密文件。

备份可导入另一个使用不同凭据加密主密钥的实例：导出前存储层已解密，导入由目标存储层重新加密。正常账号列表仍脱敏，不可用列表响应代替备份。API Key、独立模型管理记录、请求日志、Redis 命名空间及完整账本不在账号 JSON 中；账号 ID 和请求/当日 Token 计数不会恢复为原实例数据。恢复整个实例仍应备份 Redis 和加密主密钥。

页面选择不超过 8 MiB 的 JSON，先在浏览器本地预览各渠道数量，再明确确认 POST `/api/import`。文件内容仅暂存在页面内存；导入或关闭后释放。后端只接受一个 `version:1` JSON 文档及 accounts 数组。四渠道均要求持久刷新令牌；Qoder 还要求设备 Machine ID，保留 UID、组织、runtime 与目录材料。缺少必要恢复材料和退役渠道逐条跳过，不创建空凭据账号。

导入采用追加方式，不覆盖已有账号。渠道稳定身份或持久凭据相同视为重复；同一文件内重复及再次导入也跳过。响应包含 total、imported、skipped、duplicates、invalid、failed 以及 issues（1-based index、稳定 reason）；可能部分成功，不是整个文件的原子事务。存储读取失败则不开始写入，逐条写入失败会报告。单进程导入串行化，不承诺跨实例导入与同时登录之间的全局原子去重。

导入成功只证明凭据和账号状态已存储，不证明上游仍接受凭据，不会自动发送生成请求或刷新令牌。访问令牌过期通常可通过有效刷新令牌续期；历史备份中的刷新令牌若已轮换、撤销或所属账号被冻结，则仍需重新官方授权。保留的账号禁用、冷却、额度与健康观察也可能影响可选性。恢复后检查账号、重新读取模型目录，必要时点击账号刷新；不要让源与目标长期同时轮换同一份 OAuth 凭据。

## 7. 错误与排查

| 状态 | 常见原因 | 下一步 |
|---|---|---|
| 400 | JSON、媒体、工具配对、无等价协议控制 | 修正请求，读取 error.code / param |
| 401 | 网关 Key 缺失、无效或过期 | 检查客户端 Key，不用上游 token 替代 |
| 402 | Key 费用预算不能覆盖预留 | 调整预算或模型，不是普通重试错误 |
| 403 | Key 模型权限等入口拒绝 | 核对允许模型 |
| 404 | 模型权益不匹配、资源不存在或跨所有者 | 换模型、补账号或检查 ID 与 Key |
| 429 | 额度、并发、Key RPM、模型冷却或共享排队 | 根据具体类别与 Retry-After 等待 |
| 502 | 协议截断、严格输出不匹配等 | 查看终止事件和诊断，不默认处罚凭据 |
| 503 | Redis / 上游不可用、账号池无法服务或进程准入 | 区分入口、存储与上游链路 |

上游凭据认证失败可映射为服务不可用，避免误导客户端去轮换正常网关 Key。公开错误脱敏，原始上游诊断需要管理权限。

## 8. 公共与已退役路由

GET /health 返回 status、构建信息和注册通道；providers:ready 表示已注册，不是逐账号实时生成探测。GET /metrics 提供 Prometheus，默认路由不附管理认证，需网络隔离。pprof 仅 debug 启动条件注册并要求管理身份。

根路径跳转管理入口。旧公开聊天 / 媒体别名、token-cache stats / clear 未注册。不要根据目录中的历史说明调用已删除端点。

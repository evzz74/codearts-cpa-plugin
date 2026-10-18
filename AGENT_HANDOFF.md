# CodeArts CPA Plugin Handoff

更新时间：2026-10-18（第八轮：按 codebuddy-intl-cpa 的仓库结构重排；第七轮：清理为纯插件仓库；第六轮：面板对齐码道控制台）

## 目标

将 `codearts2api` 从独立 HTTP 服务迁移为 CLIProxyAPI（CPA）原生插件，最终构建：

- Windows amd64：`codearts.dll`
- Linux amd64：`codearts.so`

参考项目：`D:/Users/XIAO/Documents/.VSCode/codebuddy-intl-cpa-main/workbuddy`

用户已确认：最终只保留 CPA 插件形态；首版需要聊天、流式、工具调用、模型发现、OAuth 登录/刷新、凭证导入、管理页面、签到和保活。

## 当前状态

插件形态已实现完毕并通过本机构建/单测/静态检查。第七轮把仓库清理成**纯插件仓库**：
独立服务形态的 `cmd/`、`internal/pool`、`internal/scheduler`、`internal/server`，以及
`deploy/`、Docker 文件、shell 脚本、`config.example.json`、`.env.example`、`.vscode/`、
根目录构建产物和 `.ref/` 临时目录都已删除，可以整体上传 GitHub。
第八轮把目录结构对齐参考插件（见下），并补上 `VERSION` / `registry.json` / 打包脚本 /
多平台 CI 发布链路。
仍未做真实宿主端到端复验（原因见“未完成”）。

### 插件文件（`internal/`，package main）

第八轮按 `zidanefaqih/codebuddy-intl-cpa` 的仓库结构重排：插件源码从
`plugin/codearts/` 移到 `internal/`（`package main`，c-shared 入口同包），
协议层从 `internal/upstream` 提到根目录 `upstream/`，`internal/auth` 合并进插件包
（`auth_model.go`），面板移到 `internal/assets/panel.html`。

| 文件 | 职责 |
|------|------|
| `main.go` | cgo C ABI（`cliproxy_plugin_init`、`cliproxyPluginCall/Free/Shutdown`）、RPC 分派、注册 metadata 与能力声明（版本号由 `-X main.version` 注入） |
| `auth.go` | 凭证归属判断、`auth.parse`、`auth.refresh`（保留未知字段 / metadata / DPoP / client_id） |
| `auth_model.go` | `Auth` 结构、`auths/codearts-*.json` 读写，以及满足 `upstream.Account` 的 `AccountID`/`Credential` |
| `errors.go` | `pluginError` 状态映射、`rpcErrorEnvelope`、刷新重试、宿主 auth 回调（`hostAuthList/Get/Save`，可被测试替换） |
| `models.go` | 静态模型、按凭证发现模型、OAuth model alias 反查 |
| `executor.go` | `chat-completions` 执行器：tools、`tool_choice`、reasoning、非流式 SSE 聚合、异步流式 emit/close |
| `transport.go` | 宿主 HTTP 桥（`host.http.*`）、`hostStreamCall`、`hostStreamClose(id, message)` |
| `login.go` | OAuth PKCE + DPoP、loopback 回调、远程粘贴回调、ticket 轮询、`host.auth.save` |
| `management.go` | 管理 API 注册/分发、鉴权、账号操作、内嵌面板 |
| `usage.go` | 用量账本（按账户/天/模型汇总、估算、落盘） |
| `usagehook.go` | 宿主 `usage.handle` 回调入口 |
| `panel.html` | 管理面板（含控制台概览区，位于 `internal/assets/`） |

### 本轮行为变更（重要）

**0. 面板对齐码道控制台：积分扇形图 / 今日 tokens / 用量记录（第六轮）**

- 新增上游只读接口 `upstream.FetchStatistics` / `ParseStatistics`（`upstream/statistics.go`），
  打 `GET /snap-manager/v1/statistics/plugin`，即控制台 `AccountInfoPane` 用的同一个接口。
  - **响应是裸对象**（没有 `code`/`data` 信封），和 `/v1/ops/*` 不同；解析器也兼容信封形态。
  - 积分总额只能取 `metrics[usageTotalPackageCredit]`，**不要**把分类 metric 相加（会重复计算）。
  - `package_credit_amount/used/remain` + `start_date`/`end_date`（积分包周期）+ `usageTokenChatMessages.usage_token_num`。
  - 实测（真实账号）：总额 6500、已用 3.42、剩余 6496.58、周期 2026-10-17~2026-11-18、周期 tokens 41938。
- 新增管理路由 `GET /stats`、`GET /usage`（都在 `/v0/management/plugins/codearts` 下），
  并同步加到无密钥读代理 `/v0/resource/plugins/codearts/call?op=stats|usage`。
- **今日 tokens / 用量记录必须插件自己记账**，原因（已核实 CPA v7.2.30 源码）：
  - 插件执行器这条链路**不会**产生 `usage.Record`：`usage.PublishRecord` 只出现在
    `internal/runtime/executor/helps/usage_helpers.go`，而 `NewExecutorUsageReporter` 只被内置执行器调用；
    CodeArts 的 SSE 里也没有 usage 帧。
  - 宿主的 `redis-usage-queue` 只保留 60 秒且是 pop 语义，做不了历史。
  - 所以记账点放在两个执行器入口（`handleExecute` / `handleExecuteStream`）：上游给 usage 就用真实值，
    否则按字符数估算并在明细里标 `estimated`（CJK 1 字≈1 token，其余 4 字符≈1 token）。
  - `usage_plugin` 能力仍然打开并实现 `usage.handle`：宿主一旦推真实记录就直接入账。
  - 账本落盘 `<UserConfigDir>/codearts2api/usage-ledger.json`（可用 `CODEARTS_USAGE_DIR` 覆盖；
    宿主 registration 只有 `config_yaml`，**不提供**数据目录）。保留 500 条明细 + 62 天按天汇总，
    被截断的旧记录并入日汇总，保证「今日」数字不会因为截断而变小。
- 面板新增 `#console` 区块：环形（扇形）百分比 SVG（`pathLength="100"`，`stroke-dasharray` 直接等于百分比）、
  今日 tokens + 14 天柱状图 + 模型 Top5、用量记录表（明细/按天切换）。中英双语、三套主题都覆盖。
- 测试：`upstream/statistics_test.go`（口径/空包/信封/Token 账户/业务码）、
  `internal/usage_test.go`（汇总、落盘、截断、设备隔离、估算、`/stats`、`/usage`、usage.handle）、
  `internal/panel_test.go`（`node --check` 脚本、DOM 契约、路由接线、i18n 覆盖、payload 形状）。


0. **登录改走宿主标准插件链路（第五轮，方法 A）**
   - 第四轮结论要更正一半：CPA v7.2.30 **确实**为插件开放了登录入口，
     就是动态路由 `GET /v0/management/<provider>-auth-url`，由
     `auth_files.go:268 ServePluginAuthURL` 处理 → `host.StartLogin(provider, baseURL)`
     → `RegisterPluginOAuthSession(state, provider, metadata)`。轮询走
     `GET /v0/management/get-auth-status?state=` → `host.PollLogin` →
     `savePluginLoginRecords`。**这条链路每一环都用插件自己的 provider 名，不会 400。**
   - 400 只出在 TUI 的「回调 URL」输入框：`internal/tui/oauth_tab.go:265 submitCallback`
     从硬编码的 5 个内置 provider 里查 `providerKey`，插件 provider 得到空串；
     而宿主回退逻辑是 `if provider == "" { provider = sessionProvider }`，
     **字段被显式写成空串时回退不触发** → `NormalizePluginOAuthCallbackProvider("")`
     → `errUnsupportedOAuthFlow` → 400 `unsupported provider`。这是宿主 bug，插件改不了，
     但可以完全绕开。
   - 现在插件的登录主线是宿主标准链路，`/v0/management/oauth-callback` 一次都不碰：
     1. 面板从 `/v0/resource/plugins/codearts/bootstrap` 拿到 `auth_url` / `auth_status_url`。
     2. 「开始授权」直接打 `GET <mgmt>/codearts-auth-url`，宿主回调 `StartLogin`，
        插件绑 loopback、返回 `{url, state, metadata}`（metadata 含 `session_id`/`port`/
        `callback_url`/`expires_at`），宿主登记会话。
     3. 面板轮询 `GET <mgmt>/get-auth-status?state=`，宿主回调 `PollLogin`。
     4. 凭证由宿主 `savePluginLoginRecords` 落库，插件在 poll 里才 `host.auth.save`。
   - `handleLoginPoll` 现在**双路找会话**：先按 `req.State`，找不到再用
     `metadataSessionID(req.Metadata)`（宿主会把 StartLogin 的 metadata 原样回传），
     所以即使宿主改写了 state 也能恢复会话。
   - 新增**未鉴权资源桥**（`/v0/resource/plugins/codearts/...`，宿主只允许 GET、
     且不过管理中间件）：
     - `GET /bootstrap` — 返回宿主端点、provider、当前 pending 会话的 `port`/`session_id`。
     - `GET /post/callback?state=&callback_url=` — 把浏览器观察到的回调喂进会话。
     - `GET /ping` — 探活。
     这三个不泄露任何密钥；`/post/callback` 只改调用方自己那条在途登录会话。
     因为资源面只收 GET，写操作也走 GET + query，并在响应里加 CORS 头。
   - 面板仍保留「提交回调 URL」输入框（服务器部署场景），但它打到插件自己的
     `/post/callback` 桥，**不再打宿主 `/v0/management/oauth-callback`**。
   - `upstream` 包的 `PKCEChallenge(verifier)` 保留（会话只存 verifier）。
   - 测试：`TestBootstrapPublishesHostLoginRoutes`（端点契约 + 断言不碰 oauth-callback）、
     `TestPollRecoversSessionFromHostMetadata`（宿主改写 state 仍能恢复）、
     `TestResourceBridgeFeedsCallback`（未鉴权桥能喂进 code）。
   - 排查要点：`/v0/resource/plugins/...` 的 `Body` 过 RPC 时是 **base64**，
     断言前必须先解码（见 `bootstrap_test.go` 的 `responseBody`）。
   - 注意：`pluginapi.AuthData` 没有 `Logo` 字段，不要再往里塞 logo。

1. **RPC 错误带 HTTP 状态**（`errors.go` + `main.go`）
   - 凭证缺 token / 缺签名密钥 → 403 `auth_unusable`
   - 凭证文档解析失败 → 400 `auth_invalid`
   - token 端点不可用 → 502 `refresh_unavailable`；超时 → 504
   - 宿主 auth store 故障 → 502 `auth_store_unavailable`；账号不存在 → 404
   - `main.go` 的错误分支改用 `rpcErrorEnvelope(err)`，`Retryable` 按状态判断（429/5xx）。
2. **刷新重试**：`refreshWithRetry` 在 35s 内最多尝试 2 次（间隔 300ms，可被测试覆盖），
   上游轮转出的新 `refresh_token` 会写回 `Auth`，不会因一次 STS 抖动丢掉轮转值。
3. **`auth.refresh` 返回 `next_refresh_after`**：宿主优先用它调度刷新，而不是自己解析过期时间。
4. **流式失败会传给宿主**：`host.stream.close` 带 `error` 字段，宿主据此产生终止错误，
   不再把截断流当成功；SSE 错误事件里附带 `status` 与 `code`。
5. **管理 API 全路由鉴权**：GET 也校验（账号列表暴露身份、`/credentials` 返回密钥）；
   `managementHandle` 会剥离 path 上的 `?query`；统一返回 `managementJSON`。
6. **新增管理能力**：`GET /credentials`、`POST /enabled`（启停账号，只改 `disabled` 标记不删凭证）、
   `POST /keepalive` 支持不带 `auth_index` 做全员保活；`/accounts` 增加 `name`、`expires_at`、`expired`、`count`。
7. **面板重写**：每个账号独立卡片（状态徽章、过期时间、积分/签到/保活/启停/导出凭证）、
   全员保活、登录会话状态保持、回调导入的 `next_url` 链接、清晰的中文/英文错误提示（401 提示填管理密钥）。
8. **CI**：`.github/workflows/go.yml` 曾在第五轮精简成“只构建产物”，第八轮按参考项目
   换成 `.github/workflows/build.yml`：push/PR 跑 `go test` + `go vet` + registry 校验，
   再交叉构建 linux/amd64、linux/arm64、darwin/arm64、windows/amd64 并按
   `codearts_<version>_<goos>_<goarch>.zip` 打包；推 `v*` 标签自动发 Release
   （带 `checksums.txt` 与打版本号的 `registry.json`）。
9. **README 重写**：只描述 CPA 插件形态（构建、`plugins.configs.codearts` 配置、安装、面板 OAuth/导入、
   管理 API 表、模型与请求行为、账号维护、目录结构）。

### 测试（`go test -count=1 ./internal` 全绿）

- `core_test.go`：注册/未知方法、凭证归属、刷新保留字段与 metadata、SDK 线路格式与 alias、
  模型发现；新增 `TestRefreshRetriesAndKeepsRotatedToken`、`TestAuthErrorsCarryHostStatus`
  （新增 `rpcStatus` / `envelopeResult` 辅助）。
- `login_test.go`：loopback + 远程导入、开放重定向防护、管理鉴权；新增只读路由 401 覆盖、
  `TestManagementAccountOperations`（accounts / credentials / enabled 的完整读写路径，
  通过替换 `hostAuthList/Get/Save` 变量测试）、`TestOAuthLoopbackDualChannel`
  （真实 socket：307 握手 → secret 采纳 → code 回调 → poll 换凭证 → host.auth.save）、
  `TestLoginTicketChannelWhenCodeNeverArrives`（只靠 ticket 轮询也能完成登录）。
- `executor_test.go`、`transport_test.go`：执行器与宿主 HTTP 桥。

## OAuth 登录链路（排查用）

```text
面板 GET /v0/resource/plugins/codearts/bootstrap
  → 拿到 auth_url=/v0/management/codearts-auth-url、auth_status_url=/v0/management/get-auth-status

「开始授权」GET /v0/management/codearts-auth-url
  → 宿主 ServePluginAuthURL → host.StartLogin("codearts", baseURL)
  → 插件 handleLoginStart：绑定 127.0.0.1:<随机端口>，生成 ticket/secret/PKCE/DPoP
    返回 {url: portal/authorize?...&port=<随机端口>, state, metadata{session_id,port,...}}
  → 宿主 RegisterPluginOAuthSession(state, "codearts", metadata)

portal 第一跳：http://127.0.0.1:<端口>/oauth/callback?secret=...&redirect=portal...
  → 存下 portal 的 secret，307 跳回 redirect（必须，否则链路断）
portal 第二跳：http://127.0.0.1:<端口>/oauth/callback?code=...
  → code 入队

面板循环 GET /v0/management/get-auth-status?state=
  → 宿主 host.PollLogin → 插件 handleLoginPoll（state 找不到就用 metadata session_id）
  → 有 code：PKCE verifier + DPoP 私钥换 STS
  → 无 code：用 portal secret 轮询 /v1/login/ticket（服务器部署靠这条）
  → 宿主 savePluginLoginRecords 落库；插件同时 host.auth.save

浏览器在另一台机器：把整段回调 URL 粘到面板「提交回调 URL」
  → GET /v0/resource/plugins/codearts/post/callback?state=&callback_url=
  → 带 secret+redirect 时回 next_url（继续授权）；带 code 时 status=code_received 并自动 poll
```

排障要点：`redirect_uri` 的 port 必须是插件 loopback 端口；第一跳必须 307；
ticket 轮询只认 portal 下发的 secret（本地生成的上游不认）；**绝不要打宿主的
`/v0/management/oauth-callback`**——TUI 会把 `provider` 写成空串，触发宿主
`NormalizePluginOAuthCallbackProvider("")` → 400 `unsupported provider`（宿主 bug，插件改不了）。
插件侧一切登录入口都走 `codearts-auth-url` + `get-auth-status`，那条链路是好的。


## 面板（`panel.html`）

- 版式对齐 `zidanefaqih/codebuddy-intl-cpa` 的 `workbuddy/panel.html`：CPA 主题变量
  （`data-theme` = light / white / dark，内嵌 iframe 跟随父页）、顶部语言切换、卡片网格、
  toast 提示、模态框（OAuth / 导入凭据 / 凭据查看 / 积分）。
- 管理密钥三种获取方式（对齐参考实现）：iframe 内读 CPA 主面板 `cli-proxy-auth`
  （XOR+base64 解混淆）→ `?key=` 参数（读后从 history 抹掉）→ 手动输入（只存 sessionStorage）。
- **界面语言默认中文，可在右上角切 English**（`codearts-panel-language` 记忆）。
  新增文案时：静态标签加 `data-i18n="中文原文"`，JS 动态文案包 `tr("中文原文")`，
  并在 `PANEL_EN` 补英文；缺项回退显示中文原文。
- 账号卡片：状态徽章（正常 / 已过期 / 已禁用 / 不可用）、uid、到期与更新时间；
  操作：积分 / 签到 / 保活 / 启用-禁用 / 显示凭据。
- 顶部批量操作：全部签到、全部保活、刷新数据（进页面加载一次，之后每 30s 静默刷新）。

## 现有公共代码修改

- `go.mod`：module 改为 `github.com/HITZY2002/codearts2api`；`go 1.26.0` +
  `github.com/router-for-me/CLIProxyAPI/v7 v7.2.30`（SDK 要求 Go ≥ 1.26）
- `go.sum`：必需，已随插件一起提交（CI 构建依赖它）
- `upstream/transport.go`：新增 `Client.SetTransport`
- `upstream/sse.go`：`RawCompletion` 保留 usage，`Aggregate` 输出 usage
- `upstream/statistics.go`：新增 `FetchStatistics` / `ParseStatistics`（套餐与积分口径）
- `upstream/models.go`：`FetchModels` 改收 `upstream.Account` 接口（`AccountID()`/`Credential()`），
  不再依赖插件的 `Auth` 结构
- `VERSION` / `registry.json` / `.github/scripts/package-release.go`：构建注入版本、插件源清单、打包脚本
- `Makefile`：`build` / `build-dev` / `linux` / `darwin` / `windows` / `test` / `lint` / `check` /
  `clean` / `release` / `tag`
- `.gitignore`（根 + `internal/`）：忽略插件产物、`dist/`、`auths/`、本地密钥与编辑器目录
- `README.md`、`.github/workflows/build.yml`：按新结构重写

## 已验证（本轮）

Windows 本机（`go` 通过 toolchain 下载的 1.26.0，cgo 编译器 MinGW-w64 8.1.0）：

```text
gofmt -l .                                                 0 个文件
go build ./...                                             PASS
go vet ./...                                               PASS
go test -count=1 ./...                                     PASS（internal + upstream 两个包）
go build -trimpath -buildmode=c-shared -ldflags "-X main.version=0.1.0" -o codearts.dll ./internal  PASS
  产物内可检出注入的版本串 0.1.0，且 cliproxy_plugin_init 在导出表中
```

Linux（容器 `codearts-go-glibc:latest`，glibc）：

```text
go build -trimpath -buildmode=c-shared -ldflags '-X main.version=0.1.0' -o /tmp/codearts.so ./internal  PASS（10,978,240 B）
nm -D /tmp/codearts.so | grep -c cliproxy                              8 个导出符号
```

## 未完成 / 已知限制

1. **真实宿主端到端未复验（本轮）**
   - 上一轮记录过 CPA 容器 `eceasy/cli-proxy-api:latest`（v8.0.32）成功加载
     `plugin loaded ... plugin_id=codearts`。
   - 本轮重跑容器 smoke 时**观察不到任何插件日志**：同一镜像对 `plugins.enabled: true`、
     `dir` 相对/绝对路径、`/cpa` 与 `/CLIProxyAPI/plugins` 挂载、`config.yaml` 单文件 bind mount
     都没有出现插件相关日志；`/v1/models` 为空、`/v0/resource/plugins/codearts/panel` 404。
     容器日志里连服务初始化日志都缺 plugins 段落，怀疑该镜像 TAG 已更新且入口不再走
     `internal/cmd.StartService`，与 v7.2.30 SDK 行为不一致。
   - 另有两点待查：`Host.BasePath` 是否总是 `/v0`（插件拼的是 `/v0/resource/plugins/codearts/panel`，
     资源前缀实际在 host 里写死）；Windows 下 `host.http.*` 仍被绕过（`runtime.GOOS != "windows"`
     条件），直接走 Go HTTP。
   - 用户本轮选择“先不装客户端，按文档交付”，因此端到端留待有真实 CPA 客户端/凭证的环境。
2. ~~`go.sum` 未提交~~ —— 已在第七轮清理时一并纳入待提交文件。
3. **符号检查的 CI 首次运行仍待观察**：PowerShell 解析逻辑已用真实 DLL 验证，但未在 GitHub
   Runner 上跑过；若 runner 的 PowerShell 版本限制 `[System.IO.File]` 调用，可退回
   `go tool nm codearts.dll | Select-String cliproxy_plugin_init`。
4. ~~旧 standalone 代码未删~~ —— **第七轮已清理**：`cmd/`、`internal/pool`、`internal/scheduler`、
   `internal/server`、`deploy/`、`Dockerfile`、`docker-compose.yml`、`.dockerignore`、
   `config.example.json`、`login.sh`/`apply.sh`/`credit.sh`、`.env.example`、`.vscode/`、
   根目录 `codearts.*` 产物以及 `.ref/` 临时目录全部删除。仓库现在只有插件本体、
   `internal/{auth,upstream}`、`docs/reverse-engineering.md` 与 `plugin/codearts/panel.html`。
   删除后再跑 `go build ./... && go vet ./... && go test ./...` 全绿。
5. **OAuth 生命周期**：每次 `auth.login.start` 绑定 loopback listener + 过期 goroutine（上限 16 个会话，
   10 分钟 TTL），未在 reload/unload 场景复验回收。
6. **没有真实 CodeArts 凭证下的端到端验证**（聊天、tool call、async stream、签到、保活）；
   这些路径只有单测 + 上游实现复用。
7. **用量账本的估算口径**：上游不返回 usage 时是估算值（面板标 `est`），与码道控制台的
   `usage_token_num` 不是同一口径；积分数字来自官方接口，tokens 明细来自本地记账。
   另外账本文件是**明文**（只有 token 计数，没有凭证），位于用户配置目录而不是 CPA 目录。

## 关键代码契约

SDK 路径（注意目录名带 `!`，PowerShell 里需用 `-LiteralPath`）：

```text
D:/go/pkg/mod/github.com/router-for-me/!c!l!i!proxy!a!p!i/v7@v7.2.30/sdk/pluginabi/types.go
```

主要 RPC：`plugin.register`、`plugin.reconfigure`、`model.static`、`model.for_auth`、
`auth.identifier`、`auth.parse`、`auth.login.start/poll`、`auth.refresh`、`executor.execute`、
`executor.execute_stream`、`usage.handle`、`management.register`、`management.handle`；
宿主回调：`host.http.do/do_stream/stream_read/stream_close`、`host.stream.emit/stream.close`、
`host.auth.list/get/save`、`host.log`。

- 注册 metadata 需要非空 `Name`/`Version`/`Author`/`GitHubRepository`，能力至少一个。
- `plugin.register` 的请求体只有 `{config_yaml, schema_version}`，**没有**数据目录；插件数据只能自己找位置。
- `host.stream.close` 的 `error` 字段非空时才会让宿主流以错误结束（`stream_bridge.go`）。
- `host.auth.get` 返回 `{"json": <原始或 base64 文档>}`；`host.auth.save` 接 `{"name","json"}`。
- 宿主优先使用 `auth.refresh` 返回的 `NextRefreshAfter`（`auth_provider.go`）。

## 推荐下一步命令

```bash
git status --short
go build ./... && go vet ./... && go test -count=1 ./...
make check                                                 # lint + test + build + registry 校验
make linux                                                 # 或 darwin / windows，产物 codearts.*
git tag -a v0.1.0 -m "CodeArts CPA v0.1.0" && git push origin v0.1.0   # 触发 CI 发版
```

不要提交 `codearts.so` / `codearts.dll` / `codearts.dylib` / `codearts.h`（已在 `.gitignore`）。

发布约定：`VERSION` 是唯一版本源；CI 从 tag（或 dispatch 输入）解析版本，注入
`-X main.version`，并把同名版本写进 `registry.json` 的 `version` / `release_tag`
（商店只认 `v<version>` 这个 tag 形状）。

## 当前工作区注意事项

第七轮清理后工作区只保留插件相关文件；第七轮之前的历史提交（含 `cmd/`、`internal/server`
等独立服务）仍在 `main` 的分支历史里。若要连历史一起清掉，需另建 orphan 分支或在
GitHub 上重建仓库——**不要**直接对本仓库执行 `git reset --hard` / `git clean -fd`。

# CodeArts CPA

华为云 **CodeArts Agent**（盘古助手 / 码道）的 **CLIProxyAPI (CPA)** 提供方插件，
让任何 OpenAI 兼容客户端都能直接使用 CodeArts 的模型。仓库只做插件形态
（`codearts.so` / `codearts.dylib` / `codearts.dll`），没有独立 HTTP 服务。

插件注册为 CPA 的 `codearts` 提供方，具备以下能力：

| 能力 | 说明 |
|------|------|
| `model_provider` | 静态模型表 + 按账号发现的上游模型 |
| `auth_provider` | OAuth 登录、凭证解析、token 刷新 |
| `executor` | `chat-completions` 协议，支持流式、工具调用、推理内容 |
| `management_api` | 账号管理 API + 内嵌管理面板（菜单 **CodeArts**，中英双语） |

凭证文件、刷新调度、请求转发全部由 CPA 宿主负责；插件只通过宿主回调读写 auth。
唯一的本地状态是用量账本（面板上的「今日 tokens / 用量记录」），默认落在用户配置目录。

## 目录结构

```
internal/                插件主体（package main，c-shared 构建入口）
  main.go                C ABI 导出、RPC 分派、注册 metadata
  auth.go / auth_model.go 凭证解析、刷新、auths/codearts-*.json 读写
  errors.go              错误分类、刷新重试、宿主 auth 回调
  executor.go            chat-completions 执行器（流式 / 工具 / 推理）
  transport.go           宿主 HTTP 桥
  login.go               OAuth PKCE + DPoP 登录
  models.go              模型表与模型发现
  management.go          管理 API、读代理、面板路由
  usage.go / usagehook.go 用量账本与宿主 usage.handle
  assets/panel.html      go:embed 的内嵌面板
  docs/                  包布局与开发说明
upstream/                华为云协议客户端（不依赖 CPA）
.github/workflows/       多平台构建 + 自动发布
scripts/                 仓库自检脚本（registry 校验）
registry.json            插件源清单（商店 / 自建源都读它）
VERSION                  版本号，构建时注入插件 metadata
docs/                    码道客户端协议逆向记录
```

## 构建

要求 Go 1.26+（SDK `github.com/router-for-me/CLIProxyAPI/v7` 要求 Go ≥ 1.26）、cgo 与
系统 C 编译器。

```bash
make build      # 按当前平台产出 codearts.so / codearts.dylib / codearts.dll
make linux      # 产物 codearts.so
make windows    # 产物 codearts.dll（需要 gcc，例如 MinGW-w64 或 zig cc）
make darwin     # 产物 codearts.dylib
make check      # 格式 + vet + 单测 + 构建 + registry 校验
```

等价的手写命令：

```bash
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -ldflags "-X main.version=$(cat VERSION)" -o codearts.so ./internal
```

产物旁会生成同名 `.h` 头文件，两者都不要提交到仓库（已在 `.gitignore` 中忽略）。

## 安装

### 通过插件源（推荐）

CPA 管理面板 → **Config Panel → Advanced → Third-party Plugin Sources**，加上：

```
https://github.com/HITZY2002/codearts2api/releases/latest/download/registry.json
https://raw.githubusercontent.com/HITZY2002/codearts2api/main/registry.json
```

或者写进宿主配置（插件系统默认关闭）：

```yaml
plugins:
  enabled: true
  store-sources:
    - https://raw.githubusercontent.com/HITZY2002/codearts2api/main/registry.json
```

> ⚠️ 通过商店安装时，宿主会接管该插件的版本目录并清理其它同名库文件。
> 如果你自己编译了一份 `.so`，请在另一台宿主/容器里试商店安装。

### 手动安装

```bash
# linux/amd64
unzip codearts_<version>_linux_amd64.zip    # zip 根目录就是 codearts.so
cp codearts.so /path/to/cliproxyapi/plugins/
```

平台分目录布局也可用：`plugins/linux/amd64/codearts.so`。

然后在 `config.yaml` 中启用：

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    codearts:
      enabled: true
```

重启 CPA 后，`GET /v1/models` 应能看到 CodeArts 模型（默认静态列表：
`GLM-5.2`、`glm-5.2-sft-harmony`、`Qwen3-VL-235B`、`deepseek-v4-flash-0731`、
`deepseek-v4-pro-0813`、`glm-5.3-flash`；有账号后按账号上报的目录覆盖）。

在 CPA 管理端插件页可以看到 **CodeArts** 资源菜单，点开就是账号面板。

## 登录账号

登录流程见 origin 项目（`HITZY2002/codearts2api`）的 `cmd/login` + `login.sh`：
华为云 OAuth2 + PKCE + DPoP，双通道取凭证。插件通过宿主的**标准插件登录链路**
（`/v0/management/codearts-auth-url` + `/get-auth-status`）驱动，不碰宿主的
`/v0/management/oauth-callback`。

### 方式一：面板 OAuth 登录

1. 打开 CPA 管理面板 → CodeArts → **OAuth 登录** → **开始授权**。
2. 新窗口里用华为云账号登录。插件在本机监听 `127.0.0.1:<随机端口>` 接回调：
   - 第一跳带 `secret` + `redirect` → 插件记下 portal 下发的 secret 并 307 跳回授权页；
   - 第二跳带 `code` → 换取 STS 临时 AK/SK 并写入 CPA auth 目录
     （文件名 `codearts-<user_id>.json`）。
3. 面板自动轮询登录状态，成功后列表刷新出新账号。若没有自动刷新，点 **检查登录状态**。

**服务器部署（浏览器在另一台机器）**：把授权完成后地址栏里的整段回调 URL 粘到
**提交回调 URL**。第一次多半返回「继续授权」链接（portal 的 secret+redirect 那一跳），
打开它继续授权；第二次带上 `code` 后插件会自动完成登录。
这条路径不依赖浏览器能访问回调端口，因为插件同时用 portal 的 secret 轮询
`/v1/login/ticket` 拿凭证。

#### Docker 部署（必读）

容器里的 `127.0.0.1` 与宿主机的 `127.0.0.1` 不是同一个网络命名空间，随机回调端口也没有
通过 `-p` 发布，因此**浏览器回调通道在容器中不可用**——portal 跳到
`http://127.0.0.1/oauth/callback` 时本来就打不开。

插件对此已经适配：

- 登录监听改为绑定 `0.0.0.0`（仍只响应插件自己的回调路径）；
- `/bootstrap` 返回 `ticket_only: true`，面板据此**不再注入无用的回调 iframe**，
  并在授权弹窗里提示“容器部署，自动用 ticket 轮询”；
- 登录完成完全由 **ticket 轮询**驱动，不依赖浏览器能否连上容器端口。

使用时只要：点「开始授权」→ 华为云登录 → **保持面板打开**，插件会自动取回凭证。

如果确实想让浏览器回调也生效，需要把 loopback 端口发布出来并告诉插件真实主机名：

```bash
# 1) 发布一段端口范围（选一个不冲突的区间）
docker run -p 18317:8317 -p 40000-40100:40000-40100 ...

# 2) 让插件用它生成 redirect_uri（必须是浏览器可达的地址）
docker run -e CODEARTS_CALLBACK_HOST=172.20.0.2 ...
```

> 注意：`CODEARTS_CALLBACK_HOST` 只影响插件自己生成的 `redirect_uri` 与回调提示；
> portal 侧最终怎么跳仍由华为云决定，所以多数情况下**留空、直接用 ticket 轮询**更省事。

> 说明：宿主自带的「回调 URL」输入框由 CPA 的 TUI 实现，它把 `provider` 写成空串，
> 宿主会回 `400 unsupported provider`，所以那个框对本插件不可用（宿主已知问题）。
> 请始终用插件面板里的入口；该入口走的是宿主正常工作的插件登录路由。

### 方式二：导入已有凭证

面板 **导入凭据** 里粘贴一份 `codearts-*.json` 文档即可。字段：

```json
{
  "user_id": "...",
  "user_name": "...",
  "domain_id": "...",
  "cloud_dragon_token": "...",
  "access_key_id": "...",
  "secret_access_key": "...",
  "expiration": "2030-01-01T00:00:00Z",
  "refresh_token": "...",
  "code_verifier": "...",
  "client_id": "codearts-agent",
  "dpop_private_key": { "kty": "EC", "...": "..." }
}
```

`refresh_token` 与登录时的 `client_id`、DPoP 私钥绑定，三者必须一起迁移，否则刷新会被
STS 拒绝。插件刷新后会写回轮转后的新 `refresh_token`，并保留文件中的未知字段与
`metadata`。

## 管理 API

所有路由都在 `/v0/management/plugins/codearts` 下，**读写都需要鉴权**：带上
`Authorization: Bearer <management key>`。宿主若已配置管理密钥，插件会用同一个密钥
校验；也可以用环境变量 `CODEARTS_MANAGEMENT_KEY` 单独指定。未配置任何密钥时由宿主
管理中间件负责鉴权。

| 方法与路径 | 作用 |
|-----------|------|
| `GET /accounts` | 账号列表（uid、昵称、过期时间、是否禁用） |
| `GET /credentials?auth_index=` | 导出单个账号的凭证文档（含密钥，谨慎使用） |
| `POST /enabled` | `{"auth_index":"...","enabled":true|false}` 启用/禁用账号 |
| `GET /credits?auth_index=` | 查询积分活动（可省略 `auth_index` 查全部） |
| `GET /stats?auth_index=` | 套餐 / 积分 / 周期 token 统计（码道控制台同源接口） |
| `GET /usage?auth_index=` | 本机用量账本：今日、按天、按模型、最近明细 |
| `POST /checkin` | 领取每日签到积分，body 可带 `auth_index` |
| `POST /keepalive` | 刷新 token；body 可带 `auth_index`，省略则全部账号 |
| `POST /import` | `{"json":{...}}` 导入凭证 |
| `POST /oauth/start` | 发起登录，返回 `url` 与 `state`（等价于宿主的 `codearts-auth-url`） |
| `POST /oauth/poll` | `{"session_id":"<state>"}` 查询登录状态（等价于 `get-auth-status`） |
| `POST /oauth/import-callback` | `{"session_id":"...","callback_url":"..."}` 远程回调 |

未鉴权资源路由（`/v0/resource/plugins/codearts/...`，宿主只允许 GET）：

| 路径 | 作用 |
|------|------|
| `GET /bootstrap` | 返回宿主端点（`auth_url`、`auth_status_url`）、provider、在途登录会话的 `port`/`session_id` |
| `GET /call?op=accounts\|credits\|stats\|usage&auth_index=` | 面板用的免密钥只读代理（不含任何密钥） |
| `GET /post/callback?state=&callback_url=` | 把浏览器观察到的回调喂进登录会话（`/oauth/import-callback` 的免密钥等价物） |
| `GET /ping` | 探活 |

面板本身在 `/v0/resource/plugins/codearts/panel`（资源路由，由宿主渲染进管理控制台）。

> **不要用 CPA 自带的「回调 URL」输入框提交本插件的回调**。那个框由宿主 TUI 实现，
> 它把 `provider` 写成空串，宿主会回 `400 unsupported provider`（宿主已知问题）。
> 请用插件面板里的登录入口，或调用上面的 `/post/callback`。

### 面板说明

版式对齐 `zidanefaqih/codebuddy-intl-cpa` 的 CodeBuddy 面板：跟随 CPA 主题
（light / white / dark）、顶部语言切换、账号卡片网格、toast 提示、模态框。

- **界面语言默认中文**，右上角可切 English（选择记在 `localStorage`）。
- 从 CPA 主面板内嵌打开时自动取用管理密钥，无需手填；也可用 `?key=<management key>` 打开。
- 账号卡片显示状态徽章（正常 / 已过期 / 已禁用 / 不可用）、uid、到期与更新时间，
  操作按钮：积分、签到、保活、启用-禁用、显示凭据。
- 顶部批量操作：全部签到、全部保活、刷新数据；进入页面自动加载一次，之后每 30 秒静默刷新。
- **控制台概览**（仿码道控制台，多账号时用「积分用量」右上角的下拉切换账号）：
  - **积分用量**：环形扇形图显示已用百分比，下方给出总积分 / 已用 / 剩余、各积分包用量条、
    统计周期（积分包有效期）与本期 token。数据来自码道官方统计接口 `statistics/plugin`。
  - **今日 tokens 消耗**：今日总 tokens / 请求数 / 输入 / 输出，加近 14 天柱状图与模型 Top5。
  - **用量记录**：可切「明细」（时间、模型、输入、输出、总 tokens、耗时、状态）与「按天」。
  - 这两块的 token 数由插件本地记账（只有经过本 CPA 的请求才算）；上游没返回 usage 时
    按字符数估算，明细里会标 **估算**。账本保存在 `<用户配置目录>/codearts2api/usage-ledger.json`，
    可用环境变量 `CODEARTS_USAGE_DIR` 改位置。

```bash
KEY=your-management-key
curl -s http://127.0.0.1:8317/v0/management/plugins/codearts/accounts \
  -H "Authorization: Bearer $KEY"
curl -s -X POST http://127.0.0.1:8317/v0/management/plugins/codearts/keepalive \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{}'
```

## 模型与请求

- 端点：沿用 CPA 的 `/v1/chat/completions`、`/v1/messages` 等；插件声明
  `chat-completions` 输入/输出格式，其他协议由宿主转换。
- 模型 ID 区分大小写（`GLM-5.2`、`Qwen3-VL-235B`）。可以用 CPA 的
  `oauth-model-alias` 配置给模型起别名，插件会按别名反查真实模型名。
- 工具调用：支持 `tool_choice` 的 `auto` / `none` / `required` 及指定函数；指定函数时
  只向上游发送该函数定义，函数不存在或重复定义返回 400。
- 推理内容：上游 `delta.reasoning_content` 会原样透传。
- 流式：`stream: true` 时由插件逐块 `emit` 给宿主；上游中断会补一条 SSE 错误事件并以
  错误结束该流，避免客户端把截断的流当成正常结束。
- 图片/多模态：`content` 数组中的 `text` 段会被拼接为文本发送。

## 账号维护

- **自动刷新**：CPA 依据 auth 的过期时间与插件返回的 `next_refresh_after` 调度刷新；
  插件刷新失败返回 502/504（宿主会重试），凭证不可用返回 403（宿主可禁用/要求重登）。
- **签到**：面板「签到」或 `POST /checkin`。领取后需确认，重复点击是幂等的。
- **保活**：面板「全部保活」或 `POST /keepalive`，等价于立即刷新一次 token。
- **禁用**：面板「禁用」或 `POST /enabled`。禁用只写 auth 的 `disabled` 标记，
  不删除凭证，可随时启用。

## 开发与测试

```bash
go test ./...                                     # 全量单测
go vet ./...
make lint                                         # gofmt + vet（有 staticcheck 就跑）
CGO_ENABLED=1 go build -buildmode=c-shared -o /tmp/codearts.so ./internal
```

`make check` = `lint + test + build + registry 校验`；`make clean` 删除本地产物。

CI（`.github/workflows/build.yml`）：

- push / PR → 跑 `go test`、`go vet`、`registry.json` 校验，并交叉构建
  linux/amd64、linux/arm64、darwin/arm64、windows/amd64，产物按
  `codearts_<version>_<goos>_<goarch>.zip` 打包上传。
- 推送 `v*` 标签（或手动触发并填版本）→ 自动创建 GitHub Release，
  附带各平台 zip、`checksums.txt` 和打上版本号的 `registry.json`。

## 免责声明

仅供学习和研究使用。使用者需遵守华为云服务条款，自行承担使用风险。

## License

MIT

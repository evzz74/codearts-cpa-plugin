# 包布局

## 决定

CPA 插件是 `-buildmode=c-shared` 产物，要求导出的 C ABI 入口和 `main` 包在一起。
本仓库按“插件代码一份 `main` 包”来切分，不再为了复用把插件拆成多个内部包：

| 目录 | 内容 |
|------|------|
| `internal/` | 插件主体，`package main`，同包多文件 + 一个子目录 |
| `internal/assets/panel.html` | `go:embed` 内嵌的管理面板 |
| `upstream/` | 华为云协议客户端（登录 / 聊天 / SSE / 模型 / 签到 / 套餐统计），无 CPA 依赖 |

## `internal/` 的任务划分

| 文件 | 职责 |
|------|------|
| `main.go` | C ABI 导出、RPC 分派、注册 metadata 与能力声明 |
| `auth.go` | 凭证归属判断、`auth.parse`、`auth.refresh` |
| `auth_model.go` | `Auth` 结构、凭证文件读写（`auths/codearts-*.json`） |
| `errors.go` | 错误分类 → HTTP 状态、刷新重试、宿主 auth 回调 |
| `executor.go` | `chat-completions` 执行器：tools、`tool_choice`、reasoning、流式 |
| `transport.go` | 宿主 HTTP 桥（`host.http.*`）、流式 emit/close |
| `login.go` | OAuth PKCE + DPoP、loopback 回调、远程粘贴回调、ticket 轮询 |
| `models.go` | 静态模型表、按账号模型发现、类型映射到宿主 `pluginapi.ModelInfo` |
| `management.go` | 管理 API 注册/分发、鉴权、账号操作、面板路由 |
| `usage.go` | 用量账本：记账、按天/模型汇总、落盘与裁剪 |
| `usagehook.go` | 宿主 `usage.handle` 回调入口 |
| `*_test.go` | 单测；`panel_test.go` 直接校验内嵌面板的 DOM 契约与 i18n |

## 为什么 CLI 侧只有 `upstream/` 一个子包

`upstream` 是纯协议层：不含 CPA 类型、不认识 `Auth`，模型发现只依赖
`upstream.Account` 这个两方法接口（`AccountID()` / `Credential()`）。
这样插件包可以自由改动自己的凭证结构，而协议层不需要跟着改。

## 不做的事

- 不引入 `internal/xxx` 多包拆分：c-shared 的 `main` 包无法被别的包引用，
  拆开只会产生一堆转发函数。
- 不把面板写成前端工程：面板必须是单文件 HTML，随插件二进制一起分发。

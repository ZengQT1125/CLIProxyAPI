# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

AI 工作指南。**必读 Fork-Specific 部分**——合并上游或重构时直接决定取舍。

## Identity

- Module: `github.com/router-for-me/CLIProxyAPI/v7`
- Go 1.26.0
- 上游 `router-for-me/CLIProxyAPI`（远程 `upstream`），当前基于 v7.2.73
- Fork 增量: SF routing · usage 持久化 · 管理中心 · TUI · 管理面板自动更新
- Fork tag 命名: `fork/v*`（如 `fork/v8.20.1`），与上游 `v7.x.x` tag 共存

## Hard Constraints

- `internal/translator/` 是 PR 受保护路径——上游 CI 自动拒绝该目录的 PR。修改需走 issue。
- `fork/v*` tag 触发 docker-image 工作流，构建多架构镜像推送到 `ghcr.io`（非 DockerHub）。
- 构建参数: `CGO_ENABLED=0`，静态链接，`-trimpath`，版本通过 ldflags 注入。
- 合并上游前必读「Fork-Specific Modifications」清单；任何 fork-only 文件被删/覆盖即为合并错误。

## Architecture

### 请求处理流

```
Client → Gin HTTP Server (internal/api/server.go)
       → Middleware 栈 (logging → compression → CORS → heartbeat → safe-mode)
       → Route Handler (chat/responses/images/videos/management)
       → Translator (internal/translator/ — 请求/响应跨格式转换)
       → Auth Selector (sdk/cliproxy/auth/conductor.go — 凭证编排)
       → Provider Executor (internal/runtime/executor/ — 实际 API 调用)
       → Response streaming back to client
```

### 服务构建与生命周期

`cmd/server/main.go` → `sdk/cliproxy/Builder` (fluent builder 模式) → `Service`

Builder 注入核心组件: TokenStore、AuthManager、AccessManager、PluginHost、Hooks。
`Build()` 根据 `routing` 配置创建对应的 Selector（RoundRobin/FillFirst/SequentialFill），
可选包装 SessionAffinitySelector。

### Token Store 后端优先级

启动时按优先级检测: PostgreSQL (`PGSTORE_*`) > Object Store (`OBJECTSTORE_*`) > Git Store (`GITSTORE_*`) > 本地文件。
Store 接口统一 `List/Save/Delete`，auth 文件和 config 均可持久化到远程后端。

### 凭证选择与冷却

`auth/conductor.go` 编排多级 provider 轮转。每个 provider 内由 Selector 选择凭证。
凭证触发配额限制后进入冷却状态（`cooldown_state.go`），冷却期间自动跳过。
`save-cooldown-status` 配置项控制冷却状态持久化（支持文件 / PG 后端），重启后恢复。

### Executor 模式

每个 provider 一个 executor 文件，均实现 `ProviderExecutor` 接口。
Executor 负责：构造上游请求、处理认证、流式/非流式转发、错误处理与重试。
`helps/` 子目录提供共享工具（uTLS 客户端、HTTP 传输、重试逻辑）。

### 热重载

`internal/watcher/` 通过 `fsnotify` 监控 config 和 auth 文件变更，变更后自动重载配置和凭证，无需重启。

## Module Map

| 路径 | 职责 |
|---|---|
| `cmd/server/main.go` | 入口；CLI flags（含 `-tui`）；token store 初始化；`internal/cmd.StartService` / `StartServiceBackground` |
| `sdk/cliproxy/` | 可嵌入 SDK：`service.go` `builder.go` `auth/` `executor/`（含 `Pinned/Selected/ExecutionSessionMetadataKey`） |
| `internal/api/` | Gin HTTP server；`handlers/management/` |
| `internal/runtime/executor/` | Provider executor: Gemini / Gemini-CLI / Gemini-Vertex / Claude / Codex / Codex-WebSocket / Antigravity / Kimi / AIStudio / OpenAI-compat / xAI |
| `internal/translator/` | 请求/响应跨格式转换（受 PR 守卫保护） |
| `internal/thinking/provider/` | 扩展思考/推理 |
| `internal/watcher/` | config & auth 文件热重载 |
| `internal/auth/` | 每 provider OAuth；`xai/` 使用 PKCE |
| `internal/home/` | Home 控制平面：Redis 集群协调、TLS、cluster nodes 解析、Pub/Sub |
| `internal/usage/` | **fork**：SQLite usage 日志、监控查询、PG 镜像 |
| `internal/registry/` | 动态模型注册表 + `models/codex_client_models.json` |
| `internal/tui/` | bubbletea TUI（dashboard/auth/config/keys/logs/oauth/usage） |
| `internal/managementasset/` | **fork**：管理面板 HTML 自动下载更新（SHA256 校验、原子写） |

## Key Interfaces

```go
// sdk/cliproxy/executor
type ProviderExecutor interface {
    Identifier() string
    Execute(ctx, auth, req, opts) (Response, error)
    ExecuteStream(ctx, auth, req, opts) (<-chan StreamChunk, error)
    Refresh(ctx, auth) (*Auth, error)
}

// sdk/cliproxy/auth
type Selector interface {
    Select(ctx, auths []*Auth, model string, opts SelectOptions) (*Auth, error)
}
type ExecutionSessionCloser interface {
    CloseExecutionSession(sessionID string)
}
```

## Commands

```bash
# 构建
go build -o cli-proxy-api ./cmd/server/
docker build -t cliproxyapi .

# 测试
go test ./...                                              # 全部测试
go test ./sdk/cliproxy/auth/                               # 单包
go test ./sdk/cliproxy/auth/ -run TestSequentialFill       # 匹配名称
go test ./sdk/cliproxy/auth/ -run TestNormalize/empty      # 单子测试
go test -race ./...                                        # race 检测
go test -count=1 ./internal/usage/                         # 禁用缓存

# 运行
./cli-proxy-api [-config path] [-tui [-standalone]] [-vertex-import key.json]
# OAuth: -login -codex-login -codex-device-login -claude-login
#        -antigravity-login -kimi-login -xai-login
# Login flags: -oauth-callback-port PORT, -no-browser
```

## Testing Conventions

- **纯标准库 `testing`**——无 testify、gomock 等第三方框架。断言用 `t.Fatalf`/`t.Errorf`。
- **Table-driven tests** + `t.Run` 子测试为主流模式，子测试通常标记 `t.Parallel()`。
- **手写 stub/fake**——手动实现接口或使用函数类型适配器（类似 `http.HandlerFunc`），定义在同包 `_test.go` 中。
- **隔离**: `t.TempDir()` 临时路径、`t.Setenv()` 环境变量、`t.Cleanup()` 清理回调。
- **集成测试**: `test/` 目录，`package test`，使用 `httptest.NewServer` 构建真实依赖。
- 新增测试必须匹配上述风格——不要引入第三方断言库。

## Config

- `config.yaml`（template: `config.example.yaml`）；`auths/*.json`；`.env`
- 主要 sections: `tls` `remote-management` `routing` `proxy-url` `request-retry` `quota-exceeded` `payload` `oauth-model-alias` `oauth-excluded-models` `ws-auth` `usage-persistence-enabled` `delete-unauthorized-auth` `claude-header-defaults` `cloak` `passthrough-headers` `streaming` `home` `save-cooldown-status`
- Token store env: 默认本地 file；`PGSTORE_*` / `GITSTORE_*` / `OBJECTSTORE_*`
- `MANAGEMENT_STATIC_PATH`: 覆盖管理面板 HTML 目录（默认临时目录）
- Routing 策略: `round-robin`（default）· `fill-first`/`ff` · `sequential-fill`/`sf`（**fork**）

## API Routes

| Route | 说明 |
|---|---|
| `/v1/*` | OpenAI 兼容（chat、models） |
| `/v1/images/generations`, `/v1/images/edits` | xAI Grok 等图像 |
| `/v1/videos`, `/v1/videos/{generations,edits,extensions,:request_id}` | xAI Grok 视频 |
| `/v1/responses` POST/GET | Responses API + WS 升级 |
| `/v0/management/*` | 管理 API；`auth-files/*` CRUD；`plugins/:id` DELETE |
| `/v0/management/custom/monitor/*` | **fork** 11 个监控端点 |
| `/v0/management/custom/codex-cleanup` | **fork** Codex 认证清理 |
| `/xai/callback`, `/v1/ws` | xAI OAuth 回调 / WS |

## CI/CD Workflows

| Workflow | 触发 | 用途 |
|---|---|---|
| `release.yaml` | `fork/v*` tag · `workflow_dispatch` | 5 平台交叉编译（darwin/linux/windows × amd64/arm64），Conventional Commits release notes |
| `docker-image.yml` | `fork/v*` tag · `workflow_dispatch` | 并行构建 linux/amd64 + linux/arm64，推送 `ghcr.io`，GHA 缓存 |
| `pr-path-guard.yml` | PR | 保护 `internal/translator/` 目录 |

---

## Fork-Specific Modifications (Upstream Merge Protection)

> **合并上游时必须保护下列文件**。Upstream 不包含这些变更，被覆盖/删除 = 合并错误。

### 1. Usage Persistence & SQLite Store

Fork-only:
- `internal/usage/store.go`（+`_test.go`）— SQLite usage log persistence with auto-cleanup
- `internal/usage/database_plugin.go`（+`_test.go`）— PG log mirroring to local SQLite
- `internal/usage/monitor_queries.go`（+`_test.go`）— dashboard 聚合查询 API
- `internal/usage/logger_plugin.go`（+`_test.go`）— usage 统计 logger plugin

Modified:
- `internal/config/config.go` — `usage-persistence-enabled` 字段
- `sdk/cliproxy/builder.go` — usage persistence builder option
- `sdk/cliproxy/service.go`（+`service_usage_persistence_test.go`）— 生命周期 init/close

### 2. Management Center Monitor & Tools

Fork-only:
- `internal/api/handlers/management/monitor.go`（+`_test.go`）— 11 monitor 端点（含 service-health、key-stats、request-details）

Modified:
- `internal/api/handlers/management/api_tools.go` — `CleanupCodexAuth` 端点
- `internal/api/handlers/management/usage.go` — 增强 usage export/import
- `internal/api/server.go` — 注册 `/custom/monitor/*`（11 端点）和 `/custom/codex-cleanup`

### 3. Sequential-Fill (SF) Selector

Fork-only:
- `sdk/cliproxy/auth/routing_strategy.go`（+`_test.go`）— SF 策略常量与 `NormalizeRoutingStrategy`

Modified（文件上游已存在，SF 代码为 fork 新增）:
- `sdk/cliproxy/auth/selector.go`（+`_test.go`）— `SequentialFillSelector`（sticky 行为）
- `sdk/cliproxy/auth/conductor.go` — SF 集成，two-level provider rotation（SF 遵循配置的 `request-retry`/`max-retry-credentials`，无 selector 级 retry override）

### 4. Antigravity Enhancements

> Web Search 已由上游 v7.1.74（PR #3824）原生实现，fork 简易实现已删除。

Modified:
- `internal/runtime/executor/antigravity_executor.go` — 稳定 fallback project ID、404 base URL fallback、降低 refresh skew、404 `disable-cooling`
- `internal/translator/antigravity/openai/chat-completions/antigravity_openai_request.go` — 支持 OpenAI `type: "web_search"` 工具（上游仅支持 `google_search`）

### 5. Request Handler Fixes & Codex Enhancements

Fork-only:
- `cmd/server/config_bootstrap.go`（+`_test.go`）— Git-backed config bootstrap
- `cmd/server/local_model.go`（+`_test.go`）— 本地模型目录支持
- `internal/codexapi/base_url.go` — Codex API base URL 解析
- `internal/runtime/executor/codex_base_url.go`（+`_test.go`）— Codex executor base URL 解析
- `internal/runtime/executor/responses_encrypted_retry.go`（+`_test.go`）— Responses API 加密重试

Modified:
- `internal/runtime/executor/helps/utls_client.go`（+`_test.go`）— **fork** uTLS protected host 自动降级：`uTLS HTTP/2 -> uTLS HTTP/1.1 -> standard HTTP/1.1`；只在没有拿到 HTTP response 的 transport error 时降级，任何 HTTP status（含 401/403/429/5xx）都直接返回；POST 降级必须通过 `GetBody` 重放请求体。
  - 合并保护：不要恢复 `CLIPROXY_CODEX_TRANSPORT` / `codexTransportMode` 手动开关。
  - 合并保护：不要重新套用上游/PR #4012 的"所有 fallback upstream 强制 HTTP/1.1"。普通非 protected host 必须保留默认 HTTP/2 能力；只有 protected host 的第三段 `protectedFallbackHTTP11` 使用 standard HTTP/1.1。
  - **fork** h2 降级内存缓存（`utlsH2DegradeCache`）：protected host 的 h2 发生 response-less transport error 后，按 `scope+host`（scope=`direct`/`proxy:<redacted>` 脱敏）进程内标记跳过 h2，初始 2m、指数退避封顶 30m；h2 拿到任何 `*http.Response`（含 4xx/5xx）立即重置；scope 为空（注入式 ctx transport）或非 protected host 不进缓存。无后台 goroutine、无持久化、无配置开关。
  - 合并保护：`roundTripProtected` 用 `sentAttempts`（实际发送数）而非循环下标传给 `requestForProtectedAttempt`——跳过 h2 时 HTTP/1.1 为首次发送、无需 `GetBody`，合并时不要回退成下标。
- `sdk/api/handlers/handlers.go`（+`handlers_request_details_test.go`）— Codex chat completions 暴露 cached tokens；mixed search/function tool calls 路由到兼容 provider

### 6. Utility Additions

- `internal/util/util.go` — helper 函数

### 7. CI/CD & Build

Modified:
- `Dockerfile` — native `GOARCH` 交叉编译（弃用 QEMU）
- `.github/workflows/docker-image.yml` — DockerHub→GHCR，matrix，GHA 缓存，`fork/v*` 触发，`workflow_dispatch`
- `.github/workflows/release.yaml` — 改为 `fork/v*` 触发 + `workflow_dispatch`，构建 release artifacts

Fork deletions（删除上游文件）:
- `.github/workflows/pr-test-build.yml`
- `.github/workflows/auto-retarget-main-pr-to-dev.yml`

Other:
- `.gitignore` — 忽略 `usage.*`
- `go.mod`/`go.sum` — `golang.org/x/exp`、`charmbracelet/{bubbletea,bubbles,lipgloss}`

### 8. Documentation

- `README.md`/`README_CN.md` — Fork Features + Management Center 描述

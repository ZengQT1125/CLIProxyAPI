# CLAUDE.md

AI 工作指南。**必读 Fork-Specific 部分**——合并上游或重构时直接决定取舍。

## Identity

- Module: `github.com/router-for-me/CLIProxyAPI/v7`
- Go 1.26.0
- 上游 `router-for-me/CLIProxyAPI`（远程 `upstream`），当前基于 v7.2.28
- Fork 增量: SF routing · usage 持久化 · 管理中心 · TUI · 管理面板自动更新

## Hard Constraints

- `internal/translator/` 是 PR 受保护路径——上游 CI 自动拒绝该目录的 PR。修改需走 issue。
- `fork/v*` tag 触发 docker-image 工作流，构建多架构镜像推送到 `ghcr.io`（非 DockerHub）。
- 构建参数: `CGO_ENABLED=0`，静态链接，`-trimpath`，版本通过 ldflags 注入。
- 合并上游前必读「Fork-Specific Modifications」清单；任何 fork-only 文件被删/覆盖即为合并错误。

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
go build -o cli-proxy-api ./cmd/server/
go test ./...
docker build -t cliproxyapi .

./cli-proxy-api [-config path] [-tui [-standalone]] [-vertex-import key.json]
# OAuth: -login -codex-login -codex-device-login -claude-login
#        -antigravity-login -kimi-login -xai-login
# Login flags: -oauth-callback-port PORT, -no-browser
```

## Config

- `config.yaml`（template: `config.example.yaml`）；`auths/*.json`；`.env`
- 主要 sections: `tls` `remote-management` `routing` `proxy-url` `request-retry` `quota-exceeded` `payload` `oauth-model-alias` `oauth-excluded-models` `ws-auth` `usage-persistence-enabled` `delete-unauthorized-auth` `claude-header-defaults` `cloak` `passthrough-headers` `streaming` `home`
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

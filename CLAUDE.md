# CLAUDE.md

AI 工作指南。优先级：**Fork-Specific 部分必须读完**——合并上游或重构时直接决定取舍。

## Identity

- Module: `github.com/router-for-me/CLIProxyAPI/v7`
- Go: 1.26.0
- 上游: `router-for-me/CLIProxyAPI` (远程 `upstream`)
- Fork 增量: Antigravity web search · SF routing · usage 持久化 · 管理中心 · TUI · 管理面板自动更新

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
| `internal/api/` | Gin HTTP server；`handlers/management/`；`modules/amp/` |
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

- `config.yaml` (template: `config.example.yaml`)；`auths/*.json`；`.env`
- 主要 sections: `tls` `remote-management` `routing` `proxy-url` `request-retry` `quota-exceeded` `payload` `oauth-model-alias` `oauth-excluded-models` `ampcode` `ws-auth` `usage-persistence-enabled` `delete-unauthorized-auth` `claude-header-defaults` `cloak` `passthrough-headers` `streaming` `home`
- Token store env: 默认本地 file；`PGSTORE_*` / `GITSTORE_*` / `OBJECTSTORE_*`
- `MANAGEMENT_STATIC_PATH`: 覆盖管理面板 HTML 目录（默认临时目录）
- Routing 策略: `round-robin` (default) · `fill-first`/`ff` · `sequential-fill`/`sf` (**fork**)

## API Routes

| Route | 说明 |
|---|---|
| `/v1/*` | OpenAI 兼容（chat、models） |
| `/v1/images/generations`, `/v1/images/edits` | xAI Grok 等图像 |
| `/v1/videos`, `/v1/videos/{generations,edits,extensions,:request_id}` | xAI Grok 视频 |
| `/v1/responses` POST/GET | Responses API + WS 升级 |
| `/v0/management/*` | 管理 API；`auth-files/*` CRUD |
| `/v0/management/custom/monitor/*` | **fork** 10 个监控端点 |
| `/v0/management/custom/codex-cleanup` | **fork** Codex 认证清理 |
| `/xai/callback`, `/v1/ws`, `/api/provider/{provider}/v1/*` | xAI OAuth 回调 / WS / Amp |

---

## Fork-Specific Modifications (Upstream Merge Protection)

> **合并上游时必须保护下列文件**。Upstream 不会包含这些变更，被覆盖/删除 = 合并错误。

### 1. Usage Persistence & SQLite Store

Fork-only files (新增):
- `internal/usage/store.go` — SQLite usage log persistence with auto-cleanup
- `internal/usage/store_test.go`
- `internal/usage/database_plugin.go` — PG log mirroring to local SQLite
- `internal/usage/database_plugin_test.go`
- `internal/usage/monitor_queries.go` — Aggregation query APIs for dashboard
- `internal/usage/monitor_queries_test.go`

Fork modifications (修改):
- `internal/config/config.go` — Added `usage-persistence-enabled` config field
- `sdk/cliproxy/builder.go` — Added usage persistence builder option
- `sdk/cliproxy/service.go` — Usage persistence lifecycle (init/close)
- `sdk/cliproxy/service_usage_persistence_test.go`

### 2. Management Center Monitor & Tools

Fork-only files (新增):
- `internal/api/handlers/management/monitor.go` — 10 monitor dashboard endpoints (including service-health, key-stats)
- `internal/api/handlers/management/monitor_test.go`

Fork modifications (修改):
- `internal/api/handlers/management/api_tools.go` — Added `CleanupCodexAuth` endpoint
- `internal/api/handlers/management/usage.go` — Enhanced usage export/import
- `internal/api/server.go` — Added `/custom/monitor/*` (10 endpoints) and `/custom/codex-cleanup` route registrations

### 3. Sequential-Fill (SF) Selector

Fork-only files (新增):
- `sdk/cliproxy/auth/selector.go` — `SequentialFillSelector` implementation with sticky behavior
- Parts of `sdk/cliproxy/auth/selector_test.go` — SF-specific tests

Fork modifications (修改):
- `sdk/cliproxy/auth/conductor.go` — SF integration, two-level provider rotation (SF honors configured `request-retry`/`max-retry-credentials`, no selector-level retry override)

### 4. Antigravity Web Search & Fixes

Fork modifications (修改):
- `internal/runtime/executor/antigravity_executor.go` — Web search via Gemini, stable fallback project ID, 404 base URL fallback, reduced refresh skew, `disable-cooling` for 404
- `internal/translator/antigravity/claude/antigravity_claude_request.go` — Search grounding in Claude format
- `internal/translator/antigravity/claude/antigravity_claude_response.go` — Search result translation
- `internal/translator/antigravity/gemini/antigravity_gemini_request.go` — Search grounding in Gemini format
- `internal/translator/antigravity/openai/chat-completions/antigravity_openai_request.go` — Search grounding in OpenAI format
- `internal/translator/antigravity/openai/chat-completions/antigravity_openai_request_test.go`
- `internal/translator/antigravity/openai/responses/antigravity_openai-responses_request.go` — Search grounding in Responses format

### 5. Request Handler Fixes

Fork modifications (修改):
- `sdk/api/handlers/handlers.go` — Expose cached tokens in Codex chat completions; route mixed search/function tool calls to compatible providers
- `sdk/api/handlers/handlers_request_details_test.go`

### 6. Utility Additions

Fork modifications (修改):
- `internal/util/util.go` — Added helper functions

### 7. CI/CD & Build

Fork modifications (修改):
- `Dockerfile` — Cross-compilation (native `GOARCH`) instead of QEMU emulation
- `.github/workflows/docker-image.yml` — Complete rewrite: DockerHub→GHCR, matrix strategy, GHA caching, `fork/v*` tag trigger, `workflow_dispatch` support
- `.github/workflows/release.yaml` — Rewritten as `Build and Release`: triggers on `fork/v*` tags (not all tags), adds `workflow_dispatch` with tag input, builds release artifacts alongside `docker-image.yml`

Fork deletions (删除上游文件):
- `.github/workflows/pr-test-build.yml` — Removed (upstream PR build check, not needed for fork)
- `.github/workflows/auto-retarget-main-pr-to-dev.yml` — Removed (upstream PR retargeting bot; fork uses single-branch flow)

Other:
- `.gitignore` — Added `usage.*` to ignore usage database files
- `go.mod` / `go.sum` — Added `golang.org/x/exp`, `charmbracelet/bubbletea`, `charmbracelet/bubbles`, `charmbracelet/lipgloss` dependencies

### 8. Management Panel Auto-Updater

Fork-only files (新增):
- `internal/managementasset/updater.go` — Auto-downloads management panel HTML from GitHub releases, SHA256 verification, atomic writes, fallback static page

Fork modifications (修改):
- `cmd/server/main.go` — Integrates `managementasset.StartAutoUpdater()` on startup
- `internal/api/server.go` — Serves management panel from `MANAGEMENT_STATIC_PATH` directory

### 9. Documentation

Fork modifications (修改):
- `README.md` / `README_CN.md` — Added Fork Features section, Management Center description

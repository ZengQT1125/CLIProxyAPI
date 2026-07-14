# Delete Invalid Refresh Credentials Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Delete credentials whose refresh token is permanently invalid when `delete-unauthorized-auth` is enabled, including xAI HTTP 400 `invalid_grant` responses.

**Architecture:** Parse xAI token endpoint failures into a structured OAuth error at the wire boundary, preserving the actual HTTP status and OAuth code. In the auth manager, classify refresh-time 401 and HTTP 400/401 `invalid_grant` as terminal credential failures, then either reuse the existing full deletion path or retain and unschedule the credential according to configuration.

**Tech Stack:** Go 1.26+, `net/http`, `encoding/json`, `errors.As`, Gin-independent auth manager tests, Go standard `testing` package.

## Global Constraints

- Preserve the actual upstream HTTP status; do not rewrite xAI HTTP 400 as HTTP 401.
- Only refresh-time HTTP 401 and HTTP 400/401 OAuth `invalid_grant` are terminal credential failures.
- Other HTTP 400 and transient refresh failures keep the existing backoff/retry behavior.
- `delete-unauthorized-auth: true` removes the auth from memory, store, scheduler, model registry, cooldown state, and automatic refresh loop through the existing cleanup path.
- `delete-unauthorized-auth: false` retains the auth but stops automatic refresh until explicit repair or replacement.
- Do not re-enable xAI probing in the management cleanup endpoint.
- Keep changes small, comments in English, and all Go edits formatted with `gofmt`.
- Test observable xAI error contracts and manager lifecycle behavior; do not assert source text or private helper delegation.

---

## File Structure

- `internal/auth/xai/xai.go`: owns xAI OAuth token response parsing and produces structured token endpoint errors.
- `internal/auth/xai/xai_auth_test.go`: verifies the public `RefreshTokens` error contract against a real HTTP test server.
- `sdk/cliproxy/auth/conductor.go`: owns provider-neutral refresh failure classification and credential lifecycle transitions.
- `sdk/cliproxy/auth/auto_refresh_loop.go`: excludes retained terminal refresh failures from scheduling.
- `sdk/cliproxy/auth/delete_unauthorized_auth_test.go`: verifies deletion, retention, and non-terminal HTTP 400 behavior through `StartAutoRefresh`.
- `internal/config/config.go`: documents the expanded configuration contract.
- `config.example.yaml`: documents the operator-visible configuration behavior.

### Task 1: Return Structured xAI OAuth Token Errors

**Files:**
- Modify: `internal/auth/xai/xai_auth_test.go`
- Modify: `internal/auth/xai/xai.go`

**Interfaces:**
- Consumes: `(*XAIAuth).RefreshTokens(ctx context.Context, refreshToken, tokenEndpoint string) (*TokenData, error)`.
- Produces: errors implementing `StatusCode() int` and `OAuthErrorCode() string` for non-200 token responses.

- [ ] **Step 1: Write the failing public-contract test**

Add `errors` to the imports and append this test to `internal/auth/xai/xai_auth_test.go`:

```go
func TestRefreshTokensInvalidGrantReturnsStructuredOAuthError(t *testing.T) {
	resetXAIRefreshGroupForTest()
	t.Cleanup(resetXAIRefreshGroupForTest)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Refresh token has been revoked"}`))
	}))
	defer server.Close()

	auth := NewXAIAuth(nil)
	_, errRefresh := auth.RefreshTokens(context.Background(), "revoked-refresh-token", server.URL)
	if errRefresh == nil {
		t.Fatal("RefreshTokens() error = nil, want invalid_grant")
	}

	var statusErr interface{ StatusCode() int }
	if !errors.As(errRefresh, &statusErr) {
		t.Fatalf("RefreshTokens() error = %T, want StatusCode()", errRefresh)
	}
	if got := statusErr.StatusCode(); got != http.StatusBadRequest {
		t.Fatalf("StatusCode() = %d, want %d", got, http.StatusBadRequest)
	}

	var oauthErr interface{ OAuthErrorCode() string }
	if !errors.As(errRefresh, &oauthErr) {
		t.Fatalf("RefreshTokens() error = %T, want OAuthErrorCode()", errRefresh)
	}
	if got := oauthErr.OAuthErrorCode(); got != "invalid_grant" {
		t.Fatalf("OAuthErrorCode() = %q, want invalid_grant", got)
	}
	if !strings.Contains(errRefresh.Error(), "Refresh token has been revoked") {
		t.Fatalf("RefreshTokens() error = %q, want revoked description", errRefresh)
	}
}
```

- [ ] **Step 2: Run the test and verify RED**

Run:

```bash
go test -v -run TestRefreshTokensInvalidGrantReturnsStructuredOAuthError ./internal/auth/xai
```

Expected: FAIL at the `StatusCode()` assertion because `postTokenForm` currently returns a plain `fmt.Errorf`.

- [ ] **Step 3: Implement the structured wire error**

Add this type near the xAI auth package-level declarations in `internal/auth/xai/xai.go`:

```go
type oauthTokenError struct {
	status      int
	code        string
	description string
	body        string
}

func (e *oauthTokenError) Error() string {
	if e == nil {
		return ""
	}
	detail := strings.TrimSpace(e.body)
	if detail == "" {
		detail = strings.TrimSpace(e.description)
	}
	return fmt.Sprintf("xai token request failed with status %d: %s", e.status, detail)
}

func (e *oauthTokenError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.status
}

func (e *oauthTokenError) OAuthErrorCode() string {
	if e == nil {
		return ""
	}
	return e.code
}
```

Replace the non-200 branch in `postTokenForm` with structured parsing that never makes error-body JSON validity a second failure:

```go
	if resp.StatusCode != http.StatusOK {
		var payload struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &payload)
		return nil, &oauthTokenError{
			status:      resp.StatusCode,
			code:        strings.TrimSpace(payload.Error),
			description: strings.TrimSpace(payload.ErrorDescription),
			body:        strings.TrimSpace(string(body)),
		}
	}
```

- [ ] **Step 4: Format and verify GREEN**

Run:

```bash
gofmt -w internal/auth/xai/xai.go internal/auth/xai/xai_auth_test.go
go test -v -run 'TestRefreshTokens(InvalidGrantReturnsStructuredOAuthError|PostsClientIDAndRefreshToken)' ./internal/auth/xai
```

Expected: both tests PASS, with the invalid-grant error retaining status 400 and OAuth code `invalid_grant`.

- [ ] **Step 5: Run the complete xAI auth package tests**

Run:

```bash
go test ./internal/auth/xai
```

Expected: PASS.

- [ ] **Step 6: Commit the wire-boundary change**

```bash
git add internal/auth/xai/xai.go internal/auth/xai/xai_auth_test.go
git commit -m "fix(xai): preserve oauth refresh error semantics"
```

### Task 2: Apply Terminal Refresh Credential Lifecycle

**Files:**
- Modify: `sdk/cliproxy/auth/delete_unauthorized_auth_test.go`
- Modify: `sdk/cliproxy/auth/conductor.go`
- Modify: `sdk/cliproxy/auth/auto_refresh_loop.go`
- Modify: `internal/config/config.go`
- Modify: `config.example.yaml`

**Interfaces:**
- Consumes: refresh errors implementing `StatusCode() int`; optionally consumes `OAuthErrorCode() string` from Task 1.
- Produces: provider-neutral terminal refresh classification, deletion through `removeDeletedAuth`, and retained-auth unscheduling.

- [ ] **Step 1: Add reusable behavior-test fixtures**

Extend imports in `sdk/cliproxy/auth/delete_unauthorized_auth_test.go` with `fmt`, `sync/atomic`, and `time`. Add these fixtures after `deleteTrackingStore`:

```go
type refreshOAuthError struct {
	status  int
	code    string
	message string
}

func (e *refreshOAuthError) Error() string {
	return fmt.Sprintf("token refresh failed with status %d: %s", e.status, e.message)
}

func (e *refreshOAuthError) StatusCode() int { return e.status }

func (e *refreshOAuthError) OAuthErrorCode() string { return e.code }

type oauthRefreshFailureExecutor struct {
	schedulerProviderTestExecutor
	err   error
	calls atomic.Int32
}

func (e *oauthRefreshFailureExecutor) Refresh(context.Context, *Auth) (*Auth, error) {
	e.calls.Add(1)
	return nil, e.err
}

func registerExpiredRefreshAuth(t *testing.T, manager *Manager, id, provider string) {
	t.Helper()
	auth := &Auth{
		ID:       id,
		Provider: provider,
		Metadata: map[string]any{
			"access_token":  "expired-access-token",
			"refresh_token": "refresh-token",
			"expired":       time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
}

func waitForAuthCondition(t *testing.T, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(time.Millisecond)
	}
}
```

- [ ] **Step 2: Write the failing enabled-deletion test**

Append:

```go
func TestManagerAutoRefreshDeletesInvalidGrantWhenEnabled(t *testing.T) {
	SetDeleteUnauthorizedAuth(true)
	t.Cleanup(func() { SetDeleteUnauthorizedAuth(false) })

	store := &deleteTrackingStore{}
	manager := NewManager(store, nil, nil)
	executor := &oauthRefreshFailureExecutor{
		schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "xai"},
		err: &refreshOAuthError{status: http.StatusBadRequest, code: "invalid_grant", message: "Refresh token has been revoked"},
	}
	manager.RegisterExecutor(executor)
	registerExpiredRefreshAuth(t, manager, "xai-revoked", "xai")

	manager.StartAutoRefresh(context.Background(), time.Millisecond)
	t.Cleanup(manager.StopAutoRefresh)
	waitForAuthCondition(t, func() bool { return len(store.deleted()) == 1 }, "revoked auth deletion")

	if _, ok := manager.GetByID("xai-revoked"); ok {
		t.Fatal("expected revoked auth to be removed from manager")
	}
	if got := store.deleted(); len(got) != 1 || got[0] != "xai-revoked" {
		t.Fatalf("deleted auths = %v, want [xai-revoked]", got)
	}
}
```

- [ ] **Step 3: Write the failing disabled-retention test**

Append:

```go
func TestManagerAutoRefreshRetainsInvalidGrantWhenDisabled(t *testing.T) {
	SetDeleteUnauthorizedAuth(false)
	t.Cleanup(func() { SetDeleteUnauthorizedAuth(false) })

	store := &deleteTrackingStore{}
	manager := NewManager(store, nil, nil)
	executor := &oauthRefreshFailureExecutor{
		schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "xai"},
		err: &refreshOAuthError{status: http.StatusBadRequest, code: "invalid_grant", message: "Refresh token has been revoked"},
	}
	manager.RegisterExecutor(executor)
	registerExpiredRefreshAuth(t, manager, "xai-retained", "xai")

	manager.StartAutoRefresh(context.Background(), time.Millisecond)
	t.Cleanup(manager.StopAutoRefresh)
	waitForAuthCondition(t, func() bool {
		auth, ok := manager.GetByID("xai-retained")
		return ok && auth.LastError != nil && auth.LastError.Code == "invalid_grant"
	}, "retained invalid_grant state")

	retained, ok := manager.GetByID("xai-retained")
	if !ok || retained == nil {
		t.Fatal("expected invalid auth to remain registered")
	}
	if !retained.Unavailable || retained.Status != StatusError {
		t.Fatalf("retained auth state = unavailable:%v status:%s", retained.Unavailable, retained.Status)
	}
	if !retained.NextRefreshAfter.IsZero() {
		t.Fatalf("NextRefreshAfter = %s, want zero terminal state", retained.NextRefreshAfter)
	}
	if got := store.deleted(); len(got) != 0 {
		t.Fatalf("deleted auths = %v, want none", got)
	}

	calls := executor.calls.Load()
	time.Sleep(20 * time.Millisecond)
	if got := executor.calls.Load(); got != calls {
		t.Fatalf("refresh calls = %d after terminal failure, want %d", got, calls)
	}
}
```

- [ ] **Step 4: Write the failing non-terminal HTTP 400 test**

Append:

```go
func TestManagerAutoRefreshKeepsNonInvalidGrantBadRequest(t *testing.T) {
	SetDeleteUnauthorizedAuth(true)
	t.Cleanup(func() { SetDeleteUnauthorizedAuth(false) })

	store := &deleteTrackingStore{}
	manager := NewManager(store, nil, nil)
	executor := &oauthRefreshFailureExecutor{
		schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "xai"},
		err: &refreshOAuthError{status: http.StatusBadRequest, code: "invalid_request", message: "malformed request"},
	}
	manager.RegisterExecutor(executor)
	registerExpiredRefreshAuth(t, manager, "xai-bad-request", "xai")

	manager.StartAutoRefresh(context.Background(), time.Millisecond)
	t.Cleanup(manager.StopAutoRefresh)
	waitForAuthCondition(t, func() bool {
		auth, ok := manager.GetByID("xai-bad-request")
		return ok && auth.LastError != nil && !auth.NextRefreshAfter.IsZero()
	}, "transient bad-request backoff")

	retained, ok := manager.GetByID("xai-bad-request")
	if !ok || retained.LastError.StatusCode() != http.StatusBadRequest {
		t.Fatalf("expected retained HTTP 400 auth, got %#v", retained)
	}
	if got := store.deleted(); len(got) != 0 {
		t.Fatalf("deleted auths = %v, want none", got)
	}
}
```

- [ ] **Step 5: Run the three tests and verify RED**

Run:

```bash
go test -v -run 'TestManagerAutoRefresh(DeletesInvalidGrantWhenEnabled|RetainsInvalidGrantWhenDisabled|KeepsNonInvalidGrantBadRequest)' ./sdk/cliproxy/auth
```

Expected: the enabled-deletion test times out because refresh failures never delete; the disabled-retention test times out because `invalid_grant` is not classified; the ordinary HTTP 400 test already demonstrates the behavior that must remain unchanged.

- [ ] **Step 6: Add provider-neutral OAuth and terminal failure classification**

In `sdk/cliproxy/auth/conductor.go`, add OAuth code extraction and replace the current invalid-grant classifier with:

```go
func oauthErrorCodeFromError(err error) string {
	if err == nil {
		return ""
	}
	type oauthErrorCoder interface {
		OAuthErrorCode() string
	}
	var coder oauthErrorCoder
	if errors.As(err, &coder) && coder != nil {
		return strings.TrimSpace(coder.OAuthErrorCode())
	}
	return ""
}

func isInvalidGrantError(err error) bool {
	if err == nil {
		return false
	}
	status := statusCodeFromError(err)
	if status != http.StatusBadRequest && status != http.StatusUnauthorized {
		return false
	}
	if code := oauthErrorCodeFromError(err); code != "" {
		return strings.EqualFold(code, "invalid_grant")
	}
	return isInvalidGrantErrorMessage(err.Error())
}

func isPermanentRefreshAuthError(err error) bool {
	return isUnauthorizedError(err) || isInvalidGrantError(err)
}
```

Replace `hasUnauthorizedAuthFailure` with:

```go
func hasTerminalRefreshAuthFailure(auth *Auth) bool {
	if auth == nil || auth.LastError == nil {
		return false
	}
	return auth.LastError.StatusCode() == http.StatusUnauthorized ||
		strings.EqualFold(auth.LastError.Code, "unauthorized") ||
		strings.EqualFold(auth.LastError.Code, "invalid_grant")
}
```

Update both `Manager.shouldRefresh` in `conductor.go` and `nextRefreshCheckAt` in `auto_refresh_loop.go` to call `hasTerminalRefreshAuthFailure`.

Update `refreshErrorFromError` so terminal errors get stable codes while preserving status 400:

```go
func refreshErrorFromError(err error) *Error {
	if err == nil {
		return nil
	}
	statusCode := statusCodeFromError(err)
	authErr := &Error{Message: err.Error(), HTTPStatus: statusCode}
	switch {
	case isInvalidGrantError(err):
		authErr.Code = "invalid_grant"
		authErr.Retryable = false
	case isUnauthorizedError(err):
		if authErr.HTTPStatus == 0 {
			authErr.HTTPStatus = http.StatusUnauthorized
		}
		authErr.Code = "unauthorized"
		authErr.Retryable = false
	}
	return authErr
}
```

- [ ] **Step 7: Route terminal refresh failures through deletion or unscheduling**

Replace the refresh error branch in `refreshAuthForRequest` with this lifecycle decision:

```go
	if err != nil {
		terminal := isPermanentRefreshAuthError(err)
		shouldDelete := false
		shouldReschedule := false
		shouldUnschedule := false
		var deleteStore Store
		m.mu.Lock()
		if current := m.auths[id]; current != nil {
			current.LastError = refreshErrorFromError(err)
			if terminal && deleteUnauthorizedAuth.Load() {
				deleteStore = m.store
				delete(m.auths, id)
				shouldDelete = true
			} else {
				if terminal {
					current.NextRefreshAfter = time.Time{}
					current.Unavailable = true
					current.Status = StatusError
					current.StatusMessage = current.LastError.Code
					shouldUnschedule = true
				} else {
					current.NextRefreshAfter = now.Add(refreshFailureBackoff)
					shouldReschedule = true
				}
				m.auths[id] = current
				if m.scheduler != nil {
					m.scheduler.upsertAuth(current.Clone())
				}
			}
		}
		m.mu.Unlock()
		if shouldDelete {
			m.removeDeletedAuth(ctx, id, deleteStore)
		} else if shouldUnschedule {
			m.queueRefreshUnschedule(id)
		} else if shouldReschedule {
			m.queueRefreshReschedule(id)
		}
		return nil, err
	}
```

- [ ] **Step 8: Update the configuration contract**

Change the `SetDeleteUnauthorizedAuth` comment in `sdk/cliproxy/auth/conductor.go` to:

```go
// SetDeleteUnauthorizedAuth toggles credential deletion on upstream 401
// responses and permanently invalid OAuth refresh grants.
```

Change the field comment in `internal/config/config.go` to:

```go
	// DeleteUnauthorizedAuth controls whether to delete credentials on 401 responses
	// or permanently invalid OAuth refresh grants. When false (default), terminal
	// refresh failures remain stored but are removed from automatic refresh scheduling.
	// When true, the auth is evicted from memory and removed from the store.
```

Replace the `config.example.yaml` comment with:

```yaml
# When true, credentials returning 401 or a permanently invalid OAuth refresh grant
# (for example, invalid_grant after a refresh token is revoked) are evicted from memory
# and deleted from the store. When false (default), the credential remains stored but
# terminal refresh failures are removed from automatic refresh scheduling.
delete-unauthorized-auth: false
```

- [ ] **Step 9: Format and verify GREEN**

Run:

```bash
gofmt -w sdk/cliproxy/auth/conductor.go sdk/cliproxy/auth/auto_refresh_loop.go sdk/cliproxy/auth/delete_unauthorized_auth_test.go internal/config/config.go
go test -v -run 'TestManagerAutoRefresh(DeletesInvalidGrantWhenEnabled|RetainsInvalidGrantWhenDisabled|KeepsNonInvalidGrantBadRequest)|TestManagerMarkResult(KeepsUnauthorizedAuthWhenDeleteDisabled|DeletesUnauthorizedAuthWhenEnabled)' ./sdk/cliproxy/auth
```

Expected: all five tests PASS.

- [ ] **Step 10: Run the complete auth manager package tests**

Run:

```bash
go test ./sdk/cliproxy/auth
```

Expected: PASS.

- [ ] **Step 11: Commit the lifecycle change**

```bash
git add sdk/cliproxy/auth/conductor.go sdk/cliproxy/auth/auto_refresh_loop.go sdk/cliproxy/auth/delete_unauthorized_auth_test.go internal/config/config.go config.example.yaml
git commit -m "fix(auth): delete permanently invalid refresh credentials"
```

### Task 3: Full Verification

**Files:**
- Verify only; no new files.

**Interfaces:**
- Consumes: the xAI structured error contract and auth manager terminal lifecycle from Tasks 1 and 2.
- Produces: fresh test and build evidence for completion.

- [ ] **Step 1: Run all repository tests**

```bash
go test ./...
```

Expected: PASS with zero failed packages.

- [ ] **Step 2: Run the required server build and remove the artifact**

```bash
go build -o test-output ./cmd/server && rm test-output
```

Expected: exit status 0 and no `test-output` artifact remains.

- [ ] **Step 3: Check formatting, whitespace, and scope**

```bash
gofmt -d internal/auth/xai/xai.go internal/auth/xai/xai_auth_test.go sdk/cliproxy/auth/conductor.go sdk/cliproxy/auth/auto_refresh_loop.go sdk/cliproxy/auth/delete_unauthorized_auth_test.go internal/config/config.go
git diff --check
git status --short
```

Expected: `gofmt -d`, `git diff --check`, and `git status --short` produce no output.

- [ ] **Step 4: Review the final diff against the design**

```bash
git diff e95bc062..HEAD -- internal/auth/xai/xai.go internal/auth/xai/xai_auth_test.go sdk/cliproxy/auth/conductor.go sdk/cliproxy/auth/auto_refresh_loop.go sdk/cliproxy/auth/delete_unauthorized_auth_test.go internal/config/config.go config.example.yaml
```

Expected: the diff contains structured xAI OAuth errors, terminal refresh deletion/retention, behavior tests, and configuration comments only; it contains no management cleanup changes.

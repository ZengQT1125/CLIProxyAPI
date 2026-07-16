# Delete XAI credentials directly on permission-denied

**Goal:** Delete an XAI credential immediately when a real provider request returns structured `permission-denied`, without running a credential conversation probe.
**Why planning is required:** This changes automatic credential deletion behavior and therefore has destructive persisted-state impact.
**Acceptance:** With unauthorized-auth deletion enabled, only real XAI request results whose error JSON code is exactly `permission-denied` are removed from runtime and storage without calling `ProbeCredential`; the same error from other providers and XAI errors when deletion is disabled remain retained. Existing `invalid_grant` probe behavior remains unchanged. No live credential files are used or deleted during verification. Recovery is reverting the result classifier change; stop if tests show any provider-wide, substring-based, or probe-triggering behavior.

### Outcome 1: Direct provider-scoped deletion
- Work: Classify structured XAI request-result code `permission-denied` alongside an explicit HTTP 401 and delete through the existing manager/store lifecycle without invoking a probe.
- Verify: `go test ./sdk/cliproxy/auth -run 'TestManagerMarkResult.*PermissionDenied' -count=1`

### Outcome 2: Persisted deletion behavior is regression-covered
- Work: Extend the existing unauthorized-auth tests to prove XAI deletion, zero probe calls, non-XAI retention, and deletion-disabled retention through the manager/store boundary.
- Verify: `go test ./sdk/cliproxy/auth -count=1`

### Outcome 3: Repository remains healthy
- Work: Preserve existing request, refresh, and upload changes without unrelated edits.
- Verify: `gofmt -w . && go test ./... && go build -o test-output ./cmd/server && rm test-output`

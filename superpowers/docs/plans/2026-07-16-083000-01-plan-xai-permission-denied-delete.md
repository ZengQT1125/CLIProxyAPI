# Delete unusable XAI credentials after permission-denied probe

**Goal:** Treat a structured XAI `permission-denied` conversation-probe result as definitive credential rejection when unauthorized-auth deletion is enabled.
**Why planning is required:** This changes automatic credential deletion behavior and therefore has destructive persisted-state impact.
**Acceptance:** Only XAI credentials whose deletion-confirmation conversation probe returns JSON code `permission-denied` are removed; the same error from other providers and unrelated inconclusive probe failures remain retained. No live credential files are used or deleted during verification. Recovery is reverting the classifier change; stop if tests show any provider-wide or substring-based deletion behavior.

### Outcome 1: Provider-scoped rejection classification
- Work: Classify structured XAI probe code `permission-denied` alongside an explicit HTTP 401, without broadening the global unauthorized classifier.
- Verify: `go test ./sdk/cliproxy/auth -run 'TestManagerAutoRefresh.*ConversationProbe' -count=1`

### Outcome 2: Persisted deletion behavior is regression-covered
- Work: Extend the existing unauthorized-auth tests to prove XAI deletion and non-XAI retention through the manager/store boundary.
- Verify: `go test ./sdk/cliproxy/auth -count=1`

### Outcome 3: Repository remains healthy
- Work: Preserve existing request, refresh, and upload changes without unrelated edits.
- Verify: `gofmt -w . && go test ./... && go build -o test-output ./cmd/server && rm test-output`

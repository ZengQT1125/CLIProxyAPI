package executor

import (
	"context"
	"testing"

	internalcache "github.com/router-for-me/CLIProxyAPI/v7/internal/cache"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func TestDebugStaleThoughtSig2(t *testing.T) {
	internalcache.ClearAntigravityReasoningReplayCache()
	t.Cleanup(internalcache.ClearAntigravityReasoningReplayCache)

	item := []byte(`{"type":"thought_signature","contentIndex":8,"partIndex":3,"thoughtSignature":"sig-text"}`)
	if !internalcache.CacheAntigravityReasoningReplayItems("gemini-3-flash-agent", "session:sess-stale-text", [][]byte{item}) {
		t.Fatal("cache write failed")
	}

	// Verify cache read
	items, ok := internalcache.GetAntigravityReasoningReplayItems("gemini-3-flash-agent", "session:sess-stale-text")
	t.Logf("GetAntigravityReasoningReplayItems: ok=%v items=%d", ok, len(items))

	items2, ok2, err := internalcache.GetAntigravityReasoningReplayItemsRequired(context.Background(), "gemini-3-flash-agent", "session:sess-stale-text")
	t.Logf("GetAntigravityReasoningReplayItemsRequired: ok=%v err=%v items=%d", ok2, err, len(items2))

	payload := []byte(`{"sessionId":"sess-stale-text","request":{"contents":[{"role":"user","parts":[{"text":"hi"}]},{"role":"model","parts":[{"text":"visible answer"}]},{"role":"user","parts":[{"text":"next"}]}]}}`)

	// Check scope
	scope := antigravityReasoningReplayScopeFromPayload("gemini-3-flash-agent", payload)
	t.Logf("scope: valid=%v model=%q session=%q", scope.valid(), scope.modelName, scope.sessionKey)

	// Check if model uses replay cache
	t.Logf("usesReasoningReplayCache: %v", antigravityUsesReasoningReplayCache("gemini-3-flash-agent"))

	// Run the full function
	out, outScope, outErr := prepareAntigravityGeminiReasoningReplayPayload(context.Background(), "gemini-3-flash-agent", cliproxyexecutor.Request{}, cliproxyexecutor.Options{}, payload)
	t.Logf("prepare: err=%v scope.valid=%v", outErr, outScope.valid())
	t.Logf("output: %s", string(out))

	parts := gjson.GetBytes(out, "request.contents.1.parts").Array()
	t.Logf("parts len: %d", len(parts))
}

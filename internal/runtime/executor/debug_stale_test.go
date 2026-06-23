package executor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestDebugStaleThoughtSig(t *testing.T) {
	payload := []byte(`{"sessionId":"sess-stale-text","request":{"contents":[{"role":"user","parts":[{"text":"hi"}]},{"role":"model","parts":[{"text":"visible answer"}]},{"role":"user","parts":[{"text":"next"}]}]}}`)
	item := []byte(`{"type":"thought_signature","contentIndex":8,"partIndex":3,"thoughtSignature":"sig-text"}`)

	// Step 1: filter
	filtered := filterAntigravityReasoningReplayItemsForRequest(payload, [][]byte{item})
	t.Logf("filtered items: %d", len(filtered))
	for i, f := range filtered {
		t.Logf("  filtered[%d]: %s", i, string(f))
	}
	if len(filtered) == 0 {
		t.Fatal("item was filtered out!")
	}
	
	// Step 2: insert
	itemResult := gjson.ParseBytes(filtered[0])
	tp := strings.TrimSpace(itemResult.Get("type").String())
	t.Logf("type: %q", tp)
	
	ci := antigravityReasoningReplayResolveContentIndex(payload, int(itemResult.Get("contentIndex").Int()))
	pi := int(itemResult.Get("partIndex").Int())
	sig := strings.TrimSpace(itemResult.Get("thoughtSignature").String())
	t.Logf("resolved ci=%d, pi=%d, sig=%q", ci, pi, sig)
	
	partPath, exists := antigravityExistingReplayPartPath(payload, ci, pi)
	t.Logf("existingReplayPartPath: path=%q exists=%v", partPath, exists)
	
	writePath := antigravityReplayPartWritePath(payload, ci, pi)
	t.Logf("writePath: %q", writePath)
	
	fullPath := writePath + ".thoughtSignature"
	t.Logf("fullPath for sjson: %q", fullPath)
	
	out, changed := insertAntigravityReasoningReplayItems(payload, filtered)
	t.Logf("changed: %v", changed)
	t.Logf("result: %s", string(out))
	
	parts := gjson.GetBytes(out, fmt.Sprintf("request.contents.%d.parts", ci)).Array()
	t.Logf("parts len: %d", len(parts))
	for i, p := range parts {
		t.Logf("  parts[%d]: %s", i, p.Raw)
	}
}

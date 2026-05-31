package executor

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestIsInvalidResponsesEncryptedContentError(t *testing.T) {
	body := []byte(`{
		"error":{
			"code":"invalid_encrypted_content",
			"type":"invalid_request_error",
			"message":"The encrypted content gAAA...Vw== could not be verified. Reason: Encrypted content could not be decrypted or parsed."
		}
	}`)

	if !isInvalidResponsesEncryptedContentError(http.StatusBadRequest, body) {
		t.Fatalf("expected invalid encrypted content error to be detected")
	}
	if isInvalidResponsesEncryptedContentError(http.StatusInternalServerError, body) {
		t.Fatalf("non-400 response should not trigger encrypted content fallback")
	}
}

func TestStripInvalidEncryptedContentFromResponsesBody(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"user","content":"hello"},
			{"type":"compaction","encrypted_content":"keep-compaction"},
			{"type":"reasoning","id":"rs_bad","encrypted_content":"gAAA"},
			{"type":"reasoning","id":"rs_missing","summary":[]},
			{"type":"function_call","call_id":"call_123","name":"lookup","arguments":"{}"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done","encrypted_content":"nested"}]}
		]
	}`)

	got, changed := stripInvalidEncryptedContentFromResponsesBody(raw)
	if !changed {
		t.Fatalf("expected body to be changed")
	}
	items := gjson.GetBytes(got, "input").Array()
	if len(items) != 4 {
		t.Fatalf("expected reasoning item to be removed, got %d items: %s", len(items), got)
	}
	if typ := gjson.GetBytes(got, "input.0.type").String(); typ != "message" {
		t.Fatalf("first input should remain message, got %q; body=%s", typ, got)
	}
	if typ := gjson.GetBytes(got, "input.1.type").String(); typ != "compaction" {
		t.Fatalf("compaction item should remain, got %q; body=%s", typ, got)
	}
	if encrypted := gjson.GetBytes(got, "input.1.encrypted_content").String(); encrypted != "keep-compaction" {
		t.Fatalf("compaction encrypted_content should be preserved, got %q; body=%s", encrypted, got)
	}
	if typ := gjson.GetBytes(got, "input.2.type").String(); typ != "function_call" {
		t.Fatalf("function call should remain, got %q; body=%s", typ, got)
	}
	if strings.Contains(gjson.GetBytes(got, `input.#(type=="reasoning")`).Raw, "encrypted_content") {
		t.Fatalf("reasoning encrypted_content should be removed from retry body: %s", got)
	}
}

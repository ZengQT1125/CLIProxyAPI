package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

const invalidEncryptedContentErrBody = `{"error":{"code":"invalid_encrypted_content","type":"invalid_request_error","message":"The encrypted content gAAA...Vw== could not be verified. Reason: Encrypted content could not be decrypted or parsed."}}`

// TestCodexExecutorExecuteStreamRetriesOnInvalidEncryptedContent reproduces the
// upstream "Encrypted content could not be decrypted or parsed." rejection and
// verifies the executor retries once after stripping reasoning encrypted_content.
func TestCodexExecutorExecuteStreamRetriesOnInvalidEncryptedContent(t *testing.T) {
	validEncryptedContent := validCodexReasoningEncryptedContentForTest()

	var calls int32
	var firstBody, retryBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			firstBody = body
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(invalidEncryptedContentErrBody))
		default:
			retryBody = body
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"background\":false,\"error\":null}}\n\n"))
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	result, err := executor.ExecuteStream(context.Background(), newCodexSignatureTestAuth(server.URL), cliproxyexecutor.Request{
		Model: "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","stream":true,"input":[` +
			`{"role":"user","content":"hello"},` +
			`{"id":"rs_good","type":"reasoning","encrypted_content":"` + validEncryptedContent + `","summary":[]}` +
			`]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for range result.Chunks {
	}

	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected exactly 2 upstream calls (original + retry), got %d", got)
	}
	// First attempt must still carry the (valid-shaped) encrypted reasoning that upstream rejected.
	if !gjson.GetBytes(firstBody, "input.1.encrypted_content").Exists() {
		t.Fatalf("first request should carry encrypted reasoning; body=%s", firstBody)
	}
	// Retry must drop the whole reasoning item and any encrypted_content from input.
	if strings.Contains(gjson.GetBytes(retryBody, "input").Raw, "encrypted_content") {
		t.Fatalf("retry request input must not contain encrypted_content; body=%s", retryBody)
	}
	if gjson.GetBytes(retryBody, `input.#(type=="reasoning")`).Exists() {
		t.Fatalf("retry request must not contain reasoning items; body=%s", retryBody)
	}
	if got := gjson.GetBytes(retryBody, "input.0.role").String(); got != "user" {
		t.Fatalf("retry request should keep the user message, got role=%q; body=%s", got, retryBody)
	}
}

// TestCodexExecutorExecuteRetriesOnInvalidEncryptedContent covers the non-stream path.
func TestCodexExecutorExecuteRetriesOnInvalidEncryptedContent(t *testing.T) {
	validEncryptedContent := validCodexReasoningEncryptedContentForTest()

	var calls int32
	var retryBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(invalidEncryptedContentErrBody))
		default:
			retryBody = body
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"background\":false,\"error\":null}}\n\n"))
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	_, err := executor.Execute(context.Background(), newCodexSignatureTestAuth(server.URL), cliproxyexecutor.Request{
		Model: "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":[` +
			`{"role":"user","content":"hello"},` +
			`{"id":"rs_good","type":"reasoning","encrypted_content":"` + validEncryptedContent + `","summary":[]}` +
			`]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected exactly 2 upstream calls (original + retry), got %d", got)
	}
	if strings.Contains(gjson.GetBytes(retryBody, "input").Raw, "encrypted_content") {
		t.Fatalf("retry request input must not contain encrypted_content; body=%s", retryBody)
	}
}

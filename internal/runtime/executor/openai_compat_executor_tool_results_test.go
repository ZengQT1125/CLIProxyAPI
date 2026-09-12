package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

// countImagePartsInBody counts image content parts anywhere in the request body,
// regardless of which message the translator placed them in.
func countImagePartsInBody(body []byte) int {
	count := 0
	gjson.GetBytes(body, "messages").ForEach(func(_, message gjson.Result) bool {
		message.Get("content").ForEach(func(_, part gjson.Result) bool {
			switch strings.ToLower(strings.TrimSpace(part.Get("type").String())) {
			case "image", "image_url", "input_image":
				count++
			default:
				if part.Get("image_url").Exists() || part.Get("input_image").Exists() {
					count++
				}
			}
			return true
		})
		return true
	})
	return count
}

func TestOpenAICompatExecutorToolResultContentByInputModalities(t *testing.T) {
	// The upstream translator always flattens Claude tool_result content into a
	// string and relays any images into a following user message. The fork's
	// input_modalities feature then decides whether those relayed images survive.
	tests := []struct {
		name            string
		stream          bool
		inputModalities []string
		wantImages      bool
	}{
		{name: "non-stream text-only", stream: false, inputModalities: []string{"text"}, wantImages: false},
		{name: "stream text-only", stream: true, inputModalities: []string{"text"}, wantImages: false},
		{name: "non-stream multimodal", stream: false, inputModalities: []string{"text", "image"}, wantImages: true},
		{name: "non-stream unspecified", stream: false, inputModalities: nil, wantImages: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotBody []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotBody, _ = io.ReadAll(r.Body)
				if tt.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("data: [DONE]\n\n"))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
			}))
			defer server.Close()

			executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{
				OpenAICompatibility: []config.OpenAICompatibility{{
					Name: "compat",
					Models: []config.OpenAICompatibilityModel{{
						Name:            "mapped-model",
						Alias:           "claude-client",
						InputModalities: tt.inputModalities,
					}},
				}},
			})
			auth := &cliproxyauth.Auth{
				Provider: "openai-compatibility",
				Attributes: map[string]string{
					"base_url":     server.URL + "/v1",
					"api_key":      "test",
					"compat_name":  "compat",
					"provider_key": "compat",
				},
			}
			payload := []byte(`{"model":"claude-client","max_tokens":64,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"inspect_image","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"image inspected"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]}]}]}`)
			req := cliproxyexecutor.Request{Model: "mapped-model", Payload: payload}
			opts := cliproxyexecutor.Options{
				SourceFormat:   sdktranslator.FormatClaude,
				ResponseFormat: sdktranslator.FormatOpenAI,
				Stream:         tt.stream,
			}

			if tt.stream {
				result, errExecute := executor.ExecuteStream(context.Background(), auth, req, opts)
				if errExecute != nil {
					t.Fatalf("ExecuteStream error: %v", errExecute)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatalf("stream chunk error: %v", chunk.Err)
					}
				}
			} else if _, errExecute := executor.Execute(context.Background(), auth, req, opts); errExecute != nil {
				t.Fatalf("Execute error: %v", errExecute)
			}

			// Upstream contract: tool_result text is flattened into a string and
			// the image is relayed out of the tool message.
			toolContent := gjson.GetBytes(gotBody, "messages.1.content")
			if toolContent.Type != gjson.String {
				t.Fatalf("tool content type = %s, want string; body=%s", toolContent.Type, string(gotBody))
			}
			if toolContent.String() != "image inspected" {
				t.Fatalf("tool content = %q, want %q", toolContent.String(), "image inspected")
			}

			// Fork feature: a text-only model must not receive the relayed image,
			// while models that accept image input keep it.
			imageParts := countImagePartsInBody(gotBody)
			if tt.wantImages {
				if imageParts == 0 {
					t.Fatalf("body has no image part, want relayed image; body=%s", string(gotBody))
				}
				return
			}
			if imageParts != 0 {
				t.Fatalf("body has %d image part(s) for a text-only model; body=%s", imageParts, string(gotBody))
			}
			if !strings.Contains(string(gotBody), "[image omitted: unsupported by upstream]") {
				t.Fatalf("expected image omission marker; body=%s", string(gotBody))
			}
		})
	}
}

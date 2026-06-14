package chat_completions

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIRequestToAntigravity_WebSearchToolOnlyWhenMixed(t *testing.T) {
	input := []byte(`{
		"model":"gemini-3-flash-preview",
		"messages":[{"role":"user","content":"hello"}],
		"tools":[
			{"type":"function","function":{"name":"add_memory","parameters":{"type":"object","properties":{"content":{"type":"string"}}}}},
			{"google_search":{}},
			{"code_execution":{}}
		]
	}`)

	out := ConvertOpenAIRequestToAntigravity("gemini-3-flash-preview", input, false)

	tools := gjson.GetBytes(out, "request.tools").Array()
	if got := len(tools); got != 3 {
		t.Fatalf("len(request.tools) = %d, want %d", got, 3)
	}
	if !gjson.GetBytes(out, "request.tools.0.functionDeclarations").Exists() {
		t.Fatalf("request.tools.0.functionDeclarations missing")
	}
	if !gjson.GetBytes(out, "request.tools.1.googleSearch").Exists() {
		t.Fatalf("request.tools.1.googleSearch missing")
	}
	if !gjson.GetBytes(out, "request.tools.2.codeExecution").Exists() {
		t.Fatalf("request.tools.2.codeExecution missing")
	}
}

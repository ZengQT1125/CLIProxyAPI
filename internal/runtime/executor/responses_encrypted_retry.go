package executor

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// isInvalidResponsesEncryptedContentError reports whether an upstream /responses error
// indicates that previously supplied encrypted reasoning content can no longer be
// decrypted or verified (e.g. after credential rotation or cross-account replay).
func isInvalidResponsesEncryptedContentError(statusCode int, body []byte) bool {
	if statusCode != http.StatusBadRequest {
		return false
	}
	for _, path := range []string{"error.code", "detail.code", "code"} {
		if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, path).String()), "invalid_encrypted_content") {
			return true
		}
	}

	msgParts := []string{
		gjson.GetBytes(body, "error.message").String(),
		gjson.GetBytes(body, "detail").String(),
		string(body),
	}
	for _, msg := range msgParts {
		msg = strings.ToLower(msg)
		if strings.Contains(msg, "invalid_encrypted_content") {
			return true
		}
		if strings.Contains(msg, "encrypted content") &&
			(strings.Contains(msg, "could not be verified") || strings.Contains(msg, "could not be decrypted")) {
			return true
		}
	}
	return false
}

// stripInvalidEncryptedContentFromResponsesBody removes reasoning items from a
// /responses request body so it can be retried without the rejected encrypted
// reasoning context.
func stripInvalidEncryptedContentFromResponsesBody(body []byte) ([]byte, bool) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil || root == nil {
		return body, false
	}
	input, ok := root["input"]
	if !ok {
		return body, false
	}
	strippedInput, changed, keep := stripInvalidEncryptedContentValue(input, false)
	if !changed {
		return body, false
	}
	if keep {
		root["input"] = strippedInput
	} else {
		delete(root, "input")
	}
	stripped, err := json.Marshal(root)
	if err != nil {
		return body, false
	}
	return stripped, true
}

func stripInvalidEncryptedContentValue(value any, arrayItem bool) (any, bool, bool) {
	switch v := value.(type) {
	case []any:
		changed := false
		out := make([]any, 0, len(v))
		for _, item := range v {
			stripped, itemChanged, keep := stripInvalidEncryptedContentValue(item, true)
			if itemChanged {
				changed = true
			}
			if !keep {
				changed = true
				continue
			}
			out = append(out, stripped)
		}
		return out, changed, true
	case map[string]any:
		changed := false
		if strings.TrimSpace(firstNonEmptyAnyString(v["type"])) == "reasoning" {
			return nil, true, false
		}
		for key, child := range v {
			stripped, childChanged, keep := stripInvalidEncryptedContentValue(child, false)
			if childChanged {
				changed = true
			}
			if keep {
				v[key] = stripped
			} else {
				delete(v, key)
			}
		}
		return v, changed, true
	default:
		return value, false, true
	}
}

func firstNonEmptyAnyString(values ...any) string {
	for _, value := range values {
		if s, ok := value.(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

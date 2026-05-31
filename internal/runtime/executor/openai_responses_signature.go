package executor

import (
	"context"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/signature"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func sanitizeOpenAIResponsesReasoningEncryptedContent(ctx context.Context, provider string, body []byte) []byte {
	input := gjson.GetBytes(body, "input")
	if !input.Exists() || !input.IsArray() {
		return body
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = "openai responses upstream"
	}

	type invalidReasoningItem struct {
		index  int
		id     string
		reason string
	}
	var invalidItems []invalidReasoningItem
	for index, item := range input.Array() {
		if strings.TrimSpace(item.Get("type").String()) != "reasoning" {
			continue
		}

		reason := ""
		encryptedContent := item.Get("encrypted_content")
		if !encryptedContent.Exists() {
			reason = "encrypted_content is missing"
		} else {
			switch encryptedContent.Type {
			case gjson.String:
				rawSignature := encryptedContent.String()
				if rawSignature != strings.TrimSpace(rawSignature) {
					reason = "encrypted_content has leading or trailing whitespace"
				} else if _, err := signature.InspectGPTReasoningSignature(rawSignature); err != nil {
					reason = err.Error()
				}
			case gjson.Null:
				reason = "encrypted_content is null"
			default:
				reason = fmt.Sprintf("encrypted_content must be a string, got %s", encryptedContent.Type.String())
			}
		}
		if reason == "" {
			continue
		}

		itemID := strings.TrimSpace(item.Get("id").String())
		if itemID == "" {
			itemID = fmt.Sprintf("input[%d]", index)
		}
		invalidItems = append(invalidItems, invalidReasoningItem{
			index:  index,
			id:     itemID,
			reason: reason,
		})
	}

	updated := body
	for i := len(invalidItems) - 1; i >= 0; i-- {
		item := invalidItems[i]
		path := fmt.Sprintf("input.%d", item.index)
		next, err := sjson.DeleteBytes(updated, path)
		if err != nil {
			helps.LogWithRequestID(ctx).Debugf("%s: failed to drop invalid reasoning item at input[%d]: %v", provider, item.index, err)
			continue
		}
		updated = next
		helps.LogWithRequestID(ctx).Debugf("%s: dropped invalid reasoning item at input[%d] item_id=%q reason=%s", provider, item.index, item.id, item.reason)
	}
	return updated
}

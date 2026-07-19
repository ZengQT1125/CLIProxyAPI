package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

const (
	cliProxyAPIAuthBundleType    = "cliproxyapi-auth-bundle"
	cliProxyAPIAuthBundleVersion = 1
)

type cliProxyAPIAuthBundle struct {
	Type     string            `json:"type"`
	Version  int               `json:"version"`
	Accounts []json.RawMessage `json:"accounts"`
}

func (h *Handler) importCLIProxyAPIAuthBundle(ctx context.Context, data []byte, maxAccounts int) (authFileImportResult, bool) {
	var probe struct {
		Type string `json:"type"`
	}
	if errDecode := json.Unmarshal(data, &probe); errDecode != nil || !strings.EqualFold(strings.TrimSpace(probe.Type), cliProxyAPIAuthBundleType) {
		return authFileImportResult{}, false
	}

	var bundle cliProxyAPIAuthBundle
	if errDecode := json.Unmarshal(data, &bundle); errDecode != nil {
		return authFileImportResult{fatal: fmt.Errorf("invalid CLIProxyAPI auth bundle: %w", errDecode)}, true
	}
	if bundle.Version != cliProxyAPIAuthBundleVersion {
		return authFileImportResult{fatal: fmt.Errorf("unsupported CLIProxyAPI auth bundle version %d", bundle.Version)}, true
	}
	if len(bundle.Accounts) == 0 {
		return authFileImportResult{fatal: fmt.Errorf("CLIProxyAPI auth bundle contains no accounts")}, true
	}
	if maxAccounts < 1 || len(bundle.Accounts) > maxAccounts {
		return authFileImportResult{fatal: authUploadFileLimitError()}, true
	}

	reservedNames := make(map[string]struct{}, len(bundle.Accounts))
	result := authFileImportResult{failed: make([]authUploadFailure, 0)}
	for index, rawAccount := range bundle.Accounts {
		failureName := fmt.Sprintf("$.accounts[%d]", index)
		baseFilename, errName := cliProxyAPIAuthBundleFilename(rawAccount)
		if errName != nil {
			result.failed = append(result.failed, authUploadFailure{name: failureName, err: errName})
			continue
		}

		payload := append(bytes.TrimSpace(rawAccount), '\n')
		for {
			filename := reserveImportedAuthFileName(baseFilename, reservedNames)
			errWrite := h.writeNewAuthFile(ctx, filename, payload)
			if errors.Is(errWrite, os.ErrExist) {
				continue
			}
			if errWrite != nil {
				result.failed = append(result.failed, authUploadFailure{name: filename, err: errWrite})
				break
			}
			result.uploaded++
			break
		}
	}
	return result, true
}

func cliProxyAPIAuthBundleFilename(rawAccount json.RawMessage) (string, error) {
	var account map[string]any
	if errDecode := json.Unmarshal(rawAccount, &account); errDecode != nil || account == nil {
		return "", fmt.Errorf("account must be an object")
	}
	provider := strings.ToLower(authBundleString(account["type"]))
	providerToken := sanitizeAuthFileToken(provider)
	if providerToken == "" {
		return "", fmt.Errorf("account has no usable type")
	}

	identity := ""
	for _, field := range []string{"email", "account_id", "chatgpt_account_id", "sub", "account_uuid", "local_account_id", "name"} {
		if identity = authBundleString(account[field]); identity != "" {
			break
		}
	}
	identityToken := sanitizeAuthFileToken(identity)
	if identityToken == "" {
		return "", fmt.Errorf("account has no usable email, account id, subject, account UUID, local account id, or name")
	}
	return providerToken + "-" + identityToken + ".json", nil
}

func authBundleString(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

package management

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	xaiauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/xai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	sub2APIDataType      = "sub2api-data"
	sub2APIDataVersion   = 1
	maxSub2APIJSONDepth  = 32
	sub2APIMillisCutover = int64(100_000_000_000)
)

var errAuthUploadFileLimit = fmt.Errorf("too many files: maximum is %d", maxAuthUploadFiles)

type sub2APIAccountCandidate struct {
	record     map[string]any
	path       string
	exportedAt time.Time
}

type sub2APIConvertedAuth struct {
	baseFilename string
	payload      []byte
	fingerprint  [sha256.Size]byte
}

func (h *Handler) importSub2APIData(ctx context.Context, data []byte, maxAccounts int) (authFileImportResult, bool) {
	root, errDecode := decodeSub2APIJSON(data)
	if errDecode != nil {
		return authFileImportResult{}, false
	}
	candidates, handled, errCollect := collectSub2APIAccounts(root)
	if !handled {
		return authFileImportResult{}, false
	}
	if errCollect != nil {
		return authFileImportResult{fatal: errCollect}, true
	}
	if len(candidates) == 0 {
		return authFileImportResult{fatal: fmt.Errorf("sub2api data contains no accounts")}, true
	}
	if maxAccounts < 1 || len(candidates) > maxAccounts {
		return authFileImportResult{fatal: authUploadFileLimitError()}, true
	}

	reservedNames := make(map[string]struct{}, len(candidates))
	now := time.Now().UTC()
	result := authFileImportResult{failed: make([]authUploadFailure, 0)}
	seenFingerprints := make(map[[sha256.Size]byte]struct{}, len(candidates))
	for index := range candidates {
		candidate := candidates[index]
		failureName := candidate.path
		converted, errConvert := convertSub2APIAccount(candidate, now)
		if converted.baseFilename != "" {
			failureName = converted.baseFilename
		}
		if errConvert != nil {
			result.failed = append(result.failed, authUploadFailure{name: failureName, err: errConvert})
			continue
		}
		if _, duplicate := seenFingerprints[converted.fingerprint]; duplicate {
			result.failed = append(result.failed, authUploadFailure{name: failureName, err: fmt.Errorf("duplicate credential record")})
			continue
		}
		seenFingerprints[converted.fingerprint] = struct{}{}

		for {
			filename := reserveImportedAuthFileName(converted.baseFilename, reservedNames)
			errWrite := h.writeNewAuthFile(ctx, filename, converted.payload)
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

func authUploadFileLimitError() error {
	return errAuthUploadFileLimit
}

func decodeSub2APIJSON(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var root any
	if errDecode := decoder.Decode(&root); errDecode != nil {
		return nil, errDecode
	}
	var trailing any
	if errTrailing := decoder.Decode(&trailing); !errors.Is(errTrailing, io.EOF) {
		if errTrailing == nil {
			return nil, fmt.Errorf("multiple JSON documents are not supported")
		}
		return nil, errTrailing
	}
	return root, nil
}

func collectSub2APIAccounts(root any) ([]sub2APIAccountCandidate, bool, error) {
	candidates := make([]sub2APIAccountCandidate, 0)
	handled := containsSub2APIValue(root, 0)
	if !handled {
		return candidates, false, nil
	}
	errCollect := visitSub2APIValue(root, "$", 0, time.Time{}, &candidates, &handled)
	return candidates, handled, errCollect
}

func containsSub2APIValue(value any, depth int) bool {
	if depth > maxSub2APIJSONDepth {
		return true
	}
	switch typed := value.(type) {
	case map[string]any:
		containerType := strings.ToLower(strings.TrimSpace(sub2APIString(typed["type"])))
		if containerType == sub2APIDataType {
			return true
		}
		if _, hasAccounts := typed["accounts"]; hasAccounts && containerType == "" {
			return true
		}
		if looksLikeSub2APIAccount(typed) {
			return true
		}
		for _, child := range typed {
			if containsSub2APIValue(child, depth+1) {
				return true
			}
		}
	case []any:
		for index := range typed {
			if containsSub2APIValue(typed[index], depth+1) {
				return true
			}
		}
	}
	return false
}

func visitSub2APIValue(value any, path string, depth int, inheritedExportedAt time.Time, candidates *[]sub2APIAccountCandidate, handled *bool) error {
	if depth > maxSub2APIJSONDepth {
		return fmt.Errorf("sub2api JSON nesting exceeds %d levels", maxSub2APIJSONDepth)
	}
	switch typed := value.(type) {
	case map[string]any:
		exportedAt := inheritedExportedAt
		if rawExportedAt, exists := typed["exported_at"]; exists {
			parsed, present, errTimestamp := parseSub2APITimestamp(rawExportedAt)
			if errTimestamp != nil {
				return fmt.Errorf("invalid sub2api exported_at: %w", errTimestamp)
			}
			if present {
				exportedAt = parsed
			}
		}

		containerType := strings.ToLower(strings.TrimSpace(sub2APIString(typed["type"])))
		if containerType == sub2APIDataType {
			*handled = true
			version, okVersion := sub2APIInteger(typed["version"])
			if !okVersion || version != sub2APIDataVersion {
				return fmt.Errorf("unsupported sub2api-data version %d", version)
			}
			return appendSub2APIAccounts(typed["accounts"], path+".accounts", exportedAt, candidates)
		}
		if rawAccounts, exists := typed["accounts"]; exists && containerType == "" {
			*handled = true
			return appendSub2APIAccounts(rawAccounts, path+".accounts", exportedAt, candidates)
		}
		if looksLikeSub2APIAccount(typed) {
			*handled = true
			*candidates = append(*candidates, sub2APIAccountCandidate{record: typed, path: path, exportedAt: exportedAt})
			return nil
		}

		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if errVisit := visitSub2APIValue(typed[key], path+"."+key, depth+1, exportedAt, candidates, handled); errVisit != nil {
				return errVisit
			}
		}
	case []any:
		for index := range typed {
			childPath := fmt.Sprintf("%s[%d]", path, index)
			if errVisit := visitSub2APIValue(typed[index], childPath, depth+1, inheritedExportedAt, candidates, handled); errVisit != nil {
				return errVisit
			}
		}
	}
	return nil
}

func appendSub2APIAccounts(value any, path string, exportedAt time.Time, candidates *[]sub2APIAccountCandidate) error {
	accounts, okAccounts := value.([]any)
	if !okAccounts {
		return fmt.Errorf("sub2api accounts must be an array")
	}
	for index := range accounts {
		record, _ := accounts[index].(map[string]any)
		*candidates = append(*candidates, sub2APIAccountCandidate{
			record:     record,
			path:       fmt.Sprintf("%s[%d]", path, index),
			exportedAt: exportedAt,
		})
	}
	return nil
}

func looksLikeSub2APIAccount(record map[string]any) bool {
	if record == nil {
		return false
	}
	if _, okCredentials := record["credentials"].(map[string]any); okCredentials {
		return true
	}
	if _, okCredential := record["credential"].(map[string]any); okCredential {
		return true
	}
	platform := strings.TrimSpace(sub2APIString(record["platform"]))
	return platform != "" && sub2APIFirstString(record,
		[]string{"access_token"}, []string{"accessToken"},
		[]string{"tokens", "access_token"}, []string{"tokens", "accessToken"},
	) != ""
}

func convertSub2APIAccount(candidate sub2APIAccountCandidate, now time.Time) (sub2APIConvertedAuth, error) {
	record := candidate.record
	if record == nil {
		return sub2APIConvertedAuth{}, fmt.Errorf("account must be an object")
	}
	platform := strings.ToLower(sub2APIFirstString(record, []string{"platform"}, []string{"provider"}))
	authType := strings.ToLower(sub2APIFirstString(record, []string{"type"}, []string{"auth_type"}, []string{"authType"}))
	if authType == "" {
		authType = "oauth"
	}
	provider := ""
	switch platform {
	case "openai":
		provider = "codex"
	case "anthropic":
		provider = "claude"
	case "grok", "grok_build", "xai":
		provider = "xai"
	}
	if provider == "" || authType != "oauth" {
		return sub2APIConvertedAuth{}, fmt.Errorf("unsupported account platform/type %q/%q", platform, authType)
	}
	if provider == "codex" && isSub2APIAgentIdentity(record) {
		return convertSub2APIAgentIdentityAccount(record)
	}

	accessToken := sub2APIFirstString(record,
		[]string{"credentials", "access_token"}, []string{"credentials", "accessToken"},
		[]string{"credential", "access_token"}, []string{"credential", "accessToken"},
		[]string{"tokens", "access_token"}, []string{"tokens", "accessToken"},
		[]string{"access_token"}, []string{"accessToken"},
	)
	if accessToken == "" {
		return sub2APIConvertedAuth{}, fmt.Errorf("missing access_token")
	}
	refreshToken := sub2APIFirstString(record,
		[]string{"credentials", "refresh_token"}, []string{"credentials", "refreshToken"},
		[]string{"credential", "refresh_token"}, []string{"credential", "refreshToken"},
		[]string{"tokens", "refresh_token"}, []string{"tokens", "refreshToken"},
		[]string{"refresh_token"}, []string{"refreshToken"},
	)
	idToken := sub2APIFirstString(record,
		[]string{"credentials", "id_token"}, []string{"credentials", "idToken"},
		[]string{"credential", "id_token"}, []string{"credential", "idToken"},
		[]string{"tokens", "id_token"}, []string{"tokens", "idToken"},
		[]string{"id_token"}, []string{"idToken"},
	)
	accessPayload := parseSub2APIJWTPayload(accessToken)
	idPayload := parseSub2APIJWTPayload(idToken)
	accessOpenAIAuth := sub2APIObjectAt(accessPayload, "https://api.openai.com/auth")
	idOpenAIAuth := sub2APIObjectAt(idPayload, "https://api.openai.com/auth")
	accessProfile := sub2APIObjectAt(accessPayload, "https://api.openai.com/profile")

	name := sub2APIFirstString(record, []string{"name"}, []string{"label"})
	email := sub2APIFirstString(record,
		[]string{"credentials", "email"}, []string{"credentials", "email_address"},
		[]string{"credential", "email"}, []string{"credential", "email_address"},
		[]string{"extra", "email"}, []string{"extra", "email_address"},
		[]string{"email"}, []string{"email_address"},
	)
	if email == "" {
		email = firstSub2APIMapString([]map[string]any{idPayload, accessProfile, accessPayload}, "email")
	}
	if email == "" && strings.Contains(name, "@") {
		email = name
	}

	accountID := sub2APIFirstString(record,
		[]string{"credentials", "chatgpt_account_id"}, []string{"credentials", "account_id"}, []string{"credentials", "accountId"},
		[]string{"credential", "chatgpt_account_id"}, []string{"credential", "account_id"}, []string{"credential", "accountId"},
		[]string{"extra", "chatgpt_account_id"}, []string{"extra", "account_id"},
		[]string{"chatgpt_account_id"}, []string{"account_id"}, []string{"accountId"},
	)
	if accountID == "" {
		accountID = firstSub2APIMapString([]map[string]any{accessOpenAIAuth, idOpenAIAuth}, "chatgpt_account_id", "account_id")
	}
	planType := sub2APIFirstString(record,
		[]string{"credentials", "plan_type"}, []string{"credentials", "planType"}, []string{"credentials", "chatgpt_plan_type"},
		[]string{"extra", "plan_type"}, []string{"extra", "chatgpt_plan_type"},
		[]string{"plan_type"}, []string{"planType"}, []string{"chatgpt_plan_type"},
	)
	if planType == "" {
		planType = firstSub2APIMapString([]map[string]any{accessOpenAIAuth, idOpenAIAuth}, "chatgpt_plan_type", "plan_type")
	}
	clientID := sub2APIFirstString(record,
		[]string{"credentials", "client_id"}, []string{"credentials", "clientId"},
		[]string{"extra", "client_id"}, []string{"extra", "clientId"},
		[]string{"client_id"}, []string{"clientId"},
	)
	if clientID == "" {
		clientID = firstSub2APIMapString([]map[string]any{accessPayload, idPayload}, "client_id")
	}
	subject := sub2APIFirstString(record,
		[]string{"credentials", "sub"}, []string{"credential", "sub"},
		[]string{"extra", "sub"}, []string{"sub"},
	)
	if subject == "" {
		subject = firstSub2APIMapString([]map[string]any{accessPayload, idPayload}, "sub")
	}
	accountUUID := sub2APIFirstString(record,
		[]string{"credentials", "account_uuid"}, []string{"credentials", "accountUuid"},
		[]string{"credential", "account_uuid"}, []string{"credential", "accountUuid"},
		[]string{"extra", "account_uuid"}, []string{"extra", "accountUuid"},
		[]string{"account_uuid"}, []string{"accountUuid"},
	)

	expiresIn, hasExpiresIn, errExpiresIn := sub2APIFirstInteger(record,
		[]string{"credentials", "expires_in"}, []string{"credentials", "expiresIn"},
		[]string{"extra", "expires_in"}, []string{"extra", "expiresIn"},
		[]string{"expires_in"}, []string{"expiresIn"},
	)
	if errExpiresIn != nil || (hasExpiresIn && expiresIn < 0) {
		return sub2APIConvertedAuth{}, fmt.Errorf("invalid expires_in")
	}
	expiredAt, hasExpiredAt, errExpiredAt := sub2APIFirstTimestamp(record,
		[]string{"credentials", "expires_at"}, []string{"credentials", "expiresAt"}, []string{"credentials", "expired"},
		[]string{"extra", "expires_at"}, []string{"extra", "expiresAt"}, []string{"extra", "expired"},
		[]string{"expires_at"}, []string{"expiresAt"}, []string{"expires"}, []string{"expired"},
	)
	if errExpiredAt != nil {
		return sub2APIConvertedAuth{}, fmt.Errorf("invalid token expiry: %w", errExpiredAt)
	}
	if !hasExpiredAt {
		if jwtExpiry, okExpiry := sub2APIJWTExpiry(accessPayload); okExpiry {
			expiredAt = jwtExpiry
			hasExpiredAt = true
		}
	}
	lastRefresh, hasLastRefresh, errLastRefresh := sub2APIFirstTimestamp(record,
		[]string{"extra", "last_refresh"}, []string{"extra", "lastRefresh"}, []string{"extra", "last_refreshed_at"},
		[]string{"credentials", "last_refresh"}, []string{"credentials", "lastRefresh"}, []string{"credentials", "last_refreshed_at"},
		[]string{"last_refresh"}, []string{"lastRefresh"}, []string{"last_refreshed_at"},
	)
	if errLastRefresh != nil {
		return sub2APIConvertedAuth{}, fmt.Errorf("invalid last_refresh: %w", errLastRefresh)
	}
	if !hasLastRefresh && hasExpiredAt && hasExpiresIn && expiresIn > 0 {
		lastRefresh = expiredAt.Add(-time.Duration(expiresIn) * time.Second)
		hasLastRefresh = true
	}
	if !hasLastRefresh && !candidate.exportedAt.IsZero() {
		lastRefresh = candidate.exportedAt
		hasLastRefresh = true
	}
	if !hasLastRefresh {
		lastRefresh = now
	}

	identity := email
	if identity == "" && provider == "codex" {
		identity = accountID
	}
	if identity == "" && provider == "xai" {
		identity = subject
	}
	if identity == "" && provider == "claude" {
		identity = accountUUID
	}
	if identity == "" {
		identity = name
	}
	filenameToken := sanitizeAuthFileToken(identity)
	if filenameToken == "" {
		return sub2APIConvertedAuth{}, fmt.Errorf("account has no usable email, subject, account id, account UUID, or name")
	}
	baseFilename := provider + "-" + filenameToken + ".json"

	native := map[string]any{
		"type":          provider,
		"access_token":  accessToken,
		"refresh_token": refreshToken,
		"last_refresh":  lastRefresh.UTC().Format(time.RFC3339),
		"disabled":      false,
	}
	if email != "" {
		native["email"] = email
	}
	if idToken != "" {
		native["id_token"] = idToken
	}
	if hasExpiredAt {
		native["expired"] = expiredAt.UTC().Format(time.RFC3339)
	}
	if name != "" && provider == "codex" {
		native["name"] = name
	}
	switch provider {
	case "codex":
		setSub2APIString(native, "account_id", accountID)
		setSub2APIString(native, "chatgpt_account_id", accountID)
		setSub2APIString(native, "workspace_id", accountID)
		setSub2APIString(native, "client_id", clientID)
		setSub2APIString(native, "plan_type", planType)
		setSub2APIString(native, "chatgpt_plan_type", planType)
		setSub2APIString(native, "session_token", sub2APIFirstString(record,
			[]string{"credentials", "session_token"}, []string{"credential", "session_token"}, []string{"session_token"},
		))
	case "xai":
		native["auth_kind"] = "oauth"
		setSub2APIString(native, "sub", subject)
		setSub2APIString(native, "token_type", sub2APIFirstString(record,
			[]string{"credentials", "token_type"}, []string{"credential", "token_type"}, []string{"token_type"},
		))
		if hasExpiresIn && expiresIn > 0 {
			native["expires_in"] = expiresIn
		}
		baseURL := sub2APIFirstString(record,
			[]string{"credentials", "base_url"}, []string{"credential", "base_url"},
			[]string{"extra", "base_url"}, []string{"base_url"},
		)
		if baseURL == "" {
			baseURL = xaiauth.DefaultAPIBaseURL
		}
		native["base_url"] = baseURL
		setSub2APIString(native, "redirect_uri", sub2APIFirstString(record,
			[]string{"credentials", "redirect_uri"}, []string{"credential", "redirect_uri"},
			[]string{"extra", "redirect_uri"}, []string{"redirect_uri"},
		))
		tokenEndpoint := sub2APIFirstString(record,
			[]string{"credentials", "token_endpoint"}, []string{"credential", "token_endpoint"},
			[]string{"extra", "token_endpoint"}, []string{"token_endpoint"},
		)
		if tokenEndpoint != "" {
			validatedEndpoint, errValidate := xaiauth.ValidateOAuthEndpoint(tokenEndpoint, "token_endpoint")
			if errValidate != nil {
				return sub2APIConvertedAuth{baseFilename: baseFilename}, fmt.Errorf("invalid xai token_endpoint: %w", errValidate)
			}
			native["token_endpoint"] = validatedEndpoint
		}
	}
	if errConfig := copySub2APIConfiguration(record, native); errConfig != nil {
		return sub2APIConvertedAuth{baseFilename: baseFilename}, errConfig
	}

	payload, errMarshal := json.MarshalIndent(native, "", "  ")
	if errMarshal != nil {
		return sub2APIConvertedAuth{baseFilename: baseFilename}, fmt.Errorf("encode %s auth file: %w", provider, errMarshal)
	}
	fingerprintIdentity := accountID
	switch provider {
	case "claude":
		fingerprintIdentity = accountUUID
	case "xai":
		fingerprintIdentity = subject
	}
	fingerprint := sha256.Sum256([]byte(accessToken + "\x00" + fingerprintIdentity))
	return sub2APIConvertedAuth{
		baseFilename: baseFilename,
		payload:      append(payload, '\n'),
		fingerprint:  fingerprint,
	}, nil
}

func isSub2APIAgentIdentity(record map[string]any) bool {
	authMode := strings.ToLower(sub2APIFirstString(record,
		[]string{"credentials", "auth_mode"}, []string{"credentials", "authMode"},
		[]string{"credential", "auth_mode"}, []string{"credential", "authMode"},
		[]string{"auth_mode"}, []string{"authMode"},
	))
	switch authMode {
	case "agentidentity", "agent_identity", "agent-identity":
		return true
	default:
		return false
	}
}

func convertSub2APIAgentIdentityAccount(record map[string]any) (sub2APIConvertedAuth, error) {
	privateKey := sub2APIFirstString(record,
		[]string{"credentials", "agent_private_key"}, []string{"credentials", "agentPrivateKey"},
		[]string{"credential", "agent_private_key"}, []string{"credential", "agentPrivateKey"},
		[]string{"agent_private_key"}, []string{"agentPrivateKey"},
	)
	runtimeID := sub2APIFirstString(record,
		[]string{"credentials", "agent_runtime_id"}, []string{"credentials", "agentRuntimeId"},
		[]string{"credential", "agent_runtime_id"}, []string{"credential", "agentRuntimeId"},
		[]string{"agent_runtime_id"}, []string{"agentRuntimeId"},
	)
	taskID := sub2APIFirstString(record,
		[]string{"credentials", "task_id"}, []string{"credentials", "taskId"},
		[]string{"credential", "task_id"}, []string{"credential", "taskId"},
		[]string{"task_id"}, []string{"taskId"},
	)
	if privateKey == "" || runtimeID == "" || taskID == "" {
		return sub2APIConvertedAuth{}, fmt.Errorf("missing agent_private_key, agent_runtime_id, or task_id")
	}

	name := sub2APIFirstString(record, []string{"name"}, []string{"label"})
	email := sub2APIFirstString(record,
		[]string{"credentials", "email"}, []string{"credentials", "email_address"},
		[]string{"credential", "email"}, []string{"credential", "email_address"},
		[]string{"extra", "email"}, []string{"extra", "email_address"},
		[]string{"email"}, []string{"email_address"},
	)
	if email == "" && strings.Contains(name, "@") {
		email = name
	}
	accountID := sub2APIFirstString(record,
		[]string{"credentials", "chatgpt_account_id"}, []string{"credentials", "account_id"}, []string{"credentials", "accountId"},
		[]string{"credential", "chatgpt_account_id"}, []string{"credential", "account_id"}, []string{"credential", "accountId"},
		[]string{"extra", "chatgpt_account_id"}, []string{"extra", "account_id"},
		[]string{"chatgpt_account_id"}, []string{"account_id"}, []string{"accountId"},
	)
	identity := email
	if identity == "" {
		identity = accountID
	}
	if identity == "" {
		identity = runtimeID
	}
	if identity == "" {
		identity = name
	}
	filenameToken := sanitizeAuthFileToken(identity)
	if filenameToken == "" {
		return sub2APIConvertedAuth{}, fmt.Errorf("account has no usable email, account id, runtime id, or name")
	}
	baseFilename := "codex-" + filenameToken + ".json"

	native := map[string]any{
		"type":              "codex",
		"auth_kind":         coreauth.AuthKindAgentIdentity,
		"agent_private_key": privateKey,
		"agent_runtime_id":  runtimeID,
		"task_id":           taskID,
		"disabled":          false,
	}
	setSub2APIString(native, "name", name)
	setSub2APIString(native, "email", email)
	setSub2APIString(native, "account_id", accountID)
	setSub2APIString(native, "chatgpt_account_id", accountID)
	setSub2APIString(native, "workspace_id", accountID)
	setSub2APIString(native, "chatgpt_user_id", sub2APIFirstString(record,
		[]string{"credentials", "chatgpt_user_id"}, []string{"credentials", "user_id"},
		[]string{"credential", "chatgpt_user_id"}, []string{"credential", "user_id"},
		[]string{"chatgpt_user_id"}, []string{"user_id"},
	))
	planType := sub2APIFirstString(record,
		[]string{"credentials", "plan_type"}, []string{"credentials", "planType"}, []string{"credentials", "chatgpt_plan_type"},
		[]string{"extra", "plan_type"}, []string{"extra", "chatgpt_plan_type"},
		[]string{"plan_type"}, []string{"planType"}, []string{"chatgpt_plan_type"},
	)
	setSub2APIString(native, "plan_type", planType)
	setSub2APIString(native, "chatgpt_plan_type", planType)
	if errConfig := copySub2APIConfiguration(record, native); errConfig != nil {
		return sub2APIConvertedAuth{baseFilename: baseFilename}, errConfig
	}

	payload, errMarshal := json.MarshalIndent(native, "", "  ")
	if errMarshal != nil {
		return sub2APIConvertedAuth{baseFilename: baseFilename}, fmt.Errorf("encode codex agent identity auth file: %w", errMarshal)
	}
	fingerprint := sha256.Sum256([]byte(privateKey + "\x00" + runtimeID + "\x00" + taskID))
	return sub2APIConvertedAuth{
		baseFilename: baseFilename,
		payload:      append(payload, '\n'),
		fingerprint:  fingerprint,
	}, nil
}

func copySub2APIConfiguration(record, native map[string]any) error {
	if disabled, present, errBool := sub2APIFirstBool(record, []string{"disabled"}); errBool != nil {
		return fmt.Errorf("invalid disabled")
	} else if present {
		native["disabled"] = disabled
	}
	if priority, present, errPriority := sub2APIFirstInteger(record, []string{"priority"}); errPriority != nil {
		return fmt.Errorf("invalid priority")
	} else if present {
		native["priority"] = priority
	}
	for _, field := range []string{"note", "prefix", "proxy_url"} {
		setSub2APIString(native, field, sub2APIFirstString(record, []string{field}))
	}
	if websockets, present, errBool := sub2APIFirstBool(record, []string{"websockets"}); errBool != nil {
		return fmt.Errorf("invalid websockets")
	} else if present {
		native["websockets"] = websockets
	}
	headers := sub2APIFirstStringMap(record,
		[]string{"headers"}, []string{"extra", "headers"}, []string{"credentials", "headers"},
	)
	if len(headers) > 0 {
		native["headers"] = headers
	}
	return nil
}

func reserveImportedAuthFileName(baseName string, reserved map[string]struct{}) string {
	baseName = filepath.Base(strings.TrimSpace(baseName))
	stem := strings.TrimSuffix(baseName, filepath.Ext(baseName))
	candidate := baseName
	for suffix := 2; ; suffix++ {
		key := strings.ToLower(candidate)
		if _, exists := reserved[key]; !exists {
			reserved[key] = struct{}{}
			return candidate
		}
		candidate = fmt.Sprintf("%s-%d.json", stem, suffix)
	}
}

func sanitizeAuthFileToken(value string) string {
	value = strings.TrimSpace(value)
	var builder strings.Builder
	lastSeparator := false
	for _, character := range value {
		allowed := character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			strings.ContainsRune("@._+-", character)
		if allowed {
			builder.WriteRune(character)
			lastSeparator = false
		} else if !lastSeparator && builder.Len() > 0 {
			builder.WriteByte('-')
			lastSeparator = true
		}
		if builder.Len() >= 160 {
			break
		}
	}
	return strings.Trim(builder.String(), "-.")
}

func sub2APIValueAt(record map[string]any, path ...string) any {
	var current any = record
	for _, key := range path {
		object, okObject := current.(map[string]any)
		if !okObject {
			return nil
		}
		current, okObject = object[key]
		if !okObject {
			return nil
		}
	}
	return current
}

func sub2APIFirstString(record map[string]any, paths ...[]string) string {
	for _, path := range paths {
		if value := sub2APIString(sub2APIValueAt(record, path...)); value != "" {
			return value
		}
	}
	return ""
}

func sub2APIString(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return strings.TrimSpace(typed.String())
	default:
		return ""
	}
}

func sub2APIFirstInteger(record map[string]any, paths ...[]string) (int64, bool, error) {
	for _, path := range paths {
		value := sub2APIValueAt(record, path...)
		if value == nil {
			continue
		}
		if raw, okString := value.(string); okString && strings.TrimSpace(raw) == "" {
			continue
		}
		integer, okInteger := sub2APIInteger(value)
		if !okInteger {
			return 0, true, fmt.Errorf("not an integer")
		}
		return integer, true, nil
	}
	return 0, false, nil
}

func sub2APIInteger(value any) (int64, bool) {
	switch typed := value.(type) {
	case json.Number:
		if integer, errInteger := typed.Int64(); errInteger == nil {
			return integer, true
		}
		floating, errFloat := typed.Float64()
		if errFloat != nil || math.Trunc(floating) != floating || floating > math.MaxInt64 || floating < math.MinInt64 {
			return 0, false
		}
		return int64(floating), true
	case float64:
		if math.Trunc(typed) != typed || typed > math.MaxInt64 || typed < math.MinInt64 {
			return 0, false
		}
		return int64(typed), true
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case string:
		integer, errParse := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		return integer, errParse == nil
	default:
		return 0, false
	}
}

func sub2APIFirstBool(record map[string]any, paths ...[]string) (bool, bool, error) {
	for _, path := range paths {
		value := sub2APIValueAt(record, path...)
		if value == nil {
			continue
		}
		switch typed := value.(type) {
		case bool:
			return typed, true, nil
		case string:
			if strings.TrimSpace(typed) == "" {
				continue
			}
			parsed, errParse := strconv.ParseBool(strings.TrimSpace(typed))
			if errParse != nil {
				return false, true, errParse
			}
			return parsed, true, nil
		default:
			return false, true, fmt.Errorf("not a boolean")
		}
	}
	return false, false, nil
}

func sub2APIFirstTimestamp(record map[string]any, paths ...[]string) (time.Time, bool, error) {
	for _, path := range paths {
		value := sub2APIValueAt(record, path...)
		if value == nil {
			continue
		}
		parsed, present, errTimestamp := parseSub2APITimestamp(value)
		if errTimestamp != nil {
			return time.Time{}, true, errTimestamp
		}
		if present {
			return parsed, true, nil
		}
	}
	return time.Time{}, false, nil
}

func parseSub2APITimestamp(value any) (time.Time, bool, error) {
	if integer, okInteger := sub2APIInteger(value); okInteger {
		if integer <= 0 {
			return time.Time{}, true, fmt.Errorf("timestamp must be positive")
		}
		if integer > sub2APIMillisCutover {
			return time.UnixMilli(integer).UTC(), true, nil
		}
		return time.Unix(integer, 0).UTC(), true, nil
	}
	raw, okString := value.(string)
	if okString && strings.TrimSpace(raw) == "" {
		return time.Time{}, false, nil
	}
	if !okString {
		return time.Time{}, true, fmt.Errorf("timestamp must be Unix seconds, Unix milliseconds, or RFC3339")
	}
	parsed, errParse := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw))
	if errParse != nil {
		return time.Time{}, true, fmt.Errorf("timestamp must be Unix seconds, Unix milliseconds, or RFC3339")
	}
	return parsed.UTC(), true, nil
}

func parseSub2APIJWTPayload(token string) map[string]any {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 || parts[1] == "" {
		return nil
	}
	decoded, errDecode := base64.RawURLEncoding.DecodeString(parts[1])
	if errDecode != nil {
		decoded, errDecode = base64.URLEncoding.DecodeString(parts[1])
	}
	if errDecode != nil {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.UseNumber()
	var payload map[string]any
	if errJSON := decoder.Decode(&payload); errJSON != nil {
		return nil
	}
	return payload
}

func sub2APIJWTExpiry(payload map[string]any) (time.Time, bool) {
	if payload == nil {
		return time.Time{}, false
	}
	seconds, okSeconds := sub2APIInteger(payload["exp"])
	if !okSeconds || seconds <= 0 {
		return time.Time{}, false
	}
	return time.Unix(seconds, 0).UTC(), true
}

func sub2APIObjectAt(record map[string]any, path ...string) map[string]any {
	object, _ := sub2APIValueAt(record, path...).(map[string]any)
	return object
}

func firstSub2APIMapString(objects []map[string]any, keys ...string) string {
	for _, object := range objects {
		for _, key := range keys {
			if value := sub2APIString(object[key]); value != "" {
				return value
			}
		}
	}
	return ""
}

func sub2APIFirstStringMap(record map[string]any, paths ...[]string) map[string]string {
	for _, path := range paths {
		object, okObject := sub2APIValueAt(record, path...).(map[string]any)
		if !okObject {
			continue
		}
		out := make(map[string]string, len(object))
		for key, rawValue := range object {
			name := strings.TrimSpace(key)
			value := sub2APIString(rawValue)
			if name != "" && value != "" {
				out[name] = value
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

func setSub2APIString(document map[string]any, key, value string) {
	if value := strings.TrimSpace(value); value != "" {
		document[key] = value
	}
}

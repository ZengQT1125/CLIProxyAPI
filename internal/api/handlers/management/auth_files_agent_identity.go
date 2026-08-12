package management

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/box"
)

const (
	codexAgentIdentityAuthAPIBaseURL    = "https://auth.openai.com/api/accounts"
	codexAgentIdentityVersion           = "0.138.0-alpha.6"
	codexAgentIdentityHarnessID         = "codex-cli"
	codexAgentIdentityRunningLocation   = "local"
	maxCodexAgentIdentityImportAccounts = 100
	maxCodexAgentIdentityImportBody     = 1 << 20
)

var codexAgentIdentityBaseURL = codexAgentIdentityAuthAPIBaseURL

type codexAgentIdentityImportRequest struct {
	AccessTokens []string `json:"access_tokens"`
}

type codexAgentIdentityImportFailure struct {
	Email string `json:"email,omitempty"`
	Error string `json:"error"`
}

type codexAgentIdentityImportResponse struct {
	Status   string                            `json:"status"`
	Imported int                               `json:"imported"`
	Files    []string                          `json:"files"`
	Failed   []codexAgentIdentityImportFailure `json:"failed"`
}

type codexAgentIdentityAccount struct {
	AccessToken string
	AccountID   string
	UserID      string
	Email       string
	PlanType    string
	FedRAMP     bool
}

type codexAgentIdentityClaims struct {
	ExpiresAt int64 `json:"exp"`
	AuthInfo  struct {
		AccountID string `json:"chatgpt_account_id"`
		UserID    string `json:"chatgpt_user_id"`
		PlanType  string `json:"chatgpt_plan_type"`
		FedRAMP   bool   `json:"chatgpt_account_is_fedramp"`
	} `json:"https://api.openai.com/auth"`
	Profile struct {
		Email string `json:"email"`
	} `json:"https://api.openai.com/profile"`
}

type codexAgentIdentityRegistrationRequest struct {
	ABOM struct {
		AgentVersion    string `json:"agent_version"`
		AgentHarnessID  string `json:"agent_harness_id"`
		RunningLocation string `json:"running_location"`
	} `json:"abom"`
	AgentPublicKey string `json:"agent_public_key"`
}

type codexAgentIdentityRegistrationResponse struct {
	AgentRuntimeID string `json:"agent_runtime_id"`
}

type codexAgentTaskRegistrationRequest struct {
	Timestamp string `json:"timestamp"`
	Signature string `json:"signature"`
}

type codexAgentTaskRegistrationResponse struct {
	TaskID               string `json:"task_id"`
	TaskIDCamel          string `json:"taskId"`
	EncryptedTaskID      string `json:"encrypted_task_id"`
	EncryptedTaskIDCamel string `json:"encryptedTaskId"`
}

// ImportCodexAgentIdentity converts ChatGPT access tokens into native Codex
// Agent Identity auth files. Tokens and generated private keys are never returned.
func (h *Handler) ImportCodexAgentIdentity(c *gin.Context) {
	if h == nil || h.authManager == nil || h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth service unavailable"})
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxCodexAgentIdentityImportBody)
	var body codexAgentIdentityImportRequest
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if errDecode := decoder.Decode(&body); errDecode != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if len(body.AccessTokens) == 0 || len(body.AccessTokens) > maxCodexAgentIdentityImportAccounts {
		c.JSON(http.StatusBadRequest, gin.H{"error": "access_tokens must contain 1 to 100 items"})
		return
	}

	result := codexAgentIdentityImportResponse{
		Status: "ok",
		Files:  make([]string, 0, len(body.AccessTokens)),
		Failed: make([]codexAgentIdentityImportFailure, 0),
	}
	seenEmails := make(map[string]struct{}, len(body.AccessTokens))
	for _, rawToken := range body.AccessTokens {
		account, errAccount := parseCodexAgentIdentityAccount(rawToken, time.Now())
		if errAccount != nil {
			result.Failed = append(result.Failed, codexAgentIdentityImportFailure{Error: "invalid_access_token"})
			continue
		}
		if _, duplicate := seenEmails[account.Email]; duplicate {
			result.Failed = append(result.Failed, codexAgentIdentityImportFailure{
				Email: account.Email,
				Error: "duplicate_account",
			})
			continue
		}
		seenEmails[account.Email] = struct{}{}

		fileName, payload, errBuild := h.buildCodexAgentIdentityCredential(c.Request.Context(), account)
		if errBuild != nil {
			result.Failed = append(result.Failed, codexAgentIdentityImportFailure{
				Email: account.Email,
				Error: "registration_failed",
			})
			continue
		}
		if errWrite := h.writeAuthFile(c.Request.Context(), fileName, payload); errWrite != nil {
			result.Failed = append(result.Failed, codexAgentIdentityImportFailure{
				Email: account.Email,
				Error: "persist_failed",
			})
			continue
		}
		result.Imported++
		result.Files = append(result.Files, fileName)
	}

	if len(result.Failed) > 0 {
		result.Status = "partial"
	}
	c.JSON(http.StatusOK, result)
}

func parseCodexAgentIdentityAccount(rawToken string, now time.Time) (codexAgentIdentityAccount, error) {
	token := strings.TrimSpace(strings.Trim(strings.TrimSpace(rawToken), "\"'"))
	if strings.HasPrefix(strings.ToLower(token), "bearer ") {
		token = strings.TrimSpace(token[7:])
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[1] == "" {
		return codexAgentIdentityAccount{}, fmt.Errorf("invalid access token")
	}
	payload, errDecode := base64.RawURLEncoding.DecodeString(parts[1])
	if errDecode != nil {
		return codexAgentIdentityAccount{}, fmt.Errorf("decode access token: %w", errDecode)
	}
	var claims codexAgentIdentityClaims
	if errJSON := json.Unmarshal(payload, &claims); errJSON != nil {
		return codexAgentIdentityAccount{}, fmt.Errorf("decode access token claims: %w", errJSON)
	}
	if claims.ExpiresAt > 0 && claims.ExpiresAt <= now.Unix() {
		return codexAgentIdentityAccount{}, fmt.Errorf("access token expired")
	}

	email := strings.ToLower(strings.TrimSpace(claims.Profile.Email))
	accountID := strings.TrimSpace(claims.AuthInfo.AccountID)
	userID := strings.TrimSpace(claims.AuthInfo.UserID)
	if email == "" || accountID == "" || userID == "" {
		return codexAgentIdentityAccount{}, fmt.Errorf("access token is missing account claims")
	}
	planType := strings.TrimSpace(claims.AuthInfo.PlanType)
	if planType == "" {
		planType = "free"
	}
	return codexAgentIdentityAccount{
		AccessToken: token,
		AccountID:   accountID,
		UserID:      userID,
		Email:       email,
		PlanType:    planType,
		FedRAMP:     claims.AuthInfo.FedRAMP,
	}, nil
}

func (h *Handler) buildCodexAgentIdentityCredential(ctx context.Context, account codexAgentIdentityAccount) (string, []byte, error) {
	publicKey, privateKey, errKey := ed25519.GenerateKey(rand.Reader)
	if errKey != nil {
		return "", nil, fmt.Errorf("generate agent identity key: %w", errKey)
	}
	privateKeyDER, errMarshalKey := x509.MarshalPKCS8PrivateKey(privateKey)
	if errMarshalKey != nil {
		return "", nil, fmt.Errorf("marshal agent identity key: %w", errMarshalKey)
	}
	privateKeyBase64 := base64.StdEncoding.EncodeToString(privateKeyDER)
	publicKeySSH := encodeCodexAgentIdentityPublicKey(publicKey)

	runtimeID, errRegister := h.registerCodexAgentIdentity(ctx, account, publicKeySSH)
	if errRegister != nil {
		return "", nil, errRegister
	}
	taskID, errTask := h.registerCodexAgentTask(ctx, runtimeID, privateKey)
	if errTask != nil {
		return "", nil, errTask
	}

	credential := map[string]any{
		"type":                       "codex",
		"auth_kind":                  "agent_identity",
		"auth_mode":                  "agent_identity",
		"agent_runtime_id":           runtimeID,
		"agent_private_key":          privateKeyBase64,
		"task_id":                    taskID,
		"account_id":                 account.AccountID,
		"chatgpt_account_id":         account.AccountID,
		"chatgpt_user_id":            account.UserID,
		"email":                      account.Email,
		"plan_type":                  account.PlanType,
		"chatgpt_plan_type":          account.PlanType,
		"chatgpt_account_is_fedramp": account.FedRAMP,
		"disabled":                   false,
	}
	payload, errMarshal := json.MarshalIndent(credential, "", "  ")
	if errMarshal != nil {
		return "", nil, fmt.Errorf("marshal agent identity credential: %w", errMarshal)
	}
	payload = append(payload, '\n')
	return codexAgentIdentityFileName(account.Email), payload, nil
}

func (h *Handler) registerCodexAgentIdentity(ctx context.Context, account codexAgentIdentityAccount, publicKeySSH string) (string, error) {
	requestBody := codexAgentIdentityRegistrationRequest{AgentPublicKey: publicKeySSH}
	requestBody.ABOM.AgentVersion = codexAgentIdentityVersion
	requestBody.ABOM.AgentHarnessID = codexAgentIdentityHarnessID
	requestBody.ABOM.RunningLocation = codexAgentIdentityRunningLocation

	var response codexAgentIdentityRegistrationResponse
	if errRequest := h.sendCodexAgentIdentityRequest(ctx, http.MethodPost, codexAgentIdentityURL("v1", "agent", "register"), account.AccessToken, account.FedRAMP, requestBody, &response); errRequest != nil {
		return "", fmt.Errorf("register agent identity: %w", errRequest)
	}
	runtimeID := strings.TrimSpace(response.AgentRuntimeID)
	if runtimeID == "" {
		return "", fmt.Errorf("register agent identity: missing runtime id")
	}
	return runtimeID, nil
}

func (h *Handler) registerCodexAgentTask(ctx context.Context, runtimeID string, privateKey ed25519.PrivateKey) (string, error) {
	timestamp := time.Now().UTC().Truncate(time.Second).Format(time.RFC3339)
	signature := ed25519.Sign(privateKey, []byte(runtimeID+":"+timestamp))
	requestBody := codexAgentTaskRegistrationRequest{
		Timestamp: timestamp,
		Signature: base64.StdEncoding.EncodeToString(signature),
	}
	var response codexAgentTaskRegistrationResponse
	if errRequest := h.sendCodexAgentIdentityRequest(ctx, http.MethodPost, codexAgentIdentityURL("v1", "agent", runtimeID, "task", "register"), "", false, requestBody, &response); errRequest != nil {
		return "", fmt.Errorf("register agent task: %w", errRequest)
	}
	for _, taskID := range []string{response.TaskID, response.TaskIDCamel} {
		if taskID = strings.TrimSpace(taskID); taskID != "" {
			return taskID, nil
		}
	}
	for _, encryptedTaskID := range []string{response.EncryptedTaskID, response.EncryptedTaskIDCamel} {
		if encryptedTaskID = strings.TrimSpace(encryptedTaskID); encryptedTaskID != "" {
			return decryptCodexAgentTaskID(privateKey, encryptedTaskID)
		}
	}
	return "", fmt.Errorf("register agent task: missing task id")
}

func (h *Handler) sendCodexAgentIdentityRequest(ctx context.Context, method, requestURL, accessToken string, fedRAMP bool, requestBody, responseBody any) error {
	payload, errMarshal := json.Marshal(requestBody)
	if errMarshal != nil {
		return fmt.Errorf("marshal request: %w", errMarshal)
	}
	request, errRequest := http.NewRequestWithContext(ctx, method, requestURL, strings.NewReader(string(payload)))
	if errRequest != nil {
		return fmt.Errorf("build request: %w", errRequest)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	if accessToken != "" {
		request.Header.Set("Authorization", "Bearer "+accessToken)
	}
	if fedRAMP {
		request.Header.Set("X-OpenAI-Fedramp", "true")
	}

	client := &http.Client{Timeout: 30 * time.Second, Transport: h.apiCallTransport(nil, "")}
	response, errDo := client.Do(request)
	if errDo != nil {
		return fmt.Errorf("request failed")
	}
	defer func() {
		if errClose := response.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("close Codex agent identity response body")
		}
	}()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("upstream status %d", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if errDecode := decoder.Decode(responseBody); errDecode != nil {
		return fmt.Errorf("decode response: %w", errDecode)
	}
	return nil
}

func codexAgentIdentityURL(parts ...string) string {
	base, errParse := url.Parse(codexAgentIdentityBaseURL)
	if errParse != nil {
		return codexAgentIdentityBaseURL
	}
	pathParts := append([]string{base.Path}, parts...)
	base.Path = path.Join(pathParts...)
	return base.String()
}

func encodeCodexAgentIdentityPublicKey(publicKey ed25519.PublicKey) string {
	keyType := []byte("ssh-ed25519")
	blob := make([]byte, 0, 4+len(keyType)+4+len(publicKey))
	blob = appendSSHString(blob, keyType)
	blob = appendSSHString(blob, publicKey)
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob)
}

func appendSSHString(dst, value []byte) []byte {
	length := make([]byte, 4)
	binary.BigEndian.PutUint32(length, uint32(len(value)))
	dst = append(dst, length...)
	return append(dst, value...)
}

func decryptCodexAgentTaskID(privateKey ed25519.PrivateKey, encryptedTaskID string) (string, error) {
	ciphertext, errDecode := base64.StdEncoding.DecodeString(encryptedTaskID)
	if errDecode != nil {
		return "", fmt.Errorf("decode encrypted task id: %w", errDecode)
	}
	digest := sha512.Sum512(privateKey.Seed())
	var curvePrivate [32]byte
	copy(curvePrivate[:], digest[:32])
	curvePrivate[0] &= 248
	curvePrivate[31] &= 127
	curvePrivate[31] |= 64
	curvePublicBytes, errPublic := curve25519.X25519(curvePrivate[:], curve25519.Basepoint)
	if errPublic != nil {
		return "", fmt.Errorf("derive task decryption key: %w", errPublic)
	}
	var curvePublic [32]byte
	copy(curvePublic[:], curvePublicBytes)
	plaintext, ok := box.OpenAnonymous(nil, ciphertext, &curvePublic, &curvePrivate)
	if !ok {
		return "", fmt.Errorf("decrypt task id")
	}
	taskID := strings.TrimSpace(string(plaintext))
	if taskID == "" {
		return "", fmt.Errorf("decrypted task id is empty")
	}
	return taskID, nil
}

func codexAgentIdentityFileName(email string) string {
	safeEmail := strings.Map(func(character rune) rune {
		switch {
		case character >= 'a' && character <= 'z':
			return character
		case character >= '0' && character <= '9':
			return character
		case strings.ContainsRune("@._+-", character):
			return character
		default:
			return '-'
		}
	}, strings.ToLower(strings.TrimSpace(email)))
	safeEmail = strings.Trim(safeEmail, "-")
	return "codex-" + safeEmail + ".json"
}

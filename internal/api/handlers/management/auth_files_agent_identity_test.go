package management

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/box"
)

func TestImportCodexAgentIdentityPersistsNativeCredential(t *testing.T) {
	gin.SetMode(gin.TestMode)
	accessToken := codexAgentIdentityTestToken(t, "user@example.com", time.Now().Add(time.Hour))
	var registeredPublicKey ed25519.PublicKey

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agent/register":
			if got := r.Header.Get("Authorization"); got != "Bearer "+accessToken {
				t.Errorf("Authorization = %q, want bearer access token", got)
			}
			var request codexAgentIdentityRegistrationRequest
			if errDecode := json.NewDecoder(r.Body).Decode(&request); errDecode != nil {
				t.Errorf("decode agent registration request: %v", errDecode)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if request.ABOM.AgentHarnessID != codexAgentIdentityHarnessID || request.ABOM.RunningLocation != codexAgentIdentityRunningLocation {
				t.Errorf("ABOM = %#v", request.ABOM)
			}
			registeredPublicKey = decodeCodexAgentIdentitySSHPublicKey(t, request.AgentPublicKey)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"agent_runtime_id":"agent-runtime-id"}`))
		case "/v1/agent/agent-runtime-id/task/register":
			var request codexAgentTaskRegistrationRequest
			if errDecode := json.NewDecoder(r.Body).Decode(&request); errDecode != nil {
				t.Errorf("decode task registration request: %v", errDecode)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			signature, errSignature := base64.StdEncoding.DecodeString(request.Signature)
			if errSignature != nil {
				t.Errorf("decode task signature: %v", errSignature)
			}
			if !ed25519.Verify(registeredPublicKey, []byte("agent-runtime-id:"+request.Timestamp), signature) {
				t.Error("task registration signature is invalid")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"task_id":"task-runtime-id"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	originalBaseURL := codexAgentIdentityBaseURL
	codexAgentIdentityBaseURL = upstream.URL
	t.Cleanup(func() { codexAgentIdentityBaseURL = originalBaseURL })

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	body, errMarshal := json.Marshal(codexAgentIdentityImportRequest{AccessTokens: []string{accessToken}})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/custom/codex-agent-identity/import", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ImportCodexAgentIdentity(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), accessToken) || strings.Contains(recorder.Body.String(), "agent_private_key") {
		t.Fatal("response exposed credential material")
	}
	var response codexAgentIdentityImportResponse
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &response); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if response.Imported != 1 || response.Status != "ok" || len(response.Files) != 1 || response.Files[0] != "codex-user@example.com.json" {
		t.Fatalf("response = %#v", response)
	}

	credentialPath := filepath.Join(authDir, "codex-user@example.com.json")
	credentialBytes, errRead := os.ReadFile(credentialPath)
	if errRead != nil {
		t.Fatalf("read credential: %v", errRead)
	}
	if strings.Contains(string(credentialBytes), accessToken) {
		t.Fatal("persisted credential contains access token")
	}
	var credential map[string]any
	if errDecode := json.Unmarshal(credentialBytes, &credential); errDecode != nil {
		t.Fatalf("decode credential: %v", errDecode)
	}
	wantStrings := map[string]string{
		"type":               "codex",
		"auth_kind":          "agent_identity",
		"auth_mode":          "agent_identity",
		"agent_runtime_id":   "agent-runtime-id",
		"task_id":            "task-runtime-id",
		"account_id":         "account-id",
		"chatgpt_account_id": "account-id",
		"chatgpt_user_id":    "user-id",
		"email":              "user@example.com",
		"plan_type":          "plus",
		"chatgpt_plan_type":  "plus",
	}
	for key, want := range wantStrings {
		if got, _ := credential[key].(string); got != want {
			t.Errorf("credential[%q] = %q, want %q", key, got, want)
		}
	}
	privateKeyBase64, _ := credential["agent_private_key"].(string)
	privateKeyDER, errBase64 := base64.StdEncoding.DecodeString(privateKeyBase64)
	if errBase64 != nil || len(privateKeyDER) != 48 {
		t.Fatalf("agent_private_key is not PKCS#8 Ed25519: len=%d err=%v", len(privateKeyDER), errBase64)
	}
	auth, okAuth := manager.GetByID("codex-user@example.com.json")
	if !okAuth || auth.Provider != "codex" || auth.AuthKind() != coreauth.AuthKindAgentIdentity {
		t.Fatalf("runtime auth = %#v", auth)
	}
}

func TestImportCodexAgentIdentityDoesNotExposeUpstreamFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	accessToken := codexAgentIdentityTestToken(t, "failure@example.com", time.Now().Add(time.Hour))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"secret upstream detail"}`))
	}))
	defer upstream.Close()

	originalBaseURL := codexAgentIdentityBaseURL
	codexAgentIdentityBaseURL = upstream.URL
	t.Cleanup(func() { codexAgentIdentityBaseURL = originalBaseURL })

	handler := NewHandlerWithoutConfigFilePath(
		&config.Config{AuthDir: t.TempDir()},
		coreauth.NewManager(nil, nil, nil),
	)
	body, _ := json.Marshal(codexAgentIdentityImportRequest{AccessTokens: []string{accessToken}})
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/custom/codex-agent-identity/import", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ImportCodexAgentIdentity(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), accessToken) || strings.Contains(recorder.Body.String(), "secret upstream detail") {
		t.Fatalf("response exposed sensitive upstream data: %s", recorder.Body.String())
	}
	var response codexAgentIdentityImportResponse
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &response); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if response.Imported != 0 || response.Status != "partial" || len(response.Failed) != 1 || response.Failed[0].Error != "registration_failed" {
		t.Fatalf("response = %#v", response)
	}
}

func TestImportCodexAgentIdentityDecryptsSealedTaskID(t *testing.T) {
	_, privateKey, errKey := ed25519.GenerateKey(rand.Reader)
	if errKey != nil {
		t.Fatalf("generate key: %v", errKey)
	}
	curvePublic, _ := codexAgentIdentityTestCurveKeyPair(t, privateKey)
	ciphertext, errSeal := box.SealAnonymous(nil, []byte("task-encrypted-id"), &curvePublic, rand.Reader)
	if errSeal != nil {
		t.Fatalf("seal task id: %v", errSeal)
	}

	got, errDecrypt := decryptCodexAgentTaskID(privateKey, base64.StdEncoding.EncodeToString(ciphertext))
	if errDecrypt != nil {
		t.Fatalf("decrypt task id: %v", errDecrypt)
	}
	if got != "task-encrypted-id" {
		t.Fatalf("task id = %q, want task-encrypted-id", got)
	}
}

func codexAgentIdentityTestToken(t *testing.T, email string, expiresAt time.Time) string {
	t.Helper()
	payload, errMarshal := json.Marshal(map[string]any{
		"exp": expiresAt.Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "account-id",
			"chatgpt_user_id":    "user-id",
			"chatgpt_plan_type":  "plus",
		},
		"https://api.openai.com/profile": map[string]any{"email": email},
	})
	if errMarshal != nil {
		t.Fatalf("marshal token: %v", errMarshal)
	}
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." +
		base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func decodeCodexAgentIdentitySSHPublicKey(t *testing.T, value string) ed25519.PublicKey {
	t.Helper()
	parts := strings.Split(value, " ")
	if len(parts) != 2 || parts[0] != "ssh-ed25519" {
		t.Fatalf("public key = %q", value)
	}
	blob, errDecode := base64.StdEncoding.DecodeString(parts[1])
	if errDecode != nil {
		t.Fatalf("decode public key: %v", errDecode)
	}
	if len(blob) != 51 || binary.BigEndian.Uint32(blob[:4]) != 11 || binary.BigEndian.Uint32(blob[15:19]) != 32 {
		t.Fatalf("invalid SSH public key framing: %x", blob)
	}
	return ed25519.PublicKey(append([]byte(nil), blob[19:]...))
}

func codexAgentIdentityTestCurveKeyPair(t *testing.T, privateKey ed25519.PrivateKey) ([32]byte, [32]byte) {
	t.Helper()
	digest := sha512.Sum512(privateKey.Seed())
	var curvePrivate [32]byte
	copy(curvePrivate[:], digest[:32])
	curvePrivate[0] &= 248
	curvePrivate[31] &= 127
	curvePrivate[31] |= 64
	publicBytes, errPublic := curve25519.X25519(curvePrivate[:], curve25519.Basepoint)
	if errPublic != nil {
		t.Fatalf("derive curve public key: %v", errPublic)
	}
	var curvePublic [32]byte
	copy(curvePublic[:], publicBytes)
	return curvePublic, curvePrivate
}

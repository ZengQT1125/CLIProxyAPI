package management

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	xaiauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/xai"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestAPICallTransportDirectBypassesGlobalProxy(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
		},
	}

	transport := h.apiCallTransport(&coreauth.Auth{ProxyURL: "direct"})
	httpTransport, ok := transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", transport)
	}
	if httpTransport.Proxy != nil {
		t.Fatal("expected direct transport to disable proxy function")
	}
}

func TestAPICallUsesProviderAuthenticationForAgentIdentity(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "AgentAssertion ") {
			t.Errorf("authorization header = %q, want AgentAssertion", got)
		}
		if got := r.Header.Get("Chatgpt-Account-Id"); got != "account-id" {
			t.Errorf("Chatgpt-Account-Id = %q, want account-id", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	_, privateKey, errKey := ed25519.GenerateKey(rand.Reader)
	if errKey != nil {
		t.Fatalf("generate agent identity key: %v", errKey)
	}
	privateKeyDER, errMarshalKey := x509.MarshalPKCS8PrivateKey(privateKey)
	if errMarshalKey != nil {
		t.Fatalf("marshal agent identity key: %v", errMarshalKey)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(runtimeexecutor.NewCodexExecutor(&config.Config{}))
	auth := &coreauth.Auth{
		ID:       "agent-identity",
		Index:    "agent-auth-index",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"auth_kind":         "agent_identity",
			"agent_runtime_id":  "runtime-id",
			"agent_private_key": base64.StdEncoding.EncodeToString(privateKeyDER),
			"task_id":           "task-id",
			"account_id":        "account-id",
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, manager)

	payload := fmt.Sprintf(`{"authIndex":"agent-auth-index","method":"POST","url":%q,"header":{"Authorization":"Bearer $TOKEN$"}}`, upstream.URL)
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/api-call", strings.NewReader(payload))
	c.Request.Header.Set("Content-Type", "application/json")

	h.APICall(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("APICall status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var response apiCallResponse
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &response); errDecode != nil {
		t.Fatalf("decode APICall response: %v", errDecode)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("upstream status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}
}

func TestRemoveCodexAuthRemovesRuntimeAuth(t *testing.T) {
	t.Parallel()

	authDir := t.TempDir()
	fileName := "cleanup-user.json"
	filePath := filepath.Join(authDir, fileName)
	if err := os.WriteFile(filePath, []byte(`{"type":"codex"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "cleanup-runtime-auth",
		FileName: fileName,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": filePath,
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}
	var notifiedPath string
	h.SetAuthFileMutationHook(func(path string) {
		notifiedPath = path
		if _, ok := manager.GetByID(auth.ID); !ok {
			t.Error("runtime auth changed before mutation hook")
		}
		if _, errStat := os.Stat(filePath); !os.IsNotExist(errStat) {
			t.Errorf("auth file still exists when mutation hook ran: %v", errStat)
		}
	})

	if err := h.removeCodexAuth(context.Background(), auth); err != nil {
		t.Fatalf("removeCodexAuth returned error: %v", err)
	}
	normalizedAuthDir, errEval := filepath.EvalSymlinks(authDir)
	if errEval != nil {
		t.Fatalf("resolve auth dir: %v", errEval)
	}
	wantPath := filepath.Join(normalizedAuthDir, fileName)
	if notifiedPath != wantPath {
		t.Fatalf("mutation path = %q, want %q", notifiedPath, wantPath)
	}
	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		t.Fatalf("expected auth file to be removed, stat err: %v", err)
	}
	if _, ok := manager.GetByID(auth.ID); ok {
		t.Fatalf("expected runtime auth %q to be removed", auth.ID)
	}
}

func TestRemoveCodexAuthNotifiesMutationWhenFileAlreadyMissing(t *testing.T) {
	authDir := t.TempDir()
	fileName := "already-missing.json"
	filePath := filepath.Join(authDir, fileName)
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "already-missing-runtime-auth",
		FileName: fileName,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": filePath,
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}
	var notifiedPath string
	h.SetAuthFileMutationHook(func(path string) {
		notifiedPath = path
		if _, ok := manager.GetByID(auth.ID); !ok {
			t.Error("runtime auth changed before mutation hook")
		}
	})

	if errRemove := h.removeCodexAuth(context.Background(), auth); errRemove != nil {
		t.Fatalf("removeCodexAuth returned error: %v", errRemove)
	}
	normalizedAuthDir, errEval := filepath.EvalSymlinks(authDir)
	if errEval != nil {
		t.Fatalf("resolve auth dir: %v", errEval)
	}
	wantPath := filepath.Join(normalizedAuthDir, fileName)
	if notifiedPath != wantPath {
		t.Fatalf("mutation path = %q, want %q", notifiedPath, wantPath)
	}
	if _, ok := manager.GetByID(auth.ID); ok {
		t.Fatalf("expected runtime auth %q to be removed", auth.ID)
	}
}

func TestCleanupCodexAuthDeletesClientErrorsOnly(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		body       string
		wantDelete bool
	}{
		{
			name:       "deactivated_workspace",
			statusCode: http.StatusPaymentRequired,
			body:       `{"detail":{"code":"deactivated_workspace"}}`,
			wantDelete: true,
		},
		{
			name:       "server_error",
			statusCode: http.StatusInternalServerError,
			body:       `{"error":"temporary upstream failure"}`,
			wantDelete: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			authDir := t.TempDir()
			fileName := tc.name + ".json"
			filePath := filepath.Join(authDir, fileName)
			if err := os.WriteFile(filePath, []byte(`{"type":"codex"}`), 0o600); err != nil {
				t.Fatalf("write auth file: %v", err)
			}

			var verifyRequests atomic.Int32
			verifyServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				verifyRequests.Add(1)
				if got := r.URL.Path; got != "/backend-api/wham/usage" {
					t.Errorf("verify path = %q, want /backend-api/wham/usage", got)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer codex-token" {
					t.Errorf("authorization header = %q, want Bearer codex-token", got)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.statusCode)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer verifyServer.Close()

			oldTransport := http.DefaultTransport
			http.DefaultTransport = &http.Transport{
				DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
					var dialer net.Dialer
					return dialer.DialContext(ctx, network, verifyServer.Listener.Addr().String())
				},
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			}
			t.Cleanup(func() {
				http.DefaultTransport = oldTransport
			})

			manager := coreauth.NewManager(nil, nil, nil)
			auth := &coreauth.Auth{
				ID:       "cleanup-" + tc.name,
				FileName: fileName,
				Provider: "codex",
				Status:   coreauth.StatusActive,
				Attributes: map[string]string{
					"path": filePath,
				},
				Metadata: map[string]any{
					"access_token": "codex-token",
				},
			}
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatalf("register auth: %v", err)
			}

			h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
			h.tokenStore = &memoryAuthStore{}

			gin.SetMode(gin.TestMode)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/api-tools/codex/cleanup", nil)

			h.CleanupCodexAuth(c)

			if recorder.Code != http.StatusOK {
				t.Fatalf("cleanup status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
			}
			if got := verifyRequests.Load(); got != 1 {
				t.Fatalf("verify requests = %d, want 1", got)
			}
			_, statErr := os.Stat(filePath)
			_, stillRegistered := manager.GetByID(auth.ID)
			if tc.wantDelete {
				if !os.IsNotExist(statErr) {
					t.Fatalf("expected 4xx auth file to be removed, stat err: %v", statErr)
				}
				if stillRegistered {
					t.Fatalf("expected runtime auth %q to be removed", auth.ID)
				}
			} else {
				if statErr != nil {
					t.Fatalf("expected 5xx auth file to remain, stat err: %v", statErr)
				}
				if !stillRegistered {
					t.Fatalf("expected runtime auth %q to remain", auth.ID)
				}
			}
		})
	}
}

func TestCleanupCodexAuthVerifiesCredentialsConcurrently(t *testing.T) {
	const authCount = 4

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	for i := 0; i < authCount; i++ {
		fileName := fmt.Sprintf("concurrent-%d.json", i)
		filePath := filepath.Join(authDir, fileName)
		if err := os.WriteFile(filePath, []byte(`{"type":"codex"}`), 0o600); err != nil {
			t.Fatalf("write auth file %d: %v", i, err)
		}
		auth := &coreauth.Auth{
			ID:       fmt.Sprintf("cleanup-concurrent-%d", i),
			FileName: fileName,
			Provider: "codex",
			Status:   coreauth.StatusActive,
			Attributes: map[string]string{
				"path": filePath,
			},
			Metadata: map[string]any{
				"access_token": fmt.Sprintf("codex-token-%d", i),
			},
		}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register auth %d: %v", i, err)
		}
	}

	var inFlight atomic.Int32
	var maxInFlight atomic.Int32
	verifyServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := inFlight.Add(1)
		for {
			previous := maxInFlight.Load()
			if current <= previous || maxInFlight.CompareAndSwap(previous, current) {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
		inFlight.Add(-1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer verifyServer.Close()

	oldTransport := http.DefaultTransport
	http.DefaultTransport = &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, verifyServer.Listener.Addr().String())
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	t.Cleanup(func() {
		http.DefaultTransport = oldTransport
	})

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/api-tools/codex/cleanup", nil)

	h.CleanupCodexAuth(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("cleanup status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := maxInFlight.Load(); got < 2 {
		t.Fatalf("max concurrent verify requests = %d, want at least 2", got)
	}
}

func TestCleanupXAIAuthDeletesUnauthorizedOnly(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		wantDelete bool
	}{
		{name: "unauthorized", statusCode: http.StatusUnauthorized, wantDelete: true},
		{name: "forbidden", statusCode: http.StatusForbidden, wantDelete: false},
		{name: "quota_exhausted", statusCode: http.StatusTooManyRequests, wantDelete: false},
		{name: "server_error", statusCode: http.StatusInternalServerError, wantDelete: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			authDir := t.TempDir()
			fileName := "xai-" + tc.name + ".json"
			filePath := filepath.Join(authDir, fileName)
			if err := os.WriteFile(filePath, []byte(`{"type":"xai"}`), 0o600); err != nil {
				t.Fatalf("write auth file: %v", err)
			}

			var verifyRequests atomic.Int32
			verifyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				verifyRequests.Add(1)
				if got := r.Method; got != http.MethodGet {
					t.Errorf("verify method = %q, want GET", got)
				}
				if got := r.URL.Path; got != "/v1/billing" {
					t.Errorf("verify path = %q, want /v1/billing", got)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer xai-token" {
					t.Errorf("authorization header = %q, want Bearer xai-token", got)
				}
				if got := r.Header.Get("X-XAI-Token-Auth"); got != "xai-grok-cli" {
					t.Errorf("X-XAI-Token-Auth = %q, want xai-grok-cli", got)
				}
				if got := r.Header.Get("x-grok-client-version"); got == "" {
					t.Error("x-grok-client-version is empty")
				}
				if got := r.Header.Get("X-Cleanup-Test"); got != "custom" {
					t.Errorf("custom header = %q, want custom", got)
				}
				w.WriteHeader(tc.statusCode)
				_, _ = w.Write([]byte(`{}`))
			}))
			defer verifyServer.Close()

			manager := coreauth.NewManager(nil, nil, nil)
			auth := &coreauth.Auth{
				ID:       "cleanup-xai-" + tc.name,
				FileName: fileName,
				Provider: "xai",
				Status:   coreauth.StatusActive,
				Attributes: map[string]string{
					"path":                  filePath,
					"base_url":              verifyServer.URL + "/v1",
					"header:X-Cleanup-Test": "custom",
				},
				Metadata: map[string]any{
					"access_token": "xai-token",
				},
			}
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatalf("register auth: %v", err)
			}

			h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
			h.tokenStore = &memoryAuthStore{}

			gin.SetMode(gin.TestMode)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(
				http.MethodPost,
				"/v0/management/custom/codex-cleanup",
				strings.NewReader(`{"provider":"xai"}`),
			)
			c.Request.Header.Set("Content-Type", "application/json")

			h.CleanupCodexAuth(c)

			if recorder.Code != http.StatusOK {
				t.Fatalf("cleanup status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
			}
			if got := verifyRequests.Load(); got != 1 {
				t.Fatalf("verify requests = %d, want 1", got)
			}
			_, statErr := os.Stat(filePath)
			_, stillRegistered := manager.GetByID(auth.ID)
			if tc.wantDelete {
				if !os.IsNotExist(statErr) {
					t.Fatalf("expected 401 auth file to be removed, stat err: %v", statErr)
				}
				if stillRegistered {
					t.Fatalf("expected runtime auth %q to be removed", auth.ID)
				}
				return
			}
			if statErr != nil {
				t.Fatalf("expected non-401 auth file to remain, stat err: %v", statErr)
			}
			if !stillRegistered {
				t.Fatalf("expected runtime auth %q to remain", auth.ID)
			}
		})
	}
}

func TestCleanupXAIAuthUsesCLIBillingForDefaultOAuthBaseURL(t *testing.T) {
	authDir := t.TempDir()
	fileName := "xai-default-base.json"
	filePath := filepath.Join(authDir, fileName)
	if err := os.WriteFile(filePath, []byte(`{"type":"xai"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	var verifyRequests atomic.Int32
	verifyServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		verifyRequests.Add(1)
		if got := r.Host; got != "cli-chat-proxy.grok.com" {
			t.Errorf("verify host = %q, want cli-chat-proxy.grok.com", got)
		}
		if got := r.URL.Path; got != "/v1/billing" {
			t.Errorf("verify path = %q, want /v1/billing", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer verifyServer.Close()

	oldTransport := http.DefaultTransport
	http.DefaultTransport = &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, verifyServer.Listener.Addr().String())
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	t.Cleanup(func() {
		http.DefaultTransport = oldTransport
	})

	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "cleanup-xai-default-base",
		FileName: fileName,
		Provider: "xai",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path":      filePath,
			"base_url":  xaiauth.DefaultAPIBaseURL,
			"auth_kind": "oauth",
		},
		Metadata: map[string]any{
			"access_token": "xai-token",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/custom/codex-cleanup?provider=xai", nil)

	h.CleanupCodexAuth(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("cleanup status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := verifyRequests.Load(); got != 1 {
		t.Fatalf("verify requests = %d, want 1", got)
	}
	if _, err := os.Stat(filePath); err != nil {
		t.Fatalf("expected successful xai auth file to remain, stat err: %v", err)
	}
	if _, ok := manager.GetByID(auth.ID); !ok {
		t.Fatalf("expected successful xai runtime auth to remain")
	}
}

func TestCleanupXAIAuthSkipsConfigAPIKeys(t *testing.T) {
	authDir := t.TempDir()
	sentinelPath := filepath.Join(authDir, "sentinel")
	if err := os.WriteFile(sentinelPath, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	var verifyRequests atomic.Int32
	verifyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		verifyRequests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer verifyServer.Close()

	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "xai-config-api-key",
		Provider: "xai",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"source":    "config:xai[test]",
			"api_key":   "xai-api-key",
			"auth_kind": "apikey",
			"base_url":  verifyServer.URL + "/v1",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/custom/codex-cleanup?provider=xai", nil)

	h.CleanupCodexAuth(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("cleanup status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := verifyRequests.Load(); got != 0 {
		t.Fatalf("verify requests = %d, want 0 for config API key", got)
	}
	if _, err := os.Stat(sentinelPath); err != nil {
		t.Fatalf("expected auth directory contents to remain, stat err: %v", err)
	}
	if _, ok := manager.GetByID(auth.ID); !ok {
		t.Fatalf("expected config xai API key to remain registered")
	}
}

func TestCleanupXAIAuthSkipsFileBackedAPIKeys(t *testing.T) {
	authDir := t.TempDir()
	fileName := "xai-api-key.json"
	filePath := filepath.Join(authDir, fileName)
	if err := os.WriteFile(filePath, []byte(`{"type":"xai","auth_kind":"apikey"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	var verifyRequests atomic.Int32
	verifyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		verifyRequests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer verifyServer.Close()

	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "xai-file-api-key",
		FileName: fileName,
		Provider: "xai",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path":      filePath,
			"api_key":   "xai-api-key",
			"auth_kind": "apikey",
			"base_url":  verifyServer.URL + "/v1",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/custom/codex-cleanup?provider=xai", nil)

	h.CleanupCodexAuth(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("cleanup status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := verifyRequests.Load(); got != 0 {
		t.Fatalf("verify requests = %d, want 0 for file-backed API key", got)
	}
	if _, err := os.Stat(filePath); err != nil {
		t.Fatalf("expected xai API key file to remain, stat err: %v", err)
	}
	if _, ok := manager.GetByID(auth.ID); !ok {
		t.Fatalf("expected file-backed xai API key to remain registered")
	}
}

func TestCleanupXAIAuthDoesNotDeleteReplacementCredential(t *testing.T) {
	authDir := t.TempDir()
	oldFileName := "xai-old.json"
	oldFilePath := filepath.Join(authDir, oldFileName)
	if err := os.WriteFile(oldFilePath, []byte(`{"type":"xai","auth_kind":"oauth"}`), 0o600); err != nil {
		t.Fatalf("write old auth file: %v", err)
	}
	newFileName := "xai-new.json"
	newFilePath := filepath.Join(authDir, newFileName)
	if err := os.WriteFile(newFilePath, []byte(`{"type":"xai","auth_kind":"oauth"}`), 0o600); err != nil {
		t.Fatalf("write replacement auth file: %v", err)
	}

	verifyStarted := make(chan struct{})
	releaseVerify := make(chan struct{})
	verifyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(verifyStarted)
		<-releaseVerify
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer verifyServer.Close()

	manager := coreauth.NewManager(nil, nil, nil)
	original := &coreauth.Auth{
		ID:       "xai-replaced-during-cleanup",
		FileName: oldFileName,
		Provider: "xai",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path":      oldFilePath,
			"base_url":  verifyServer.URL + "/v1",
			"auth_kind": "oauth",
		},
		Metadata: map[string]any{"access_token": "old-token"},
	}
	if _, err := manager.Register(context.Background(), original); err != nil {
		t.Fatalf("register original auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/custom/codex-cleanup?provider=xai", nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.CleanupCodexAuth(c)
	}()

	select {
	case <-verifyStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for xai billing verification")
	}

	replacement := &coreauth.Auth{
		ID:       original.ID,
		FileName: newFileName,
		Provider: "xai",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path":      newFilePath,
			"base_url":  verifyServer.URL + "/v1",
			"auth_kind": "oauth",
		},
		Metadata: map[string]any{"access_token": "new-token"},
	}
	if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), replacement); err != nil {
		t.Fatalf("register replacement auth: %v", err)
	}
	close(releaseVerify)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cleanup to finish")
	}

	if recorder.Code != http.StatusOK {
		t.Fatalf("cleanup status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if _, err := os.Stat(oldFilePath); err != nil {
		t.Fatalf("expected stale auth file to remain after replacement, stat err: %v", err)
	}
	if _, err := os.Stat(newFilePath); err != nil {
		t.Fatalf("expected replacement auth file to remain, stat err: %v", err)
	}
	current, ok := manager.GetByID(original.ID)
	if !ok {
		t.Fatalf("expected replacement auth to remain registered")
	}
	if got := current.Metadata["access_token"]; got != "new-token" {
		t.Fatalf("replacement access token = %#v, want new-token", got)
	}
}

func TestCleanupAuthRejectsUnsupportedProvider(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, coreauth.NewManager(nil, nil, nil))

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/v0/management/custom/codex-cleanup",
		strings.NewReader(`{"provider":"qwen"}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.CleanupCodexAuth(c)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("cleanup status = %d, want %d; body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "unsupported cleanup provider: qwen") {
		t.Fatalf("expected unsupported provider message, body=%s", recorder.Body.String())
	}
}

func TestAPICallTransportInvalidAuthFallsBackToGlobalProxy(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
		},
	}

	transport := h.apiCallTransport(&coreauth.Auth{ProxyURL: "bad-value"})
	httpTransport, ok := transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", transport)
	}

	req, errRequest := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if errRequest != nil {
		t.Fatalf("http.NewRequest returned error: %v", errRequest)
	}

	proxyURL, errProxy := httpTransport.Proxy(req)
	if errProxy != nil {
		t.Fatalf("httpTransport.Proxy returned error: %v", errProxy)
	}
	if proxyURL == nil || proxyURL.String() != "http://global-proxy.example.com:8080" {
		t.Fatalf("proxy URL = %v, want http://global-proxy.example.com:8080", proxyURL)
	}
}

func TestAPICallTransportAPIKeyAuthFallsBackToConfigProxyURL(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
			GeminiKey: []config.GeminiKey{{
				APIKey:   "gemini-key",
				ProxyURL: "http://gemini-proxy.example.com:8080",
			}},
			ClaudeKey: []config.ClaudeKey{{
				APIKey:   "claude-key",
				ProxyURL: "http://claude-proxy.example.com:8080",
			}},
			CodexKey: []config.CodexKey{{
				APIKey:   "codex-key",
				ProxyURL: "http://codex-proxy.example.com:8080",
			}},
			XAIKey: []config.XAIKey{{
				APIKey:   "xai-key",
				ProxyURL: "http://xai-proxy.example.com:8080",
			}},
			OpenAICompatibility: []config.OpenAICompatibility{{
				Name:    "bohe",
				BaseURL: "https://bohe.example.com",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{{
					APIKey:   "compat-key",
					ProxyURL: "http://compat-proxy.example.com:8080",
				}},
			}},
		},
	}

	cases := []struct {
		name      string
		auth      *coreauth.Auth
		wantProxy string
	}{
		{
			name: "gemini",
			auth: &coreauth.Auth{
				Provider:   "gemini",
				Attributes: map[string]string{"api_key": "gemini-key"},
			},
			wantProxy: "http://gemini-proxy.example.com:8080",
		},
		{
			name: "claude",
			auth: &coreauth.Auth{
				Provider:   "claude",
				Attributes: map[string]string{"api_key": "claude-key"},
			},
			wantProxy: "http://claude-proxy.example.com:8080",
		},
		{
			name: "codex",
			auth: &coreauth.Auth{
				Provider:   "codex",
				Attributes: map[string]string{"api_key": "codex-key"},
			},
			wantProxy: "http://codex-proxy.example.com:8080",
		},
		{
			name: "xai",
			auth: &coreauth.Auth{
				Provider:   "xai",
				Attributes: map[string]string{"api_key": "xai-key"},
			},
			wantProxy: "http://xai-proxy.example.com:8080",
		},
		{
			name: "openai-compatibility",
			auth: &coreauth.Auth{
				Provider: "bohe",
				Attributes: map[string]string{
					"api_key":      "compat-key",
					"compat_name":  "bohe",
					"provider_key": "bohe",
				},
			},
			wantProxy: "http://compat-proxy.example.com:8080",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			transport := h.apiCallTransport(tc.auth)
			httpTransport, ok := transport.(*http.Transport)
			if !ok {
				t.Fatalf("transport type = %T, want *http.Transport", transport)
			}

			req, errRequest := http.NewRequest(http.MethodGet, "https://example.com", nil)
			if errRequest != nil {
				t.Fatalf("http.NewRequest returned error: %v", errRequest)
			}

			proxyURL, errProxy := httpTransport.Proxy(req)
			if errProxy != nil {
				t.Fatalf("httpTransport.Proxy returned error: %v", errProxy)
			}
			if proxyURL == nil || proxyURL.String() != tc.wantProxy {
				t.Fatalf("proxy URL = %v, want %s", proxyURL, tc.wantProxy)
			}
		})
	}
}

func TestAuthByIndexDistinguishesSharedAPIKeysAcrossProviders(t *testing.T) {
	t.Parallel()

	manager := coreauth.NewManager(nil, nil, nil)
	geminiAuth := &coreauth.Auth{
		ID:       "gemini:apikey:123",
		Provider: "gemini",
		Attributes: map[string]string{
			"api_key": "shared-key",
		},
	}
	compatAuth := &coreauth.Auth{
		ID:       "openai-compatibility:bohe:456",
		Provider: "bohe",
		Label:    "bohe",
		Attributes: map[string]string{
			"api_key":      "shared-key",
			"compat_name":  "bohe",
			"provider_key": "bohe",
		},
	}

	if _, errRegister := manager.Register(context.Background(), geminiAuth); errRegister != nil {
		t.Fatalf("register gemini auth: %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), compatAuth); errRegister != nil {
		t.Fatalf("register compat auth: %v", errRegister)
	}

	geminiIndex := geminiAuth.EnsureIndex()
	compatIndex := compatAuth.EnsureIndex()
	if geminiIndex == compatIndex {
		t.Fatalf("shared api key produced duplicate auth_index %q", geminiIndex)
	}

	h := &Handler{authManager: manager}

	gotGemini := h.authByIndex(geminiIndex)
	if gotGemini == nil {
		t.Fatal("expected gemini auth by index")
	}
	if gotGemini.ID != geminiAuth.ID {
		t.Fatalf("authByIndex(gemini) returned %q, want %q", gotGemini.ID, geminiAuth.ID)
	}

	gotCompat := h.authByIndex(compatIndex)
	if gotCompat == nil {
		t.Fatal("expected compat auth by index")
	}
	if gotCompat.ID != compatAuth.ID {
		t.Fatalf("authByIndex(compat) returned %q, want %q", gotCompat.ID, compatAuth.ID)
	}
}

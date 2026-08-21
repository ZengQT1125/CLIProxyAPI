package management

import (
	"bytes"
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
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestAPICallUsesRequestProxyURL(t *testing.T) {
	t.Parallel()

	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("proxied"))
	}))
	defer proxyServer.Close()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://127.0.0.1:1"},
		},
	}
	router := gin.New()
	router.POST("/", h.APICall)

	body := `{"method":"GET","url":"http://upstream.invalid/test","proxy_url":"` + proxyServer.URL + `"}`
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	var response apiCallResponse
	if errDecode := json.NewDecoder(recorder.Body).Decode(&response); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("upstream status code = %d, want %d", response.StatusCode, http.StatusCreated)
	}
	if response.Body != "proxied" {
		t.Fatalf("upstream body = %q, want %q", response.Body, "proxied")
	}
}

func TestAPICallUsesRequestProxyURLWithProviderAuthentication(t *testing.T) {
	t.Parallel()

	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer request-token" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer request-token")
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("authenticated proxy"))
	}))
	defer proxyServer.Close()

	cfg := &config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://127.0.0.1:1"}}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(runtimeexecutor.NewCodexExecutor(cfg))
	auth := &coreauth.Auth{
		ID:       "request-proxy-auth",
		Index:    "request-proxy-index",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		ProxyURL: "http://127.0.0.1:2",
		Metadata: map[string]any{"access_token": "request-token"},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(cfg, manager)
	router := gin.New()
	router.POST("/", h.APICall)

	body := `{"auth_index":"request-proxy-index","method":"GET","url":"http://upstream.invalid/test","proxy_url":"` + proxyServer.URL + `"}`
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	var response apiCallResponse
	if errDecode := json.NewDecoder(recorder.Body).Decode(&response); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("upstream status code = %d, want %d", response.StatusCode, http.StatusCreated)
	}
	if response.Body != "authenticated proxy" {
		t.Fatalf("upstream body = %q, want %q", response.Body, "authenticated proxy")
	}
}

func TestAPICallTransportDirectBypassesGlobalProxy(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
		},
	}

	transport := h.apiCallTransport(&coreauth.Auth{ProxyURL: "direct"}, "")
	httpTransport, ok := transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", transport)
	}
	if httpTransport.Proxy != nil {
		t.Fatal("expected direct transport to disable proxy function")
	}
}

// TestAPICallRejectsCredentialWithoutToken pins the failure mode behind the
// misleading upstream error "x-api-key header is required": when a credential
// holds no access token, the $TOKEN$ header is dropped and the request used to
// reach the upstream with no authentication at all. It must fail here instead.
func TestAPICallRejectsCredentialWithoutToken(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(runtimeexecutor.NewClaudeExecutor(&config.Config{}))
	auth := &coreauth.Auth{
		ID:       "claude-oauth",
		Index:    "claude-auth-index",
		Provider: "claude",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"type":          "claude",
			"email":         "user@example.com",
			"refresh_token": "sk-ant-ort01-refresh",
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, manager)

	payload := fmt.Sprintf(`{"authIndex":"claude-auth-index","method":"POST","url":%q,"header":{"Authorization":"Bearer $TOKEN$"},"data":"{}"}`, upstream.URL)
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/api-call", strings.NewReader(payload))
	c.Request.Header.Set("Content-Type", "application/json")

	h.APICall(c)

	if got := upstreamCalls.Load(); got != 0 {
		t.Errorf("upstream calls = %d, want 0: unauthenticated request must not be forwarded", got)
	}
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("APICall status = %d, want %d; body=%s", recorder.Code, http.StatusUnauthorized, recorder.Body.String())
	}
}

func TestAPICallAllowsEmptyAPIKeyCredential(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want empty for empty API key", got)
		}
		if got := r.Header.Get("Custom-Token"); got != "custom-secret" {
			t.Errorf("Custom-Token = %q, want custom-secret", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(runtimeexecutor.NewClaudeExecutor(&config.Config{}))
	auth := &coreauth.Auth{
		ID:       "claude-apikey",
		Index:    "claude-apikey-index",
		Provider: "claude",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			coreauth.AttributeAuthKind: coreauth.AuthKindAPIKey,
			"base_url":                 upstream.URL,
			"header:Custom-Token":      "custom-secret",
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, manager)

	payload := fmt.Sprintf(`{"authIndex":"claude-apikey-index","method":"POST","url":%q,"header":{"Authorization":"Bearer $TOKEN$"},"data":"{}"}`, upstream.URL)
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/api-call", strings.NewReader(payload))
	c.Request.Header.Set("Content-Type", "application/json")

	h.APICall(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("APICall status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 for base_URL-only API key", got)
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

func TestAPICallBatchReturnsOrderedPartialResults(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("X-Test-Path", r.URL.Path)
		if r.URL.Path == "/denied" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"denied"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	payload := fmt.Sprintf(`{"requests":[
		{"id":"ok","method":"GET","url":%q},
		{"id":"invalid","method":"GET","url":"not-a-url"},
		{"id":"denied","method":"GET","url":%q}
	]}`, upstream.URL+"/ok", upstream.URL+"/denied")

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/custom/api-call/batch", strings.NewReader(payload))
	c.Request.Header.Set("Content-Type", "application/json")

	h.APICallBatch(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("APICallBatch status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := upstreamCalls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}

	var response apiCallBatchResponse
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &response); errDecode != nil {
		t.Fatalf("decode APICallBatch response: %v", errDecode)
	}
	if len(response.Results) != 3 {
		t.Fatalf("results length = %d, want 3", len(response.Results))
	}
	if got := response.Results[0]; got.ID != "ok" || got.StatusCode != http.StatusOK || got.Error != "" {
		t.Errorf("first result = %+v, want successful ok result", got)
	}
	if got := response.Results[1]; got.ID != "invalid" || got.ErrorStatus != http.StatusBadRequest || got.Error != "invalid url" {
		t.Errorf("second result = %+v, want invalid-url item error", got)
	}
	if got := response.Results[2]; got.ID != "denied" || got.StatusCode != http.StatusForbidden || got.Error != "" {
		t.Errorf("third result = %+v, want upstream forbidden response", got)
	}
}

func TestAPICallBatchRejectsAmbiguousRequestSet(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{name: "empty", payload: `{"requests":[]}`},
		{name: "missing id", payload: `{"requests":[{"method":"GET","url":"https://example.com"}]}`},
		{name: "duplicate id", payload: `{"requests":[{"id":"same","method":"GET","url":"https://example.com"},{"id":"same","method":"GET","url":"https://example.com"}]}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/custom/api-call/batch", strings.NewReader(tc.payload))
			c.Request.Header.Set("Content-Type", "application/json")

			h.APICallBatch(c)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
			}
		})
	}
}

func TestAPICallBatchRejectsTooManyRequests(t *testing.T) {
	requests := make([]apiCallRequest, maxAPICallBatchSize+1)
	for i := range requests {
		requests[i] = apiCallRequest{
			ID:     fmt.Sprintf("request-%d", i),
			Method: http.MethodGet,
			URL:    "https://example.com",
		}
	}
	payload, errMarshal := json.Marshal(apiCallBatchRequest{Requests: requests})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/custom/api-call/batch", bytes.NewReader(payload))
	c.Request.Header.Set("Content-Type", "application/json")

	h.APICallBatch(c)

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusRequestEntityTooLarge, recorder.Body.String())
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

func TestCleanupAuthRejectsUnsupportedProvider(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, coreauth.NewManager(nil, nil, nil))

	for _, provider := range []string{"qwen", "xai"} {
		t.Run(provider, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(
				http.MethodPost,
				"/v0/management/custom/codex-cleanup",
				strings.NewReader(`{"provider":"`+provider+`"}`),
			)
			c.Request.Header.Set("Content-Type", "application/json")

			h.CleanupCodexAuth(c)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("cleanup status = %d, want %d; body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), "unsupported cleanup provider: "+provider) {
				t.Fatalf("expected unsupported provider message, body=%s", recorder.Body.String())
			}
		})
	}
}

func TestAPICallTransportInvalidAuthFallsBackToGlobalProxy(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
		},
	}

	transport := h.apiCallTransport(&coreauth.Auth{ProxyURL: "bad-value"}, "")
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

func TestAPICallTransportRequestProxyOverridesCredentialAndGlobalProxy(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
		},
	}
	auth := &coreauth.Auth{ProxyURL: "http://credential-proxy.example.com:8080"}

	transport := h.apiCallTransport(auth, " http://request-proxy.example.com:8080 ")
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
	if proxyURL == nil || proxyURL.String() != "http://request-proxy.example.com:8080" {
		t.Fatalf("proxy URL = %v, want http://request-proxy.example.com:8080", proxyURL)
	}
}

func TestAPICallTransportInvalidRequestProxyDoesNotFallBack(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
		},
	}
	auth := &coreauth.Auth{ProxyURL: "http://credential-proxy.example.com:8080"}

	transport := h.apiCallTransport(auth, "bad-value")
	httpTransport, ok := transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", transport)
	}
	if httpTransport.Proxy != nil {
		t.Fatal("expected invalid request proxy to avoid lower-priority proxy settings")
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

			transport := h.apiCallTransport(tc.auth, "")
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

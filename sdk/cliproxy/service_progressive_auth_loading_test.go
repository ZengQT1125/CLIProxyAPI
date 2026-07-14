package cliproxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestServiceRunListensBeforeInitialAuthLoadCompletes(t *testing.T) {
	oldStore := sdkAuth.GetTokenStore()
	sdkAuth.RegisterTokenStore(sdkAuth.NewFileTokenStore())
	t.Cleanup(func() { sdkAuth.RegisterTokenStore(oldStore) })
	authDir := t.TempDir()
	port := reserveTCPPort(t)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(configPath, []byte(fmt.Sprintf("host: 127.0.0.1\nport: %d\nauth-dir: %s\n", port, authDir)), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	loadStarted := make(chan struct{})
	loadRelease := make(chan struct{})
	wrapper := &WatcherWrapper{
		start:          func(context.Context) error { return nil },
		stop:           func() error { return nil },
		setConfig:      func(*config.Config) {},
		setUpdateQueue: func(chan<- watcher.AuthUpdateBatch) {},
		startInitialAuthLoad: func(context.Context, int) <-chan struct{} {
			done := make(chan struct{})
			close(loadStarted)
			go func() { <-loadRelease; close(done) }()
			return done
		},
		authLoadStatus: func() watcher.AuthLoadStatus {
			return watcher.AuthLoadStatus{State: watcher.AuthLoadStateLoading}
		},
	}
	service, errBuild := NewBuilder().
		WithConfig(&config.Config{Host: "127.0.0.1", Port: port, AuthDir: authDir, AuthLoadWorkers: 4}).
		WithConfigPath(configPath).
		WithWatcherFactory(func(string, string, func(*config.Config)) (*WatcherWrapper, error) { return wrapper, nil }).
		Build()
	if errBuild != nil {
		t.Fatal(errBuild)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	defer func() {
		close(loadRelease)
		cancel()
		<-done
	}()
	select {
	case <-loadStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("initial auth load did not start")
	}
	response, errGet := http.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", port))
	if errGet != nil {
		t.Fatalf("listener unavailable while auth load blocked: %v", errGet)
	}
	_ = response.Body.Close()
}

func reserveTCPPort(t testing.TB) int {
	t.Helper()
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatal(errListen)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if errClose := listener.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	return port
}

func TestBuilderMarksOnlyDefaultFileManagerProgressive(t *testing.T) {
	oldStore := sdkAuth.GetTokenStore()
	fileStore := sdkAuth.NewFileTokenStore()
	sdkAuth.RegisterTokenStore(fileStore)
	t.Cleanup(func() { sdkAuth.RegisterTokenStore(oldStore) })
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	cfg := &config.Config{AuthDir: t.TempDir(), Port: 8317, AuthLoadWorkers: 16}
	service, errBuild := NewBuilder().WithConfig(cfg).WithConfigPath(configPath).Build()
	if errBuild != nil {
		t.Fatal(errBuild)
	}
	if !service.progressiveFileAuth {
		t.Fatal("default FileTokenStore manager was not marked progressive")
	}
	customManager := coreauth.NewManager(fileStore, nil, nil)
	customService, errCustom := NewBuilder().WithConfig(cfg).WithConfigPath(configPath).WithCoreAuthManager(customManager).Build()
	if errCustom != nil {
		t.Fatal(errCustom)
	}
	if customService.progressiveFileAuth {
		t.Fatal("injected Manager was incorrectly marked progressive")
	}
}

func TestServiceAuthBatchAcknowledgedAfterModelsReachScheduler(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	service := &Service{
		cfg:         &config.Config{},
		coreManager: manager,
		authUpdates: make(chan watcher.AuthUpdateBatch, 1),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go service.consumeAuthUpdates(ctx)
	resultCh := make(chan []watcher.AuthUpdateResult, 1)
	service.authUpdates <- watcher.AuthUpdateBatch{
		Updates: []watcher.AuthUpdate{{
			Action: watcher.AuthUpdateActionAdd,
			ID:     "xai-progressive",
			Auth:   &coreauth.Auth{ID: "xai-progressive", Provider: "xai", Status: coreauth.StatusActive},
		}},
		Result: resultCh,
	}
	results := <-resultCh
	if len(results) != 1 || !results[0].Loaded {
		t.Fatalf("batch results = %+v", results)
	}
	models := registry.GetGlobalRegistry().GetModelsForClient("xai-progressive")
	if len(models) == 0 {
		t.Fatal("acknowledgement arrived before model registration")
	}
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("xai-progressive") })
	manager.RegisterExecutor(progressiveBatchTestExecutor{})
	response, errExecute := manager.Execute(ctx, []string{"xai"}, cliproxyexecutor.Request{Model: models[0].ID}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("Execute() after acknowledgement error = %v", errExecute)
	}
	if string(response.Payload) != "xai-progressive" {
		t.Fatalf("selected payload = %q, want auth id", response.Payload)
	}
}

type progressiveBatchTestExecutor struct{}

func (progressiveBatchTestExecutor) Identifier() string { return "xai" }

func (progressiveBatchTestExecutor) Execute(_ context.Context, auth *coreauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}

func (progressiveBatchTestExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (progressiveBatchTestExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (progressiveBatchTestExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (progressiveBatchTestExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

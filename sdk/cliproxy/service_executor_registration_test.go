package cliproxy

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginhost"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

type serviceTestPluginExecutor struct{}

func (serviceTestPluginExecutor) Identifier() string {
	return "plugin-provider"
}

func (serviceTestPluginExecutor) Execute(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (serviceTestPluginExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (serviceTestPluginExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (serviceTestPluginExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (serviceTestPluginExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestRegisterAvailableExecutors(t *testing.T) {
	oldRegisterPluginExecutors := registerPluginExecutors
	pluginRegisterCalls := 0
	var expectedPluginHost *pluginhost.Host
	var expectedManager *coreauth.Manager
	registerPluginExecutors = func(host *pluginhost.Host, manager *coreauth.Manager) {
		pluginRegisterCalls++
		if host != expectedPluginHost {
			t.Fatalf("plugin executor registration host = %p, want %p", host, expectedPluginHost)
		}
		if manager != expectedManager {
			t.Fatalf("plugin executor registration manager = %p, want %p", manager, expectedManager)
		}
		manager.RegisterExecutor(serviceTestPluginExecutor{})
	}
	t.Cleanup(func() {
		registerPluginExecutors = oldRegisterPluginExecutors
	})

	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
		pluginHost:  pluginhost.New(),
	}
	expectedPluginHost = service.pluginHost
	expectedManager = service.coreManager
	service.ensureWebsocketGateway()

	service.registerAvailableExecutors(nil, executorRegistrationOptions{
		includeBaseline: true,
		includePlugins:  true,
	})

	if pluginRegisterCalls != 1 {
		t.Fatalf("plugin executor registration calls = %d, want 1", pluginRegisterCalls)
	}

	providers := []string{
		"codex",
		"claude",
		"gemini",
		"gemini-interactions",
		"vertex",
		"aistudio",
		"antigravity",
		"kimi",
		"xai",
		"openai-compatibility",
		"plugin-provider",
	}
	for _, provider := range providers {
		resolved, ok := service.coreManager.Executor(provider)
		if !ok || resolved == nil {
			t.Fatalf("expected executor for provider %s after registration", provider)
		}
	}

	resolved, _ := service.coreManager.Executor("plugin-provider")
	if _, isPlugin := resolved.(serviceTestPluginExecutor); !isPlugin {
		t.Fatalf("executor type = %T, want serviceTestPluginExecutor", resolved)
	}
}

func TestRegisterExecutorForAuth_OpenAICompatUsesNamespacedProviderKey(t *testing.T) {
	testCases := []struct {
		name  string
		auths []*coreauth.Auth
	}{
		{
			name: "native first",
			auths: []*coreauth.Auth{
				{ID: "native-kimi", Provider: "kimi"},
				openAICompatKimiAuth(),
			},
		},
		{
			name: "compat first",
			auths: []*coreauth.Auth{
				openAICompatKimiAuth(),
				{ID: "native-kimi", Provider: "kimi"},
			},
		},
	}

	for _, tt := range testCases {
		t.Run(tt.name, func(t *testing.T) {
			service := &Service{
				cfg:         &config.Config{},
				coreManager: coreauth.NewManager(nil, nil, nil),
			}

			service.registerExecutorsForAuths(tt.auths, true)

			nativeExecutor, okNative := service.coreManager.Executor("kimi")
			if !okNative {
				t.Fatal("expected native kimi executor")
			}
			if _, okKimi := nativeExecutor.(*runtimeexecutor.KimiExecutor); !okKimi {
				t.Fatalf("native executor type = %T, want *executor.KimiExecutor", nativeExecutor)
			}

			compatExecutor, okCompat := service.coreManager.Executor("openai-compatible-kimi")
			if !okCompat {
				t.Fatal("expected namespaced OpenAI-compatible executor")
			}
			if _, okOpenAICompat := compatExecutor.(*runtimeexecutor.OpenAICompatExecutor); !okOpenAICompat {
				t.Fatalf("compat executor type = %T, want *executor.OpenAICompatExecutor", compatExecutor)
			}
		})
	}
}

func openAICompatKimiAuth() *coreauth.Auth {
	return &coreauth.Auth{
		ID:       "compat-kimi",
		Provider: "openai-compatibility",
		Label:    "kimi",
		Attributes: map[string]string{
			"compat_name":  "kimi",
			"provider_key": "kimi",
		},
	}
}

// TestRegisterModelsForAuthBatch_BindsExecutorForLoadedPGAuth reproduces the
// non-home PostgreSQL startup path:
//
//	Manager.Load (oauth auths in coreManager) →
//	registerModelsForAuthBatch / completeModelRegistrationForAuthWithCache
//
// prepareCoreAuthForModelRegistration is the only historical path that called
// ensureExecutorsForAuth. The batch path used on PG (non-progressive) startup
// skipped it. Models still land in the global registry (so handlers report
// providers=xai), but pickNextMixed filters out providers with no executor and
// returns auth_not_found: no auth available — the production symptom.
func TestRegisterModelsForAuthBatch_BindsExecutorForLoadedPGAuth(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	auth := &coreauth.Auth{
		ID:       "pg-xai-oauth-1",
		Provider: "xai",
		Status:   coreauth.StatusActive,
	}
	if _, err := service.coreManager.Register(coreauth.WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := service.coreManager.Executor("xai"); ok {
		t.Fatal("precondition: xai executor must not be bound before model registration")
	}

	service.registerModelsForAuthBatch(context.Background(), []*coreauth.Auth{auth})

	got, ok := service.coreManager.Executor("xai")
	if !ok || got == nil {
		t.Fatal("expected registerModelsForAuthBatch to bind xai executor for a loaded PG oauth auth")
	}
	if id := got.Identifier(); id != "xai" {
		t.Fatalf("executor Identifier() = %q, want xai (type %T)", id, got)
	}
}

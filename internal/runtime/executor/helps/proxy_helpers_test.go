package helps

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestNewProxyAwareHTTPClientDirectBypassesGlobalProxy(t *testing.T) {
	t.Parallel()

	client := NewProxyAwareHTTPClient(
		context.Background(),
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}},
		&cliproxyauth.Auth{ProxyURL: "direct"},
		0,
	)

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("expected direct transport to disable proxy function")
	}
}

func TestNewProxyAwareHTTPClientReusesProxyTransport(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://127.0.0.1:9"}}
	first := NewProxyAwareHTTPClient(context.Background(), cfg, nil, 0)
	second := NewProxyAwareHTTPClient(context.Background(), cfg, nil, 2*time.Second)

	if first == second {
		t.Fatal("clients unexpectedly share the same *http.Client")
	}
	if first.Transport == nil {
		t.Fatal("first client transport is nil")
	}
	if first.Transport != second.Transport {
		t.Fatalf("proxy transport was not reused: first=%p second=%p", first.Transport, second.Transport)
	}
	if second.Timeout != 2*time.Second {
		t.Fatalf("second client timeout = %s, want 2s", second.Timeout)
	}
}

func BenchmarkNewProxyAwareHTTPClientWithProxy(b *testing.B) {
	ctx := context.Background()
	cfg := &config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://127.0.0.1:9"}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = NewProxyAwareHTTPClient(ctx, cfg, nil, 0)
	}
}

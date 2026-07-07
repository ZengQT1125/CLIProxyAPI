package helps

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

type proxyTransportCacheStore struct {
	mu         sync.Mutex
	transports map[string]*http.Transport
}

var proxyTransportCache = &proxyTransportCacheStore{
	transports: make(map[string]*http.Transport),
}

// NewProxyAwareHTTPClient creates an HTTP client with proper proxy configuration priority:
// 1. Use auth.ProxyURL if configured (highest priority)
// 2. Use cfg.ProxyURL if auth proxy is not configured
// 3. Use RoundTripper from context if neither are configured
//
// Parameters:
//   - ctx: The context containing optional RoundTripper
//   - cfg: The application configuration
//   - auth: The authentication information
//   - timeout: The client timeout (0 means no timeout)
//
// Returns:
//   - *http.Client: An HTTP client with configured proxy or transport
func NewProxyAwareHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	httpClient := &http.Client{}
	if timeout > 0 {
		httpClient.Timeout = timeout
	}

	proxyURL := ProxyURLForHTTPClient(cfg, auth)

	// If we have a proxy URL configured, set up the transport
	if proxyURL != "" {
		transport := CachedProxyTransport(proxyURL)
		if transport != nil {
			httpClient.Transport = transport
			return httpClient
		}
		// If proxy setup failed, log and fall through to context RoundTripper
		log.Debugf("failed to setup proxy from URL: %s, falling back to context transport", proxyutil.Redact(proxyURL))
	}

	// Priority 3: Use RoundTripper from context (typically from RoundTripperFor)
	if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
		httpClient.Transport = rt
	}

	return httpClient
}

// ProxyURLForHTTPClient resolves the proxy URL used by proxy-aware clients.
func ProxyURLForHTTPClient(cfg *config.Config, auth *cliproxyauth.Auth) string {
	// Priority 1: Use auth.ProxyURL if configured.
	if auth != nil {
		if proxyURL := strings.TrimSpace(auth.ProxyURL); proxyURL != "" {
			return proxyURL
		}
	}

	// Priority 2: Use cfg.ProxyURL if auth proxy is not configured.
	if cfg != nil {
		return strings.TrimSpace(cfg.ProxyURL)
	}

	return ""
}

// CachedProxyTransport returns a shared transport for the given proxy URL.
func CachedProxyTransport(proxyURL string) *http.Transport {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return nil
	}
	return proxyTransportCache.transport(proxyURL)
}

func (c *proxyTransportCacheStore) transport(proxyURL string) *http.Transport {
	if c == nil {
		return buildProxyTransport(proxyURL)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if transport := c.transports[proxyURL]; transport != nil {
		return transport
	}
	transport := buildProxyTransport(proxyURL)
	if transport == nil {
		return nil
	}
	c.transports[proxyURL] = transport
	return transport
}

// buildProxyTransport creates an HTTP transport configured for the given proxy URL.
// It supports SOCKS5, HTTP, and HTTPS proxy protocols.
//
// Parameters:
//   - proxyURL: The proxy URL string (e.g., "socks5://user:pass@host:port", "http://host:port")
//
// Returns:
//   - *http.Transport: A configured transport, or nil if the proxy URL is invalid
func buildProxyTransport(proxyURL string) *http.Transport {
	transport, _, errBuild := proxyutil.BuildHTTPTransport(proxyURL)
	if errBuild != nil {
		log.Errorf("%v", errBuild)
		return nil
	}
	return transport
}

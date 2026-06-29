package helps

import (
	"context"
	cryptotls "crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	tls "github.com/refraction-networking/utls"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

// utlsRoundTripper implements http.RoundTripper using utls with Chrome fingerprint
// to bypass Cloudflare's TLS fingerprinting on Anthropic domains.
type utlsRoundTripper struct {
	mu          sync.Mutex
	connections map[string]*http2.ClientConn
	pending     map[string]*sync.Cond
	dialer      proxy.Dialer
}

func newUtlsRoundTripper(proxyURL string) *utlsRoundTripper {
	var dialer proxy.Dialer = proxy.Direct
	if proxyURL != "" {
		proxyDialer, mode, errBuild := proxyutil.BuildDialer(proxyURL)
		if errBuild != nil {
			log.Errorf("utls: failed to configure proxy dialer for %q: %v", proxyutil.Redact(proxyURL), errBuild)
		} else if mode != proxyutil.ModeInherit && proxyDialer != nil {
			dialer = proxyDialer
		}
	}
	return &utlsRoundTripper{
		connections: make(map[string]*http2.ClientConn),
		pending:     make(map[string]*sync.Cond),
		dialer:      dialer,
	}
}

func (t *utlsRoundTripper) getOrCreateConnection(host, addr string) (*http2.ClientConn, error) {
	t.mu.Lock()

	if h2Conn, ok := t.connections[host]; ok && h2Conn.CanTakeNewRequest() {
		t.mu.Unlock()
		return h2Conn, nil
	}

	if cond, ok := t.pending[host]; ok {
		cond.Wait()
		if h2Conn, ok := t.connections[host]; ok && h2Conn.CanTakeNewRequest() {
			t.mu.Unlock()
			return h2Conn, nil
		}
	}

	cond := sync.NewCond(&t.mu)
	t.pending[host] = cond
	t.mu.Unlock()

	h2Conn, err := t.createConnection(host, addr)

	t.mu.Lock()
	defer t.mu.Unlock()

	delete(t.pending, host)
	cond.Broadcast()

	if err != nil {
		return nil, err
	}

	t.connections[host] = h2Conn
	return h2Conn, nil
}

func (t *utlsRoundTripper) createConnection(host, addr string) (*http2.ClientConn, error) {
	conn, err := t.dialer.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}

	tlsConfig := &tls.Config{ServerName: host}
	spec, errSpec := chromeH2ClientHelloSpec()
	if errSpec != nil {
		if errClose := conn.Close(); errClose != nil {
			log.Errorf("utls HTTP/2 connection close after ClientHello spec error: %v", errClose)
		}
		return nil, errSpec
	}
	tlsConn := tls.UClient(conn, tlsConfig, tls.HelloCustom)
	if errApply := tlsConn.ApplyPreset(&spec); errApply != nil {
		if errClose := conn.Close(); errClose != nil {
			log.Errorf("utls HTTP/2 connection close after ClientHello preset error: %v", errClose)
		}
		return nil, errApply
	}

	if err := tlsConn.Handshake(); err != nil {
		conn.Close()
		return nil, err
	}
	if negotiated := tlsConn.ConnectionState().NegotiatedProtocol; negotiated != "h2" {
		if errClose := tlsConn.Close(); errClose != nil {
			log.Errorf("utls HTTP/2 connection close after ALPN mismatch: %v", errClose)
		}
		return nil, fmt.Errorf("utls HTTP/2 negotiated ALPN %q, want h2", negotiated)
	}

	tr := &http2.Transport{}
	h2Conn, err := tr.NewClientConn(tlsConn)
	if err != nil {
		tlsConn.Close()
		return nil, err
	}

	return h2Conn, nil
}

func (t *utlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	hostname := req.URL.Hostname()
	port := req.URL.Port()
	if port == "" {
		port = "443"
	}
	addr := net.JoinHostPort(hostname, port)

	h2Conn, err := t.getOrCreateConnection(hostname, addr)
	if err != nil {
		return nil, err
	}

	resp, err := h2Conn.RoundTrip(req)
	if err != nil {
		t.mu.Lock()
		if cached, ok := t.connections[hostname]; ok && cached == h2Conn {
			delete(t.connections, hostname)
		}
		t.mu.Unlock()
		return nil, err
	}

	return resp, nil
}

type utlsHTTP11RoundTripper struct {
	transport http.RoundTripper
}

func newUtlsHTTP11RoundTripper(proxyURL string) http.RoundTripper {
	base := &http.Transport{
		ForceAttemptHTTP2: false,
		TLSNextProto:      make(map[string]func(authority string, c *cryptotls.Conn) http.RoundTripper),
		TLSClientConfig: &cryptotls.Config{
			NextProtos: []string{"http/1.1"},
		},
	}
	var dialer proxy.Dialer = proxy.Direct
	if proxyURL != "" {
		proxyDialer, mode, errBuild := proxyutil.BuildDialer(proxyURL)
		if errBuild != nil {
			log.Errorf("utls: failed to configure HTTP/1.1 proxy dialer for %q: %v", proxyutil.Redact(proxyURL), errBuild)
		} else if mode != proxyutil.ModeInherit && proxyDialer != nil {
			dialer = proxyDialer
		}
	}
	base.DialTLSContext = func(_ context.Context, network, addr string) (net.Conn, error) {
		return dialUTLSHTTP11(dialer, network, addr)
	}
	return &utlsHTTP11RoundTripper{transport: base}
}

func dialUTLSHTTP11(dialer proxy.Dialer, network, addr string) (net.Conn, error) {
	conn, err := dialer.Dial(network, addr)
	if err != nil {
		return nil, err
	}

	host, _, errSplit := net.SplitHostPort(addr)
	if errSplit != nil {
		host = addr
	}
	tlsConfig := &tls.Config{
		ServerName: host,
		NextProtos: []string{"http/1.1"},
	}
	spec, errSpec := chromeHTTP11ClientHelloSpec()
	if errSpec != nil {
		if errClose := conn.Close(); errClose != nil {
			log.Errorf("utls HTTP/1.1 connection close after ClientHello spec error: %v", errClose)
		}
		return nil, errSpec
	}
	tlsConn := tls.UClient(conn, tlsConfig, tls.HelloCustom)
	if errApply := tlsConn.ApplyPreset(&spec); errApply != nil {
		if errClose := conn.Close(); errClose != nil {
			log.Errorf("utls HTTP/1.1 connection close after ClientHello preset error: %v", errClose)
		}
		return nil, errApply
	}
	if errHandshake := tlsConn.Handshake(); errHandshake != nil {
		if errClose := conn.Close(); errClose != nil {
			log.Errorf("utls HTTP/1.1 connection close after handshake error: %v", errClose)
		}
		return nil, errHandshake
	}
	return tlsConn, nil
}

func chromeH2ClientHelloSpec() (tls.ClientHelloSpec, error) {
	return chromeClientHelloSpecWithALPN([]string{"h2"}, false)
}

func chromeHTTP11ClientHelloSpec() (tls.ClientHelloSpec, error) {
	return chromeClientHelloSpecWithALPN([]string{"http/1.1"}, true)
}

func chromeClientHelloSpecWithALPN(alpn []string, dropApplicationSettings bool) (tls.ClientHelloSpec, error) {
	spec, err := tls.UTLSIdToSpec(tls.HelloChrome_Auto)
	if err != nil {
		return tls.ClientHelloSpec{}, err
	}

	extensions := spec.Extensions[:0]
	alpnSet := false
	for _, ext := range spec.Extensions {
		switch ext.(type) {
		case *tls.ALPNExtension:
			extensions = append(extensions, &tls.ALPNExtension{AlpnProtocols: append([]string(nil), alpn...)})
			alpnSet = true
		case *tls.ApplicationSettingsExtension, *tls.ApplicationSettingsExtensionNew:
			if dropApplicationSettings {
				continue
			}
			extensions = append(extensions, ext)
		default:
			extensions = append(extensions, ext)
		}
	}
	if !alpnSet {
		extensions = append(extensions, &tls.ALPNExtension{AlpnProtocols: append([]string(nil), alpn...)})
	}
	spec.Extensions = extensions
	return spec, nil
}

func (t *utlsHTTP11RoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.transport.RoundTrip(req)
}

// utlsProtectedHosts contains the hosts that should use utls Chrome TLS fingerprint
// to bypass Cloudflare's TLS fingerprinting.
var utlsProtectedHosts = map[string]struct{}{
	"api.anthropic.com": {},
	"chatgpt.com":       {},
}

// utlsH2DegradeInitialTTL is the first window during which h2 is skipped after a
// transport error; utlsH2DegradeMaxTTL caps the exponential backoff between
// reprobes so h2 is retried at least every 30 minutes.
const (
	utlsH2DegradeInitialTTL = 2 * time.Minute
	utlsH2DegradeMaxTTL     = 30 * time.Minute
)

// utlsH2DegradeState records how long h2 stays skipped for one cache key plus
// the TTL used last time, so the next failure can back off exponentially.
type utlsH2DegradeState struct {
	until time.Time
	ttl   time.Duration
}

// utlsH2DegradeCacheStore is a process-wide cache of protected hosts whose h2
// transport is currently failing. NewUtlsHTTPClient builds a fresh client per
// upstream request, so this state must live outside the client to let later
// requests skip the known-bad h2 path. State is memory only and never persisted.
type utlsH2DegradeCacheStore struct {
	mu     sync.Mutex
	states map[string]utlsH2DegradeState
	now    func() time.Time
}

var utlsH2DegradeCache = &utlsH2DegradeCacheStore{
	states: make(map[string]utlsH2DegradeState),
	now:    time.Now,
}

// shouldSkip reports whether h2 is still within an active degrade window for key.
func (c *utlsH2DegradeCacheStore) shouldSkip(key string) bool {
	if c == nil || key == "" {
		return false
	}
	now := c.now()

	c.mu.Lock()
	defer c.mu.Unlock()

	state, ok := c.states[key]
	return ok && now.Before(state.until)
}

// recordFailure opens or extends the degrade window for key, doubling the TTL up
// to utlsH2DegradeMaxTTL on repeated failures.
func (c *utlsH2DegradeCacheStore) recordFailure(key string) {
	if c == nil || key == "" {
		return
	}
	now := c.now()

	c.mu.Lock()
	defer c.mu.Unlock()

	ttl := utlsH2DegradeInitialTTL
	if state, ok := c.states[key]; ok && state.ttl > 0 {
		ttl = min(state.ttl*2, utlsH2DegradeMaxTTL)
	}
	c.states[key] = utlsH2DegradeState{
		until: now.Add(ttl),
		ttl:   ttl,
	}
}

// recordSuccess clears any degrade window for key after h2 returns a response.
func (c *utlsH2DegradeCacheStore) recordSuccess(key string) {
	if c == nil || key == "" {
		return
	}

	c.mu.Lock()
	delete(c.states, key)
	c.mu.Unlock()
}

// fallbackRoundTripper uses utls for protected HTTPS hosts and falls back to
// standard transport for all other requests.
type fallbackRoundTripper struct {
	utls                    http.RoundTripper
	protectedHTTP11         http.RoundTripper
	protectedFallbackHTTP11 http.RoundTripper
	fallback                http.RoundTripper
	h2DegradeScope          string
}

// utlsH2DegradeScope returns the cache scope for h2 degrade state. Direct and
// proxied outbound paths get distinct scopes; the proxy endpoint is redacted so
// credentials never enter cache keys. An injected context RoundTripper is a
// custom transport and must not poison global h2 protocol state, so it gets an
// empty scope that disables the cache.
func utlsH2DegradeScope(proxyURL string, hasInjectedRoundTripper bool) string {
	if proxyURL != "" {
		return "proxy:" + proxyutil.Redact(proxyURL)
	}
	if hasInjectedRoundTripper {
		return ""
	}
	return "direct"
}

// h2DegradeKey combines the transport scope with the request hostname. An empty
// scope yields an empty key, which disables the degrade cache for this client.
func (f *fallbackRoundTripper) h2DegradeKey(hostname string) string {
	if f == nil || f.h2DegradeScope == "" {
		return ""
	}
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	if hostname == "" {
		return ""
	}
	return f.h2DegradeScope + "\x00" + hostname
}

func (f *fallbackRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == "https" {
		hostname := strings.ToLower(req.URL.Hostname())
		if _, ok := utlsProtectedHosts[hostname]; ok {
			return f.roundTripProtected(req)
		}
	}
	return f.fallback.RoundTrip(req)
}

func (f *fallbackRoundTripper) roundTripProtected(req *http.Request) (*http.Response, error) {
	h2Key := f.h2DegradeKey(req.URL.Hostname())
	transports := []struct {
		transport http.RoundTripper
		h2        bool
	}{
		{transport: f.utls, h2: true},
		{transport: f.protectedHTTP11},
		{transport: f.protectedFallbackHTTP11},
	}

	var lastErr error
	// sentAttempts counts transports actually invoked, not the loop index, so a
	// skipped h2 leaves HTTP/1.1 as the first real send and avoids needing GetBody.
	sentAttempts := 0
	for _, candidate := range transports {
		if candidate.transport == nil {
			continue
		}
		if candidate.h2 && utlsH2DegradeCache.shouldSkip(h2Key) {
			continue
		}

		attemptReq, errReq := requestForProtectedAttempt(req, sentAttempts)
		if errReq != nil {
			return nil, errReq
		}
		resp, err := candidate.transport.RoundTrip(attemptReq)
		sentAttempts++

		if candidate.h2 {
			// A non-nil response means the h2 transport works regardless of HTTP
			// status, so clear the degrade window. Only a response-less transport
			// error degrades h2.
			if resp != nil {
				utlsH2DegradeCache.recordSuccess(h2Key)
			} else if err != nil {
				utlsH2DegradeCache.recordFailure(h2Key)
			}
		}

		if resp != nil {
			return resp, err
		}
		if err != nil {
			lastErr = err
			continue
		}
		return nil, nil
	}
	return nil, lastErr
}

func requestForProtectedAttempt(req *http.Request, attempt int) (*http.Request, error) {
	if attempt == 0 || req.Body == nil || req.Body == http.NoBody {
		return req, nil
	}
	if req.GetBody == nil {
		return nil, fmt.Errorf("utls protected transport fallback requires request body rewind")
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, fmt.Errorf("utls protected transport fallback rewind request body: %w", err)
	}
	clone := req.Clone(req.Context())
	clone.Body = body
	return clone, nil
}

// standardHTTP11Transport returns a clone of base that negotiates HTTP/1.1 only.
// Round-trippers that are not *http.Transport (e.g. an injected context
// RoundTripper or a uTLS round-tripper) are returned unchanged. Proxy and dial
// settings on base are preserved; only ALPN/protocol negotiation is pinned.
// The base is cloned, never mutated, so the shared http.DefaultTransport global
// is left intact.
func standardHTTP11Transport(base http.RoundTripper) http.RoundTripper {
	tr, ok := base.(*http.Transport)
	if !ok || tr == nil {
		return base
	}
	clone := tr.Clone()
	clone.ForceAttemptHTTP2 = false
	// Wipe TLSNextProto to prevent an implicit HTTP/2 upgrade.
	clone.TLSNextProto = make(map[string]func(authority string, c *cryptotls.Conn) http.RoundTripper)
	if clone.TLSClientConfig == nil {
		clone.TLSClientConfig = &cryptotls.Config{}
	} else {
		clone.TLSClientConfig = clone.TLSClientConfig.Clone()
	}
	// Actively advertise only HTTP/1.1 in the ALPN handshake.
	clone.TLSClientConfig.NextProtos = []string{"http/1.1"}
	return clone
}

// NewUtlsHTTPClient creates an HTTP client using utls Chrome TLS fingerprint.
// Use this for provider requests that need a Chrome-like TLS fingerprint.
// Falls back to standard transport for non-HTTPS requests.
func NewUtlsHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	var proxyURL string
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}

	var ctxRoundTripper http.RoundTripper
	if ctx != nil {
		ctxRoundTripper, _ = ctx.Value("cliproxy.roundtripper").(http.RoundTripper)
	}

	var utlsRT http.RoundTripper = newUtlsRoundTripper(proxyURL)
	var protectedHTTP11Transport http.RoundTripper = newUtlsHTTP11RoundTripper(proxyURL)
	var standardTransport http.RoundTripper = http.DefaultTransport
	if proxyURL != "" {
		if transport := buildProxyTransport(proxyURL); transport != nil {
			standardTransport = transport
		}
	} else if ctxRoundTripper != nil {
		utlsRT = ctxRoundTripper
		protectedHTTP11Transport = ctxRoundTripper
		standardTransport = ctxRoundTripper
	}
	protectedFallbackHTTP11Transport := standardHTTP11Transport(standardTransport)
	h2DegradeScope := utlsH2DegradeScope(proxyURL, ctxRoundTripper != nil)

	client := &http.Client{
		Transport: &fallbackRoundTripper{
			utls:                    utlsRT,
			protectedHTTP11:         protectedHTTP11Transport,
			protectedFallbackHTTP11: protectedFallbackHTTP11Transport,
			fallback:                standardTransport,
			h2DegradeScope:          h2DegradeScope,
		},
	}
	if timeout > 0 {
		client.Timeout = timeout
	}
	return client
}

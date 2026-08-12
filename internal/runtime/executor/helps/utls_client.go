package helps

import (
	"context"
	cryptotls "crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	tls "github.com/refraction-networking/utls"
	internalcache "github.com/router-for-me/CLIProxyAPI/v7/internal/cache"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/httpwire"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

const (
	// defaultUtlsH2MaxConnsPerHost is the per-host HTTP/2 connection pool size for
	// protected hosts. One conn caps out around the peer SETTINGS max concurrent
	// streams (~100); multiple conns are required for higher fan-out.
	defaultUtlsH2MaxConnsPerHost = 8

	// HTTP/1.1 pool defaults for the protected-host fallback path. Go's zero-value
	// MaxIdleConnsPerHost resolves to only 2, which thrashes TLS under concurrency.
	defaultUtlsHTTP11MaxIdleConns        = 256
	defaultUtlsHTTP11MaxIdleConnsPerHost = 64
	defaultUtlsHTTP11MaxConnsPerHost     = 128
	defaultUtlsHTTP11IdleConnTimeout     = 90 * time.Second
)

// utlsH2HostPool holds the live HTTP/2 client conns for a single host.
type utlsH2HostPool struct {
	conns   []*http2.ClientConn
	dialing int
	cond    *sync.Cond
}

// utlsRoundTripper implements http.RoundTripper using a Chrome fingerprint for
// providers that require a browser-like TLS and HTTP/2 transport.
//
// Connections are pooled per host up to maxConnsPerHost. When every live conn is
// at its stream limit, RoundTrip still reuses a busy conn (http2 waits for a
// stream slot) instead of unbounded dial storms.
type utlsRoundTripper struct {
	mu              sync.Mutex
	pools           map[string]*utlsH2HostPool
	dialer          proxy.Dialer
	maxConnsPerHost int
	// newClientConn optionally overrides createConnection (tests only).
	newClientConn func(host, addr string) (*http2.ClientConn, error)
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
		pools:           make(map[string]*utlsH2HostPool),
		dialer:          dialer,
		maxConnsPerHost: defaultUtlsH2MaxConnsPerHost,
	}
}

func (t *utlsRoundTripper) hostPool(host string) *utlsH2HostPool {
	pool, ok := t.pools[host]
	if ok {
		return pool
	}
	pool = &utlsH2HostPool{}
	pool.cond = sync.NewCond(&t.mu)
	t.pools[host] = pool
	return pool
}

func (t *utlsRoundTripper) maxConns() int {
	if t == nil || t.maxConnsPerHost <= 0 {
		return defaultUtlsH2MaxConnsPerHost
	}
	return t.maxConnsPerHost
}

// pruneClosedConns drops closed/closing conns under t.mu. A closing HTTP/2
// connection may still have live streams, so removing it from the pool must not
// force-close it.
func (t *utlsRoundTripper) pruneClosedConns(pool *utlsH2HostPool) {
	if pool == nil || len(pool.conns) == 0 {
		return
	}
	kept := pool.conns[:0]
	for _, conn := range pool.conns {
		if conn == nil {
			continue
		}
		state := conn.State()
		if state.Closed || state.Closing {
			continue
		}
		kept = append(kept, conn)
	}
	pool.conns = kept
}

func utlsH2ConnLoad(state http2.ClientConnState) int {
	return state.StreamsActive + state.StreamsReserved + state.StreamsPending
}

// pickReadyConn reserves and returns a conn with an immediately available
// stream slot, if any.
func (t *utlsRoundTripper) pickReadyConn(pool *utlsH2HostPool) *http2.ClientConn {
	t.pruneClosedConns(pool)
	for i := 0; i < len(pool.conns); {
		conn := pool.conns[i]
		state := conn.State()
		load := utlsH2ConnLoad(state)
		if state.MaxConcurrentStreams == 0 && load > 0 {
			i++
			continue
		}
		if state.MaxConcurrentStreams != 0 && uint64(load) >= uint64(state.MaxConcurrentStreams) {
			i++
			continue
		}
		if conn.ReserveNewRequest() {
			return conn
		}
		pool.conns = append(pool.conns[:i], pool.conns[i+1:]...)
	}
	return nil
}

// pickAnyOpenConn reserves the least-loaded open conn. Client connections use
// StrictMaxConcurrentStreams, so RoundTrip waits for a stream slot when the pool
// is at capacity instead of failing or opening unbounded connections.
func (t *utlsRoundTripper) pickAnyOpenConn(pool *utlsH2HostPool) *http2.ClientConn {
	for {
		t.pruneClosedConns(pool)
		if len(pool.conns) == 0 {
			return nil
		}

		bestIndex := 0
		bestLoad := utlsH2ConnLoad(pool.conns[0].State())
		for i, conn := range pool.conns[1:] {
			load := utlsH2ConnLoad(conn.State())
			if load < bestLoad {
				bestIndex = i + 1
				bestLoad = load
			}
		}
		best := pool.conns[bestIndex]
		if best.ReserveNewRequest() {
			return best
		}
		pool.conns = append(pool.conns[:bestIndex], pool.conns[bestIndex+1:]...)
	}
}

func (t *utlsRoundTripper) removeConnIfUnusable(host string, target *http2.ClientConn) {
	if t == nil || target == nil {
		return
	}
	state := target.State()
	if !state.Closed && !state.Closing {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	pool, ok := t.pools[host]
	if !ok || pool == nil {
		return
	}
	for i, conn := range pool.conns {
		if conn != target {
			continue
		}
		pool.conns = append(pool.conns[:i], pool.conns[i+1:]...)
		break
	}
	pool.cond.Broadcast()
}

// waitForPoolChange waits on pool.cond while still allowing request cancellation.
// The caller must hold t.mu. context.AfterFunc broadcasts under the same lock, so
// cancellation cannot race between checking the context and entering Cond.Wait.
func (t *utlsRoundTripper) waitForPoolChange(ctx context.Context, pool *utlsH2HostPool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	stopBroadcast := context.AfterFunc(ctx, func() {
		t.mu.Lock()
		pool.cond.Broadcast()
		t.mu.Unlock()
	})
	pool.cond.Wait()
	stopBroadcast()
	return ctx.Err()
}

func (t *utlsRoundTripper) getOrCreateConnection(ctx context.Context, host, addr string) (*http2.ClientConn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	maxConns := t.maxConns()

	t.mu.Lock()
	defer t.mu.Unlock()

	for {
		if errCtx := ctx.Err(); errCtx != nil {
			return nil, errCtx
		}
		pool := t.hostPool(host)
		if conn := t.pickReadyConn(pool); conn != nil {
			return conn, nil
		}

		if len(pool.conns)+pool.dialing < maxConns {
			pool.dialing++
			t.mu.Unlock()
			conn, err := t.createConnection(ctx, host, addr)
			t.mu.Lock()
			pool.dialing--
			if err != nil {
				pool.cond.Broadcast()
				if ready := t.pickReadyConn(pool); ready != nil {
					return ready, nil
				}
				if open := t.pickAnyOpenConn(pool); open != nil {
					return open, nil
				}
				if pool.dialing > 0 {
					if errWait := t.waitForPoolChange(ctx, pool); errWait != nil {
						return nil, errWait
					}
					continue
				}
				return nil, err
			}
			if errCtx := ctx.Err(); errCtx != nil {
				_ = conn.Close()
				pool.cond.Broadcast()
				return nil, errCtx
			}
			if !conn.ReserveNewRequest() {
				_ = conn.Close()
				pool.cond.Broadcast()
				return nil, fmt.Errorf("utls HTTP/2: new connection unavailable for host %s", host)
			}
			pool.conns = append(pool.conns, conn)
			pool.cond.Broadcast()
			return conn, nil
		}

		// At capacity, reuse an open conn first. Strict HTTP/2 flow control waits
		// there with the request context instead of blocking on an unrelated dial.
		if conn := t.pickAnyOpenConn(pool); conn != nil {
			return conn, nil
		}
		if pool.dialing > 0 {
			if errWait := t.waitForPoolChange(ctx, pool); errWait != nil {
				return nil, errWait
			}
			continue
		}
		return nil, fmt.Errorf("utls HTTP/2: no connection available for host %s", host)
	}
}

func (t *utlsRoundTripper) createConnection(ctx context.Context, host, addr string) (*http2.ClientConn, error) {
	if t != nil && t.newClientConn != nil {
		return t.newClientConn(host, addr)
	}

	contextDialer, ok := t.dialer.(proxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("utls: dialer does not support context cancellation")
	}
	conn, errDial := contextDialer.DialContext(ctx, "tcp", addr)
	if errDial != nil {
		return nil, fmt.Errorf("utls: dial upstream: %w", errDial)
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

	if errHandshake := tlsConn.HandshakeContext(ctx); errHandshake != nil {
		if errors.Is(errHandshake, context.Canceled) || errors.Is(errHandshake, context.DeadlineExceeded) {
			return nil, fmt.Errorf("utls: TLS handshake: %w", errHandshake)
		}
		if errClose := conn.Close(); errClose != nil {
			return nil, fmt.Errorf("utls: TLS handshake: %w; close connection: %v", errHandshake, errClose)
		}
		return nil, fmt.Errorf("utls: TLS handshake: %w", errHandshake)
	}
	if negotiated := tlsConn.ConnectionState().NegotiatedProtocol; negotiated != "h2" {
		if errClose := tlsConn.Close(); errClose != nil {
			log.Errorf("utls HTTP/2 connection close after ALPN mismatch: %v", errClose)
		}
		return nil, fmt.Errorf("utls HTTP/2 negotiated ALPN %q, want h2", negotiated)
	}

	tr := &http2.Transport{StrictMaxConcurrentStreams: true}
	h2Conn, errClientConn := tr.NewClientConn(tlsConn)
	if errClientConn != nil {
		if errClose := tlsConn.Close(); errClose != nil {
			return nil, fmt.Errorf("utls: initialize HTTP/2 connection: %w; close TLS connection: %v", errClientConn, errClose)
		}
		return nil, fmt.Errorf("utls: initialize HTTP/2 connection: %w", errClientConn)
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

	h2Conn, err := t.getOrCreateConnection(req.Context(), hostname, addr)
	if err != nil {
		return nil, err
	}

	resp, err := h2Conn.RoundTrip(req)
	if err != nil {
		t.removeConnIfUnusable(hostname, h2Conn)
		return nil, err
	}
	if resp == nil {
		t.removeConnIfUnusable(hostname, h2Conn)
		return nil, fmt.Errorf("utls: upstream returned an empty response")
	}
	if resp.Body == nil {
		resp.Body = http.NoBody
	}
	return resp, nil
}

type utlsHTTP11RoundTripper struct {
	transport http.RoundTripper
}

func applyUtlsHTTP11PoolSettings(tr *http.Transport) {
	if tr == nil {
		return
	}
	if tr.MaxIdleConns < defaultUtlsHTTP11MaxIdleConns {
		tr.MaxIdleConns = defaultUtlsHTTP11MaxIdleConns
	}
	if tr.MaxIdleConnsPerHost < defaultUtlsHTTP11MaxIdleConnsPerHost {
		tr.MaxIdleConnsPerHost = defaultUtlsHTTP11MaxIdleConnsPerHost
	}
	if tr.MaxConnsPerHost == 0 || tr.MaxConnsPerHost > defaultUtlsHTTP11MaxConnsPerHost {
		// Cap runaway dials on the H1 fallback path. Zero means unlimited in net/http.
		tr.MaxConnsPerHost = defaultUtlsHTTP11MaxConnsPerHost
	}
	if tr.IdleConnTimeout == 0 {
		tr.IdleConnTimeout = defaultUtlsHTTP11IdleConnTimeout
	}
}

func newUtlsHTTP11RoundTripper(proxyURL string) http.RoundTripper {
	base := &http.Transport{
		ForceAttemptHTTP2:   false,
		MaxIdleConns:        defaultUtlsHTTP11MaxIdleConns,
		MaxIdleConnsPerHost: defaultUtlsHTTP11MaxIdleConnsPerHost,
		MaxConnsPerHost:     defaultUtlsHTTP11MaxConnsPerHost,
		IdleConnTimeout:     defaultUtlsHTTP11IdleConnTimeout,
		TLSNextProto:        make(map[string]func(authority string, c *cryptotls.Conn) http.RoundTripper),
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

// utlsProtectedHosts contains hosts that use the adaptive Chrome HTTP/2
// transport. Anthropic uses the separate Claude Code HTTP/1.1 wire profile.
var utlsProtectedHosts = map[string]struct{}{
	"chatgpt.com": {},
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
// transport is currently failing. State is memory only and never persisted.
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

// fallbackRoundTripper uses Claude Code's wire profile for Anthropic, the
// adaptive Chrome transport for other protected hosts, and the standard
// transport for everything else.
type fallbackRoundTripper struct {
	anthropic               http.RoundTripper
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
	if IsAnthropicUpstreamURL(req.URL) {
		return f.anthropic.RoundTrip(req)
	}
	if req.URL.Scheme == "https" {
		hostname := strings.ToLower(req.URL.Hostname())
		if _, ok := utlsProtectedHosts[hostname]; ok {
			return f.roundTripProtected(req)
		}
	}
	return f.fallback.RoundTrip(req)
}

// claudeCodeSessionCacheCapacity bounds the per-transport TLS session cache for
// the Anthropic inference plane.
const claudeCodeSessionCacheCapacity = 32

// newClaudeCodeTLSConfig builds the uTLS config for one inference-plane dial.
//
// OmitEmptyPsk keeps the pre_shared_key extension silent until a session is
// cached, so an unresumed ClientHello stays byte-identical to the captured
// native handshake. PreferSkipResumptionOnNilExtension turns uTLS's HelloCustom
// "resume without the matching extension" panic into a skipped resumption.
func newClaudeCodeTLSConfig(host string, sessionCache tls.ClientSessionCache) *tls.Config {
	return &tls.Config{
		ServerName:                         host,
		ClientSessionCache:                 sessionCache,
		OmitEmptyPsk:                       true,
		PreferSkipResumptionOnNilExtension: true,
	}
}

// claudeCodeTLSClientHelloSpec reproduces the deterministic Node/OpenSSL
// ClientHello emitted by Claude Code 2.1.220 on macOS arm64. Keep this spec in
// sync with a fresh native capture whenever the advertised Claude Code version
// changes.
func claudeCodeTLSClientHelloSpec() *tls.ClientHelloSpec {
	return &tls.ClientHelloSpec{
		CipherSuites: []uint16{
			tls.TLS_AES_128_GCM_SHA256,
			tls.TLS_AES_256_GCM_SHA384,
			tls.TLS_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
			tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
			tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
			tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_RSA_WITH_AES_128_CBC_SHA,
			tls.TLS_RSA_WITH_AES_256_CBC_SHA,
		},
		CompressionMethods: []uint8{0},
		Extensions: []tls.TLSExtension{
			&tls.SNIExtension{},
			&tls.ExtendedMasterSecretExtension{},
			&tls.RenegotiationInfoExtension{Renegotiation: tls.RenegotiateOnceAsClient},
			&tls.SupportedCurvesExtension{Curves: []tls.CurveID{tls.X25519, tls.CurveP256, tls.CurveP384}},
			&tls.SupportedPointsExtension{SupportedPoints: []byte{0}},
			&tls.SessionTicketExtension{},
			&tls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},
			&tls.StatusRequestExtension{},
			&tls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []tls.SignatureScheme{
				tls.ECDSAWithP256AndSHA256,
				tls.PSSWithSHA256,
				tls.PKCS1WithSHA256,
				tls.ECDSAWithP384AndSHA384,
				tls.PSSWithSHA384,
				tls.PKCS1WithSHA384,
				tls.PSSWithSHA512,
				tls.PKCS1WithSHA512,
				tls.PKCS1WithSHA1,
			}},
			&tls.SCTExtension{},
			&tls.KeyShareExtension{KeyShares: []tls.KeyShare{{Group: tls.X25519}}},
			&tls.PSKKeyExchangeModesExtension{Modes: []uint8{tls.PskModeDHE}},
			&tls.SupportedVersionsExtension{Versions: []uint16{tls.VersionTLS13, tls.VersionTLS12}},
			&tls.UtlsPaddingExtension{GetPaddingLen: tls.BoringPaddingStyle},
			// pre_shared_key MUST be the final extension (RFC 8446 4.2.11), after
			// padding. It contributes zero bytes until a cached session exists.
			&tls.UtlsPreSharedKeyExtension{},
		},
	}
}

const claudeCodeRoundTripperCacheCapacity = 64

var claudeCodeRoundTripperCache = internalcache.NewBoundedLRU[string, http.RoundTripper](
	claudeCodeRoundTripperCacheCapacity,
	func(_ string, roundTripper http.RoundTripper) {
		if transport, ok := roundTripper.(interface{ CloseIdleConnections() }); ok {
			transport.CloseIdleConnections()
		}
	},
)

var claudeCodeMessagesHeaderOrder = []string{
	"Accept",
	"Authorization",
	"Content-Type",
	"User-Agent",
	"X-Claude-Code-Session-Id",
	"X-Stainless-Arch",
	"X-Stainless-Lang",
	"X-Stainless-OS",
	"X-Stainless-Package-Version",
	"X-Stainless-Retry-Count",
	"X-Stainless-Runtime",
	"X-Stainless-Runtime-Version",
	"X-Stainless-Timeout",
	"anthropic-beta",
	"anthropic-dangerous-direct-browser-access",
	"anthropic-version",
	"x-app",
	"x-client-request-id",
	"Connection",
	"Host",
	"Accept-Encoding",
	"Content-Length",
}

var claudeCodeCountTokensHeaderOrder = []string{
	"Accept",
	"Authorization",
	"Content-Type",
	"User-Agent",
	"X-Claude-Code-Session-Id",
	"X-Stainless-Arch",
	"X-Stainless-Lang",
	"X-Stainless-OS",
	"X-Stainless-Package-Version",
	"X-Stainless-Retry-Count",
	"X-Stainless-Runtime",
	"X-Stainless-Runtime-Version",
	"anthropic-beta",
	"anthropic-dangerous-direct-browser-access",
	"anthropic-version",
	"x-app",
	"x-client-request-id",
	"Connection",
	"Host",
	"Accept-Encoding",
	"Content-Length",
}

func claudeCodeRequestHeaderOrder(_, requestTarget string) []string {
	if strings.HasPrefix(requestTarget, "/v1/messages/count_tokens") {
		return claudeCodeCountTokensHeaderOrder
	}
	return claudeCodeMessagesHeaderOrder
}

func cachedClaudeCodeRoundTripper(proxyURL string) http.RoundTripper {
	return claudeCodeRoundTripperCache.GetOrAdd(proxyURL, func() http.RoundTripper {
		return newClaudeCodeRoundTripper(proxyURL)
	})
}

func newClaudeCodeRoundTripper(proxyURL string) http.RoundTripper {
	// The cache is scoped to this round tripper, which is already keyed by proxy,
	// so resumption never crosses proxy boundaries.
	sessionCache := tls.NewLRUClientSessionCache(claudeCodeSessionCacheCapacity)
	var dialer proxy.Dialer = proxy.Direct
	if proxyURL != "" {
		proxyDialer, mode, errBuild := proxyutil.BuildDialer(proxyURL)
		if errBuild != nil {
			log.Errorf("claude tls: failed to configure proxy dialer for %q: %v", proxyutil.Redact(proxyURL), errBuild)
		} else if mode != proxyutil.ModeInherit && proxyDialer != nil {
			dialer = proxyDialer
		}
	}

	transport := &http.Transport{
		ForceAttemptHTTP2: false,
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var (
				conn net.Conn
				err  error
			)
			if contextDialer, ok := dialer.(proxy.ContextDialer); ok {
				conn, err = contextDialer.DialContext(ctx, network, addr)
			} else {
				conn, err = dialer.Dial(network, addr)
			}
			if err != nil {
				return nil, fmt.Errorf("claude tls: dial upstream: %w", err)
			}

			host, _, errSplit := net.SplitHostPort(addr)
			if errSplit != nil {
				if errClose := conn.Close(); errClose != nil {
					log.Debugf("claude tls: close failed connection: %v", errClose)
				}
				return nil, fmt.Errorf("claude tls: split upstream address: %w", errSplit)
			}
			tlsConn := tls.UClient(conn, newClaudeCodeTLSConfig(host, sessionCache), tls.HelloCustom)
			if errPreset := tlsConn.ApplyPreset(claudeCodeTLSClientHelloSpec()); errPreset != nil {
				if errClose := tlsConn.Close(); errClose != nil {
					log.Debugf("claude tls: close connection after preset failure: %v", errClose)
				}
				return nil, fmt.Errorf("claude tls: apply Claude Code ClientHello: %w", errPreset)
			}
			if errHandshake := tlsConn.HandshakeContext(ctx); errHandshake != nil {
				if errClose := tlsConn.Close(); errClose != nil {
					log.Debugf("claude tls: close connection after handshake failure: %v", errClose)
				}
				return nil, fmt.Errorf("claude tls: handshake upstream: %w", errHandshake)
			}
			return httpwire.NewOrderedRequestConn(tlsConn, claudeCodeRequestHeaderOrder), nil
		},
	}
	return transport
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
	applyUtlsHTTP11PoolSettings(clone)
	return clone
}

type utlsClientTransportCacheStore struct {
	mu         sync.Mutex
	transports map[string]http.RoundTripper
}

var utlsClientTransportCache = &utlsClientTransportCacheStore{
	transports: make(map[string]http.RoundTripper),
}

func (c *utlsClientTransportCacheStore) transport(proxyURL, authScope string) http.RoundTripper {
	if c == nil {
		return newUtlsFallbackRoundTripper(proxyURL, nil)
	}
	key := utlsClientTransportCacheKey(proxyURL, authScope)

	c.mu.Lock()
	defer c.mu.Unlock()

	if transport := c.transports[key]; transport != nil {
		return transport
	}
	transport := newUtlsFallbackRoundTripper(proxyURL, nil)
	c.transports[key] = transport
	return transport
}

func utlsClientTransportCacheKey(proxyURL, authScope string) string {
	if authScope == "" {
		authScope = "anonymous"
	}
	if proxyURL == "" {
		return "direct\x00" + authScope
	}
	return "proxy\x00" + proxyURL + "\x00" + authScope
}

func utlsClientTransportScope(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return "anonymous"
	}
	if id := strings.TrimSpace(auth.ID); id != "" {
		return "auth-id\x00" + id
	}
	if index := strings.TrimSpace(auth.Index); index != "" {
		return "auth-index\x00" + index
	}
	return "anonymous"
}

func newUtlsFallbackRoundTripper(proxyURL string, ctxRoundTripper http.RoundTripper) *fallbackRoundTripper {
	hasInjectedRoundTripper := proxyURL == "" && ctxRoundTripper != nil
	var anthropicRT http.RoundTripper = cachedClaudeCodeRoundTripper(proxyURL)
	var utlsRT http.RoundTripper = newUtlsRoundTripper(proxyURL)
	var protectedHTTP11Transport http.RoundTripper = newUtlsHTTP11RoundTripper(proxyURL)
	var standardTransport http.RoundTripper = http.DefaultTransport
	if proxyURL != "" {
		if transport := buildProxyTransport(proxyURL); transport != nil {
			standardTransport = transport
		}
	} else if hasInjectedRoundTripper {
		anthropicRT = ctxRoundTripper
		utlsRT = ctxRoundTripper
		protectedHTTP11Transport = ctxRoundTripper
		standardTransport = ctxRoundTripper
	}
	protectedFallbackHTTP11Transport := standardHTTP11Transport(standardTransport)
	h2DegradeScope := utlsH2DegradeScope(proxyURL, hasInjectedRoundTripper)

	return &fallbackRoundTripper{
		anthropic:               anthropicRT,
		utls:                    utlsRT,
		protectedHTTP11:         protectedHTTP11Transport,
		protectedFallbackHTTP11: protectedFallbackHTTP11Transport,
		fallback:                standardTransport,
		h2DegradeScope:          h2DegradeScope,
	}
}

// NewUtlsHTTPClient creates an HTTP client using provider-specific TLS
// fingerprints for protected hosts. It uses Claude Code's Node/OpenSSL profile
// for Anthropic and a Chrome profile for ChatGPT, with a standard-transport
// fallback for other hosts.
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

	var transport http.RoundTripper
	if proxyURL == "" && ctxRoundTripper != nil {
		transport = newUtlsFallbackRoundTripper(proxyURL, ctxRoundTripper)
	} else {
		transport = utlsClientTransportCache.transport(proxyURL, utlsClientTransportScope(auth))
	}

	client := &http.Client{
		Transport: transport,
	}
	if timeout > 0 {
		client.Timeout = timeout
	}
	return client
}

package helps

import (
	"bufio"
	"context"
	cryptotls "crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type utlsClientRoundTripFunc func(*http.Request) (*http.Response, error)

func (f utlsClientRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestNewUtlsHTTPClientUsesContextRoundTripperForProtectedHost(t *testing.T) {
	t.Setenv(codexTransportModeEnv, "")

	called := false
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		called = true
		if req.URL.Hostname() != "chatgpt.com" {
			t.Fatalf("hostname = %q, want chatgpt.com", req.URL.Hostname())
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("{}")),
			Request:    req,
		}, nil
	}))

	client := NewUtlsHTTPClient(ctx, nil, nil, 0)
	resp, err := client.Get("https://chatgpt.com/backend-api/codex/responses")
	if err != nil {
		t.Fatalf("client.Get returned error: %v", err)
	}
	if errClose := resp.Body.Close(); errClose != nil {
		t.Fatalf("response body close returned error: %v", errClose)
	}
	if !called {
		t.Fatal("expected context RoundTripper to handle protected host request")
	}
}

func TestNewUtlsHTTPClientCodexHTTP1UsesProtectedHTTP11Transport(t *testing.T) {
	t.Setenv(codexTransportModeEnv, codexTransportModeHTTP1)

	var protectedH2Called bool
	var protectedHTTP11Called bool
	client := &http.Client{
		Transport: &fallbackRoundTripper{
			utls: utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				protectedH2Called = true
				if req.URL.Hostname() != "chatgpt.com" {
					t.Fatalf("hostname = %q, want chatgpt.com", req.URL.Hostname())
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("{}")),
					Request:    req,
				}, nil
			}),
			protectedHTTP11: utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				protectedHTTP11Called = true
				if req.URL.Hostname() != "chatgpt.com" {
					t.Fatalf("hostname = %q, want chatgpt.com", req.URL.Hostname())
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("{}")),
					Request:    req,
				}, nil
			}),
		},
	}

	resp, err := client.Get("https://chatgpt.com/backend-api/codex/responses")
	if err != nil {
		t.Fatalf("client.Get returned error: %v", err)
	}
	if errClose := resp.Body.Close(); errClose != nil {
		t.Fatalf("response body close returned error: %v", errClose)
	}
	if protectedH2Called {
		t.Fatal("did not expect http1 mode to use protected HTTP/2 path")
	}
	if !protectedHTTP11Called {
		t.Fatal("expected http1 mode to use protected HTTP/1.1 path")
	}
}

func TestNewUtlsHTTPClientCodexStandardHTTP1BypassesProtectedTransport(t *testing.T) {
	t.Setenv(codexTransportModeEnv, codexTransportModeStandardHTTP1)

	var utlsCalled bool
	var fallbackCalled bool
	client := &http.Client{
		Transport: &fallbackRoundTripper{
			utls: utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				utlsCalled = true
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("{}")),
					Request:    req,
				}, nil
			}),
			fallback: utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				fallbackCalled = true
				if req.URL.Hostname() != "chatgpt.com" {
					t.Fatalf("hostname = %q, want chatgpt.com", req.URL.Hostname())
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("{}")),
					Request:    req,
				}, nil
			}),
		},
	}

	resp, err := client.Get("https://chatgpt.com/backend-api/codex/responses")
	if err != nil {
		t.Fatalf("client.Get returned error: %v", err)
	}
	if errClose := resp.Body.Close(); errClose != nil {
		t.Fatalf("response body close returned error: %v", errClose)
	}
	if utlsCalled {
		t.Fatal("did not expect standard-http1 mode to use protected uTLS path")
	}
	if !fallbackCalled {
		t.Fatal("expected standard-http1 mode to use standard fallback path")
	}
}

func TestNewUtlsHTTPClientCodexHTTP1PinsProtectedTransportToHTTP11(t *testing.T) {
	t.Setenv(codexTransportModeEnv, codexTransportModeHTTP1)

	client := NewUtlsHTTPClient(context.Background(), nil, nil, 0)
	fb, ok := client.Transport.(*fallbackRoundTripper)
	if !ok {
		t.Fatalf("client.Transport type = %T, want *fallbackRoundTripper", client.Transport)
	}
	protected, ok := fb.protectedHTTP11.(*utlsHTTP11RoundTripper)
	if !ok {
		t.Fatalf("protectedHTTP11 transport type = %T, want *utlsHTTP11RoundTripper", fb.protectedHTTP11)
	}
	tr, ok := protected.transport.(*http.Transport)
	if !ok {
		t.Fatalf("protectedHTTP11 inner transport type = %T, want *http.Transport", protected.transport)
	}
	if tr.ForceAttemptHTTP2 {
		t.Fatal("protectedHTTP11 ForceAttemptHTTP2 = true, want false")
	}
	if tr.TLSClientConfig == nil {
		t.Fatal("protectedHTTP11 TLSClientConfig = nil, want NextProtos pinned to http/1.1")
	}
	if got := tr.TLSClientConfig.NextProtos; len(got) != 1 || got[0] != "http/1.1" {
		t.Fatalf("protectedHTTP11 NextProtos = %v, want [http/1.1]", got)
	}
	if len(tr.TLSNextProto) != 0 {
		t.Fatalf("protectedHTTP11 TLSNextProto has %d entries, want 0", len(tr.TLSNextProto))
	}
}

func TestNewUtlsHTTPClientCodexHTTP1AdvertisesOnlyHTTP11ALPN(t *testing.T) {
	t.Setenv(codexTransportModeEnv, codexTransportModeHTTP1)

	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("net.Listen() error = %v", errListen)
	}
	defer func() {
		if errClose := listener.Close(); errClose != nil {
			t.Errorf("listener.Close() error = %v", errClose)
		}
	}()

	captured := make(chan []string, 1)
	proxyErr := make(chan error, 1)
	go serveALPNCaptureProxy(listener, captured, proxyErr)

	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://" + listener.Addr().String()}}
	client := NewUtlsHTTPClient(context.Background(), cfg, nil, 0)
	resp, errGet := client.Get("https://chatgpt.com/backend-api/codex/responses")
	if resp != nil {
		if errClose := resp.Body.Close(); errClose != nil {
			t.Fatalf("response body close returned error: %v", errClose)
		}
	}
	if errGet == nil {
		t.Fatal("client.Get() returned nil error, want TLS handshake failure after capturing ClientHello")
	}

	select {
	case protos := <-captured:
		want := []string{"http/1.1"}
		if strings.Join(protos, ",") != strings.Join(want, ",") {
			t.Fatalf("advertised ALPN protocols = %v, want %v", protos, want)
		}
	case errProxy := <-proxyErr:
		t.Fatalf("capture proxy error = %v", errProxy)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for ClientHello ALPN capture")
	}
}

func TestNewUtlsHTTPClientAutoAdvertisesOnlyH2ALPN(t *testing.T) {
	t.Setenv(codexTransportModeEnv, "")

	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("net.Listen() error = %v", errListen)
	}
	defer func() {
		if errClose := listener.Close(); errClose != nil {
			t.Errorf("listener.Close() error = %v", errClose)
		}
	}()

	captured := make(chan []string, 1)
	proxyErr := make(chan error, 1)
	go serveALPNCaptureProxy(listener, captured, proxyErr)

	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://" + listener.Addr().String()}}
	client := NewUtlsHTTPClient(context.Background(), cfg, nil, 0)
	resp, errGet := client.Get("https://chatgpt.com/backend-api/codex/responses")
	if resp != nil {
		if errClose := resp.Body.Close(); errClose != nil {
			t.Fatalf("response body close returned error: %v", errClose)
		}
	}
	if errGet == nil {
		t.Fatal("client.Get() returned nil error, want TLS handshake failure after capturing ClientHello")
	}

	select {
	case protos := <-captured:
		want := []string{"h2"}
		if strings.Join(protos, ",") != strings.Join(want, ",") {
			t.Fatalf("advertised ALPN protocols = %v, want %v", protos, want)
		}
	case errProxy := <-proxyErr:
		t.Fatalf("capture proxy error = %v", errProxy)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for ClientHello ALPN capture")
	}
}

func serveALPNCaptureProxy(listener net.Listener, captured chan<- []string, proxyErr chan<- error) {
	conn, errAccept := listener.Accept()
	if errAccept != nil {
		proxyErr <- errAccept
		return
	}
	defer func() { _ = conn.Close() }()

	req, errRead := http.ReadRequest(bufio.NewReader(conn))
	if errRead != nil {
		proxyErr <- fmt.Errorf("read CONNECT request failed: %w", errRead)
		return
	}
	if req.Method != http.MethodConnect {
		proxyErr <- fmt.Errorf("method = %s, want CONNECT", req.Method)
		return
	}
	if req.Host != "chatgpt.com:443" {
		proxyErr <- fmt.Errorf("host = %s, want chatgpt.com:443", req.Host)
		return
	}
	if _, errWrite := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); errWrite != nil {
		proxyErr <- fmt.Errorf("write CONNECT response failed: %w", errWrite)
		return
	}

	errStopHandshake := errors.New("stop after ClientHello")
	tlsConn := cryptotls.Server(conn, &cryptotls.Config{
		GetConfigForClient: func(hello *cryptotls.ClientHelloInfo) (*cryptotls.Config, error) {
			captured <- append([]string(nil), hello.SupportedProtos...)
			return nil, errStopHandshake
		},
	})
	if errHandshake := tlsConn.Handshake(); errHandshake != nil && !errors.Is(errHandshake, errStopHandshake) {
		proxyErr <- fmt.Errorf("TLS handshake failed: %w", errHandshake)
	}
}

// TestNewUtlsHTTPClientFallbackForcesHTTP11 verifies that requests to
// non-fingerprinted hosts (everything except the uTLS-protected Anthropic /
// ChatGPT domains) go through a fallback transport that negotiates HTTP/1.1
// only. Forcing HTTP/1.1 eliminates the HTTP/2 RST_STREAM(INTERNAL_ERROR)
// error class that flaky reseller upstreams emit mid-stream; a mid-response
// drop then surfaces as a clean EOF that the executor already terminates
// gracefully with a synthetic message_stop.
func TestNewUtlsHTTPClientFallbackForcesHTTP11(t *testing.T) {
	t.Parallel()

	client := NewUtlsHTTPClient(context.Background(), nil, nil, 0)

	fb, ok := client.Transport.(*fallbackRoundTripper)
	if !ok {
		t.Fatalf("client.Transport type = %T, want *fallbackRoundTripper", client.Transport)
	}

	tr, ok := fb.fallback.(*http.Transport)
	if !ok {
		t.Fatalf("fallback transport type = %T, want *http.Transport", fb.fallback)
	}

	if tr.ForceAttemptHTTP2 {
		t.Error("fallback ForceAttemptHTTP2 = true, want false (HTTP/1.1 only)")
	}
	if tr.TLSClientConfig == nil {
		t.Fatal("fallback TLSClientConfig = nil, want NextProtos pinned to http/1.1")
	}
	if got := tr.TLSClientConfig.NextProtos; len(got) != 1 || got[0] != "http/1.1" {
		t.Errorf("fallback TLSClientConfig.NextProtos = %v, want [http/1.1]", got)
	}
	if len(tr.TLSNextProto) != 0 {
		t.Errorf("fallback TLSNextProto has %d entries, want 0 (no implicit h2 upgrade)", len(tr.TLSNextProto))
	}

	// The shared global must not be mutated into HTTP/1.1; the client must own
	// an isolated clone.
	if def, ok := http.DefaultTransport.(*http.Transport); ok && !def.ForceAttemptHTTP2 {
		t.Error("http.DefaultTransport.ForceAttemptHTTP2 was flipped to false; expected an isolated clone, not mutation of the global")
	}
}

// TestForceHTTP11TransportNegotiatesHTTP11AgainstH2Server proves the real
// behavior: against a server that advertises HTTP/2 over ALPN, the forced
// transport still negotiates HTTP/1.1. The control assertion confirms the test
// server genuinely offers h2, so the HTTP/1.1 result is meaningful and not an
// artifact of the server only speaking HTTP/1.1.
func TestForceHTTP11TransportNegotiatesHTTP11AgainstH2Server(t *testing.T) {
	t.Parallel()

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	// srv.Client()'s transport trusts the test server's self-signed cert.
	base, ok := srv.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("test server client transport type = %T, want *http.Transport", srv.Client().Transport)
	}

	// Control: the unmodified client negotiates HTTP/2 with this server.
	controlResp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("control request returned error: %v", err)
	}
	controlProto := controlResp.ProtoMajor
	_ = controlResp.Body.Close()
	if controlProto != 2 {
		t.Fatalf("control negotiated HTTP/%d, want HTTP/2 (test server must offer h2 for this test to be meaningful)", controlProto)
	}

	// Forced: the same transport, pinned to HTTP/1.1, must negotiate HTTP/1.1.
	forced, ok := forceHTTP11Transport(base.Clone()).(*http.Transport)
	if !ok {
		t.Fatalf("forceHTTP11Transport returned %T, want *http.Transport", forceHTTP11Transport(base.Clone()))
	}
	client := &http.Client{Transport: forced}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("forced request returned error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.ProtoMajor != 1 {
		t.Fatalf("forced transport negotiated HTTP/%d, want HTTP/1.1", resp.ProtoMajor)
	}
}

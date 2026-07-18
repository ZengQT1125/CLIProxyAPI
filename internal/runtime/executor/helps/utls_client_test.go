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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"golang.org/x/net/http2"
)

type utlsClientRoundTripFunc func(*http.Request) (*http.Response, error)

func (f utlsClientRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func resetUtlsH2DegradeCacheForTest(t *testing.T, now func() time.Time) {
	t.Helper()

	utlsH2DegradeCache.mu.Lock()
	oldStates := utlsH2DegradeCache.states
	oldNow := utlsH2DegradeCache.now
	utlsH2DegradeCache.states = make(map[string]utlsH2DegradeState)
	utlsH2DegradeCache.now = now
	utlsH2DegradeCache.mu.Unlock()

	t.Cleanup(func() {
		utlsH2DegradeCache.mu.Lock()
		utlsH2DegradeCache.states = oldStates
		utlsH2DegradeCache.now = oldNow
		utlsH2DegradeCache.mu.Unlock()
	})
}

func utlsTestResponse(req *http.Request) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("{}")),
		Request:    req,
	}
}

func TestFallbackRoundTripperProtectedHostFallsBackAfterTransportErrors(t *testing.T) {
	for host := range utlsProtectedHosts {
		t.Run(host, func(t *testing.T) {
			var calls []string
			client := &http.Client{
				Transport: &fallbackRoundTripper{
					utls: utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
						calls = append(calls, "utls-h2")
						return nil, errors.New("h2 transport failed")
					}),
					protectedHTTP11: utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
						calls = append(calls, "utls-http1")
						return nil, errors.New("utls http1 transport failed")
					}),
					protectedFallbackHTTP11: utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
						calls = append(calls, "fallback-http1")
						return &http.Response{
							StatusCode: http.StatusTeapot,
							Header:     make(http.Header),
							Body:       io.NopCloser(strings.NewReader("{}")),
							Request:    req,
						}, nil
					}),
				},
			}

			resp, err := client.Get("https://" + host + "/backend-api/codex/responses")
			if err != nil {
				t.Fatalf("client.Get returned error: %v", err)
			}
			defer func() {
				if errClose := resp.Body.Close(); errClose != nil {
					t.Errorf("response body close returned error: %v", errClose)
				}
			}()
			if resp.StatusCode != http.StatusTeapot {
				t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusTeapot)
			}
			wantCalls := []string{"utls-h2", "utls-http1", "fallback-http1"}
			if strings.Join(calls, ",") != strings.Join(wantCalls, ",") {
				t.Fatalf("calls = %v, want %v", calls, wantCalls)
			}
		})
	}
}

func TestFallbackRoundTripperProtectedHostHTTPResponseStopsFallback(t *testing.T) {
	var calls []string
	client := &http.Client{
		Transport: &fallbackRoundTripper{
			utls: utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls = append(calls, "utls-h2")
				return &http.Response{
					StatusCode: http.StatusInternalServerError,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"error":"upstream"}`)),
					Request:    req,
				}, nil
			}),
			protectedHTTP11: utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls = append(calls, "utls-http1")
				return nil, errors.New("should not be called")
			}),
			protectedFallbackHTTP11: utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls = append(calls, "fallback-http1")
				return nil, errors.New("should not be called")
			}),
		},
	}

	resp, err := client.Get("https://chatgpt.com/backend-api/codex/responses")
	if err != nil {
		t.Fatalf("client.Get returned error: %v", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			t.Errorf("response body close returned error: %v", errClose)
		}
	}()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusInternalServerError)
	}
	wantCalls := []string{"utls-h2"}
	if strings.Join(calls, ",") != strings.Join(wantCalls, ",") {
		t.Fatalf("calls = %v, want %v", calls, wantCalls)
	}
}

func TestFallbackRoundTripperProtectedHostReplaysBodyOnFallback(t *testing.T) {
	var bodies []string
	readBody := func(req *http.Request) string {
		body, errRead := io.ReadAll(req.Body)
		if errRead != nil {
			t.Fatalf("ReadAll() error = %v", errRead)
		}
		return string(body)
	}
	client := &http.Client{
		Transport: &fallbackRoundTripper{
			utls: utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				bodies = append(bodies, readBody(req))
				return nil, errors.New("h2 transport failed")
			}),
			protectedHTTP11: utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				bodies = append(bodies, readBody(req))
				return nil, errors.New("utls http1 transport failed")
			}),
			protectedFallbackHTTP11: utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				bodies = append(bodies, readBody(req))
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("{}")),
					Request:    req,
				}, nil
			}),
		},
	}

	resp, err := client.Post("https://chatgpt.com/backend-api/codex/responses", "application/json", strings.NewReader(`{"input":"hello"}`))
	if err != nil {
		t.Fatalf("client.Post returned error: %v", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			t.Errorf("response body close returned error: %v", errClose)
		}
	}()
	wantBodies := []string{`{"input":"hello"}`, `{"input":"hello"}`, `{"input":"hello"}`}
	if strings.Join(bodies, "\n") != strings.Join(wantBodies, "\n") {
		t.Fatalf("bodies = %q, want %q", bodies, wantBodies)
	}
}

func TestNewUtlsHTTPClientUsesContextRoundTripperForProtectedHost(t *testing.T) {
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

func TestNewUtlsHTTPClientUsesContextRoundTripperForProtectedFallbacks(t *testing.T) {
	var calls int
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls < 3 {
			return nil, fmt.Errorf("injected transport failure %d", calls)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("{}")),
			Request:    req,
		}, nil
	}))

	client := NewUtlsHTTPClient(ctx, nil, nil, 0)
	resp, err := client.Post("https://chatgpt.com/backend-api/codex/responses", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("client.Post returned error: %v", err)
	}
	if errClose := resp.Body.Close(); errClose != nil {
		t.Fatalf("response body close returned error: %v", errClose)
	}
	if calls != 3 {
		t.Fatalf("context RoundTripper calls = %d, want 3", calls)
	}
}

func TestNewUtlsHTTPClientProtectedHTTP11TransportPinsHTTP11(t *testing.T) {
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
	assertUtlsHTTP11PoolSettings(t, "protectedHTTP11", tr)
}

func TestNewUtlsHTTPClientProtectedFallbackPinsStandardHTTP11(t *testing.T) {
	client := NewUtlsHTTPClient(context.Background(), nil, nil, 0)
	fb, ok := client.Transport.(*fallbackRoundTripper)
	if !ok {
		t.Fatalf("client.Transport type = %T, want *fallbackRoundTripper", client.Transport)
	}
	tr, ok := fb.protectedFallbackHTTP11.(*http.Transport)
	if !ok {
		t.Fatalf("protectedFallbackHTTP11 transport type = %T, want *http.Transport", fb.protectedFallbackHTTP11)
	}
	if tr.ForceAttemptHTTP2 {
		t.Fatal("protectedFallbackHTTP11 ForceAttemptHTTP2 = true, want false")
	}
	if tr.TLSClientConfig == nil {
		t.Fatal("protectedFallbackHTTP11 TLSClientConfig = nil, want NextProtos pinned to http/1.1")
	}
	if got := tr.TLSClientConfig.NextProtos; len(got) != 1 || got[0] != "http/1.1" {
		t.Fatalf("protectedFallbackHTTP11 NextProtos = %v, want [http/1.1]", got)
	}
	if len(tr.TLSNextProto) != 0 {
		t.Fatalf("protectedFallbackHTTP11 TLSNextProto has %d entries, want 0", len(tr.TLSNextProto))
	}
	assertUtlsHTTP11PoolSettings(t, "protectedFallbackHTTP11", tr)
}

func assertUtlsHTTP11PoolSettings(t *testing.T, name string, tr *http.Transport) {
	t.Helper()
	if tr.MaxIdleConns != defaultUtlsHTTP11MaxIdleConns {
		t.Fatalf("%s MaxIdleConns = %d, want %d", name, tr.MaxIdleConns, defaultUtlsHTTP11MaxIdleConns)
	}
	if tr.MaxIdleConnsPerHost != defaultUtlsHTTP11MaxIdleConnsPerHost {
		t.Fatalf("%s MaxIdleConnsPerHost = %d, want %d", name, tr.MaxIdleConnsPerHost, defaultUtlsHTTP11MaxIdleConnsPerHost)
	}
	if tr.MaxConnsPerHost != defaultUtlsHTTP11MaxConnsPerHost {
		t.Fatalf("%s MaxConnsPerHost = %d, want %d", name, tr.MaxConnsPerHost, defaultUtlsHTTP11MaxConnsPerHost)
	}
	if tr.IdleConnTimeout != defaultUtlsHTTP11IdleConnTimeout {
		t.Fatalf("%s IdleConnTimeout = %s, want %s", name, tr.IdleConnTimeout, defaultUtlsHTTP11IdleConnTimeout)
	}
}

func TestNewUtlsHTTPClientReusesReusableTransport(t *testing.T) {
	first := NewUtlsHTTPClient(context.Background(), nil, nil, time.Second)
	second := NewUtlsHTTPClient(context.Background(), nil, nil, 2*time.Second)

	if first == second {
		t.Fatal("clients unexpectedly share the same *http.Client")
	}
	if first.Transport == nil || second.Transport == nil {
		t.Fatalf("transports must be non-nil: first=%T second=%T", first.Transport, second.Transport)
	}
	if first.Transport != second.Transport {
		t.Fatalf("direct transports differ: first=%T %p second=%T %p", first.Transport, first.Transport, second.Transport, second.Transport)
	}
	if first.Timeout != time.Second {
		t.Fatalf("first timeout = %s, want %s", first.Timeout, time.Second)
	}
	if second.Timeout != 2*time.Second {
		t.Fatalf("second timeout = %s, want %s", second.Timeout, 2*time.Second)
	}

	authA := &cliproxyauth.Auth{ID: "codex-a"}
	authB := &cliproxyauth.Auth{ID: "codex-b"}
	firstAuth := NewUtlsHTTPClient(context.Background(), nil, authA, 0)
	secondAuth := NewUtlsHTTPClient(context.Background(), nil, authA, 0)
	otherAuth := NewUtlsHTTPClient(context.Background(), nil, authB, 0)

	if firstAuth.Transport != secondAuth.Transport {
		t.Fatalf("same auth transports differ: first=%T %p second=%T %p", firstAuth.Transport, firstAuth.Transport, secondAuth.Transport, secondAuth.Transport)
	}
	if firstAuth.Transport == otherAuth.Transport {
		t.Fatal("different auth transports unexpectedly share the same transport")
	}

	firstProxy := NewUtlsHTTPClient(context.Background(), nil, &cliproxyauth.Auth{ID: "codex-proxy", ProxyURL: "http://127.0.0.1:9"}, 0)
	secondProxy := NewUtlsHTTPClient(context.Background(), nil, &cliproxyauth.Auth{ID: "codex-proxy", ProxyURL: "http://127.0.0.1:9"}, 0)
	otherProxy := NewUtlsHTTPClient(context.Background(), nil, &cliproxyauth.Auth{ID: "codex-proxy", ProxyURL: "http://127.0.0.1:10"}, 0)

	if firstProxy.Transport != secondProxy.Transport {
		t.Fatalf("same proxy transports differ: first=%T %p second=%T %p", firstProxy.Transport, firstProxy.Transport, secondProxy.Transport, secondProxy.Transport)
	}
	if firstProxy.Transport == otherProxy.Transport {
		t.Fatal("different proxy transports unexpectedly share the same transport")
	}
}

func BenchmarkNewUtlsHTTPClient(b *testing.B) {
	ctx := context.Background()
	_ = NewUtlsHTTPClient(ctx, nil, nil, 0)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		client := NewUtlsHTTPClient(ctx, nil, nil, 0)
		if client.Transport == nil {
			b.Fatal("client transport is nil")
		}
	}
}

func TestUtlsHTTP11RoundTripperAdvertisesOnlyHTTP11ALPN(t *testing.T) {
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

	client := &http.Client{Transport: newUtlsHTTP11RoundTripper("http://" + listener.Addr().String())}
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

func TestUtlsRoundTripperAdvertisesOnlyH2ALPN(t *testing.T) {
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

	client := &http.Client{Transport: newUtlsRoundTripper("http://" + listener.Addr().String())}
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

func TestNewUtlsHTTPClientFallbackKeepsHTTP2Enabled(t *testing.T) {
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

	if !tr.ForceAttemptHTTP2 {
		t.Fatal("fallback ForceAttemptHTTP2 = false, want true")
	}
	if tr.TLSClientConfig != nil && strings.Join(tr.TLSClientConfig.NextProtos, ",") == "http/1.1" {
		t.Fatalf("fallback NextProtos = %v, want HTTP/2 capable default transport", tr.TLSClientConfig.NextProtos)
	}
}

func TestFallbackRoundTripperSkipsH2DuringDegradeWindowAcrossClients(t *testing.T) {
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	resetUtlsH2DegradeCacheForTest(t, func() time.Time { return now })

	scope := "test-direct"
	var firstCalls []string
	firstClient := &http.Client{Transport: &fallbackRoundTripper{
		h2DegradeScope: scope,
		utls: utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			firstCalls = append(firstCalls, "utls-h2")
			return nil, errors.New("h2 transport failed")
		}),
		protectedHTTP11: utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			firstCalls = append(firstCalls, "utls-http1")
			return utlsTestResponse(req), nil
		}),
	}}

	resp, err := firstClient.Get("https://chatgpt.com/backend-api/codex/responses")
	if err != nil {
		t.Fatalf("first client.Get returned error: %v", err)
	}
	if errClose := resp.Body.Close(); errClose != nil {
		t.Fatalf("first response body close returned error: %v", errClose)
	}
	if got, want := strings.Join(firstCalls, ","), "utls-h2,utls-http1"; got != want {
		t.Fatalf("first calls = %s, want %s", got, want)
	}

	var secondCalls []string
	secondClient := &http.Client{Transport: &fallbackRoundTripper{
		h2DegradeScope: scope,
		utls: utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			secondCalls = append(secondCalls, "utls-h2")
			return nil, errors.New("h2 should be skipped")
		}),
		protectedHTTP11: utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			secondCalls = append(secondCalls, "utls-http1")
			return utlsTestResponse(req), nil
		}),
	}}

	resp, err = secondClient.Get("https://chatgpt.com/backend-api/codex/responses")
	if err != nil {
		t.Fatalf("second client.Get returned error: %v", err)
	}
	if errClose := resp.Body.Close(); errClose != nil {
		t.Fatalf("second response body close returned error: %v", errClose)
	}
	if got, want := strings.Join(secondCalls, ","), "utls-http1"; got != want {
		t.Fatalf("second calls = %s, want %s", got, want)
	}
}

func TestFallbackRoundTripperReprobesH2AfterDegradeTTLAndBacksOff(t *testing.T) {
	base := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	now := base
	resetUtlsH2DegradeCacheForTest(t, func() time.Time { return now })

	var calls []string
	client := &http.Client{Transport: &fallbackRoundTripper{
		h2DegradeScope: "test-backoff",
		utls: utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls = append(calls, "utls-h2")
			return nil, errors.New("h2 transport failed")
		}),
		protectedHTTP11: utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls = append(calls, "utls-http1")
			return utlsTestResponse(req), nil
		}),
	}}

	doGet := func() {
		t.Helper()
		resp, err := client.Get("https://chatgpt.com/backend-api/codex/responses")
		if err != nil {
			t.Fatalf("client.Get returned error: %v", err)
		}
		if errClose := resp.Body.Close(); errClose != nil {
			t.Fatalf("response body close returned error: %v", errClose)
		}
	}

	doGet()
	if got, want := strings.Join(calls, ","), "utls-h2,utls-http1"; got != want {
		t.Fatalf("initial calls = %s, want %s", got, want)
	}

	calls = nil
	now = base.Add(2*time.Minute - time.Nanosecond)
	doGet()
	if got, want := strings.Join(calls, ","), "utls-http1"; got != want {
		t.Fatalf("calls before initial TTL expiry = %s, want %s", got, want)
	}

	calls = nil
	probeTime := base.Add(2*time.Minute + time.Nanosecond)
	now = probeTime
	doGet()
	if got, want := strings.Join(calls, ","), "utls-h2,utls-http1"; got != want {
		t.Fatalf("calls after initial TTL expiry = %s, want %s", got, want)
	}

	calls = nil
	now = probeTime.Add(4*time.Minute - time.Nanosecond)
	doGet()
	if got, want := strings.Join(calls, ","), "utls-http1"; got != want {
		t.Fatalf("calls before second TTL expiry = %s, want %s", got, want)
	}
}

func TestFallbackRoundTripperH2DegradeTTLCapsAtThirtyMinutes(t *testing.T) {
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	resetUtlsH2DegradeCacheForTest(t, func() time.Time { return now })

	h2Calls := 0
	client := &http.Client{Transport: &fallbackRoundTripper{
		h2DegradeScope: "test-cap",
		utls: utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			h2Calls++
			return nil, errors.New("h2 transport failed")
		}),
		protectedHTTP11: utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return utlsTestResponse(req), nil
		}),
	}}

	doGet := func() {
		t.Helper()
		resp, err := client.Get("https://chatgpt.com/backend-api/codex/responses")
		if err != nil {
			t.Fatalf("client.Get returned error: %v", err)
		}
		if errClose := resp.Body.Close(); errClose != nil {
			t.Fatalf("response body close returned error: %v", errClose)
		}
	}

	doGet()
	for _, wait := range []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute} {
		now = now.Add(wait + time.Nanosecond)
		doGet()
	}
	if h2Calls != 5 {
		t.Fatalf("h2 calls after probes = %d, want 5", h2Calls)
	}

	now = now.Add(31 * time.Minute)
	doGet()
	if h2Calls != 6 {
		t.Fatalf("h2 calls after capped TTL expiry = %d, want 6", h2Calls)
	}
}

func TestFallbackRoundTripperSkippedH2DoesNotRequireBodyReplay(t *testing.T) {
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	resetUtlsH2DegradeCacheForTest(t, func() time.Time { return now })

	scope := "test-body"
	warmup := &http.Client{Transport: &fallbackRoundTripper{
		h2DegradeScope: scope,
		utls: utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return nil, errors.New("h2 transport failed")
		}),
		protectedHTTP11: utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return utlsTestResponse(req), nil
		}),
	}}
	resp, err := warmup.Get("https://chatgpt.com/backend-api/codex/responses")
	if err != nil {
		t.Fatalf("warmup client.Get returned error: %v", err)
	}
	if errClose := resp.Body.Close(); errClose != nil {
		t.Fatalf("warmup response body close returned error: %v", errClose)
	}

	body := io.NopCloser(strings.NewReader(`{"input":"hello"}`))
	req, errReq := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", body)
	if errReq != nil {
		t.Fatalf("NewRequest returned error: %v", errReq)
	}
	if req.GetBody != nil {
		t.Fatal("test request unexpectedly has GetBody")
	}

	client := &http.Client{Transport: &fallbackRoundTripper{
		h2DegradeScope: scope,
		utls: utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return nil, errors.New("h2 should be skipped")
		}),
		protectedHTTP11: utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			got, errRead := io.ReadAll(req.Body)
			if errRead != nil {
				t.Fatalf("ReadAll returned error: %v", errRead)
			}
			if string(got) != `{"input":"hello"}` {
				t.Fatalf("body = %q, want original payload", got)
			}
			return utlsTestResponse(req), nil
		}),
	}}

	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("client.Do returned error: %v", err)
	}
	if errClose := resp.Body.Close(); errClose != nil {
		t.Fatalf("response body close returned error: %v", errClose)
	}
}

func TestUtlsRoundTripperPoolsMultipleH2ConnsPerHost(t *testing.T) {
	t.Parallel()

	// Hold the first stream so a second concurrent request must dial another conn.
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	listener, closeServer := startUtlsH2TestServer(t, 1, func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	defer closeServer()

	var dials atomic.Int32
	rt := &utlsRoundTripper{
		pools:           make(map[string]*utlsH2HostPool),
		maxConnsPerHost: 4,
		newClientConn: func(host, addr string) (*http2.ClientConn, error) {
			dials.Add(1)
			return dialUtlsH2TestClientConn(listener.Addr().String())
		},
	}
	defer closeUtlsH2Pool(rt)

	errCh := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodGet, "https://api.anthropic.com/v1/messages", nil)
			if err != nil {
				errCh <- err
				return
			}
			resp, err := rt.RoundTrip(req)
			if err != nil {
				errCh <- err
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()
	}

	deadline := time.After(3 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-deadline:
			t.Fatal("timed out waiting for both streams to start on separate conns")
		}
	}
	if got := dials.Load(); got != 2 {
		t.Fatalf("h2 dials = %d, want 2", got)
	}

	close(release)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("RoundTrip error: %v", err)
		}
	}
}

func TestUtlsRoundTripperReusesReadyH2Conn(t *testing.T) {
	t.Parallel()

	listener, closeServer := startUtlsH2TestServer(t, 100, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	defer closeServer()

	var dials atomic.Int32
	rt := &utlsRoundTripper{
		pools:           make(map[string]*utlsH2HostPool),
		maxConnsPerHost: 4,
		newClientConn: func(host, addr string) (*http2.ClientConn, error) {
			dials.Add(1)
			return dialUtlsH2TestClientConn(listener.Addr().String())
		},
	}
	defer closeUtlsH2Pool(rt)

	for i := 0; i < 5; i++ {
		req, err := http.NewRequest(http.MethodGet, "https://chatgpt.com/backend-api/codex/responses", nil)
		if err != nil {
			t.Fatalf("NewRequest error: %v", err)
		}
		resp, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip error: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	if got := dials.Load(); got != 1 {
		t.Fatalf("sequential dials = %d, want 1 (reuse ready conn)", got)
	}
}

func TestUtlsRoundTripperCapsConcurrentH2Dials(t *testing.T) {
	t.Parallel()

	// Block dials themselves to observe the concurrent dial cap independently of
	// peer stream settings.
	dialStarted := make(chan struct{}, 8)
	allowDial := make(chan struct{})
	listener, closeServer := startUtlsH2TestServer(t, 100, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	defer closeServer()

	var dials atomic.Int32
	rt := &utlsRoundTripper{
		pools:           make(map[string]*utlsH2HostPool),
		maxConnsPerHost: 2,
		newClientConn: func(host, addr string) (*http2.ClientConn, error) {
			dials.Add(1)
			dialStarted <- struct{}{}
			<-allowDial
			return dialUtlsH2TestClientConn(listener.Addr().String())
		},
	}
	defer closeUtlsH2Pool(rt)

	const inflight = 6
	errCh := make(chan error, inflight)
	var wg sync.WaitGroup
	for i := 0; i < inflight; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodGet, "https://api.anthropic.com/v1/messages", nil)
			if err != nil {
				errCh <- err
				return
			}
			resp, err := rt.RoundTrip(req)
			if err != nil {
				errCh <- err
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()
	}

	deadline := time.After(3 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-dialStarted:
		case <-deadline:
			t.Fatal("timed out waiting for capped concurrent dials to start")
		}
	}
	// No third dial should start while the first two are still in-flight.
	time.Sleep(50 * time.Millisecond)
	if got := dials.Load(); got != 2 {
		t.Fatalf("in-flight h2 dials = %d, want 2", got)
	}

	close(allowDial)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("RoundTrip error: %v", err)
		}
	}
	// After the first two conns become ready, remaining requests reuse them.
	if got := dials.Load(); got != 2 {
		t.Fatalf("total h2 dials = %d, want 2", got)
	}
}

func TestUtlsRoundTripperWaitsWhenH2PoolIsSaturated(t *testing.T) {
	t.Parallel()

	started := make(chan string, 3)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })

	listener, closeServer := startUtlsH2TestServer(t, 1, func(w http.ResponseWriter, r *http.Request) {
		started <- r.URL.Path
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	defer closeServer()

	rt := &utlsRoundTripper{
		pools:           make(map[string]*utlsH2HostPool),
		maxConnsPerHost: 2,
		newClientConn: func(host, addr string) (*http2.ClientConn, error) {
			return dialUtlsH2TestClientConn(listener.Addr().String())
		},
	}
	defer closeUtlsH2Pool(rt)

	results := make(chan error, 3)
	startRequest := func(path string) {
		go func() {
			req, err := http.NewRequest(http.MethodGet, "https://api.anthropic.com"+path, nil)
			if err != nil {
				results <- err
				return
			}
			resp, err := rt.RoundTrip(req)
			if err != nil {
				results <- err
				return
			}
			_, errCopy := io.Copy(io.Discard, resp.Body)
			errClose := resp.Body.Close()
			if errCopy != nil {
				results <- errCopy
				return
			}
			results <- errClose
		}()
	}
	waitStarted := func(want string) {
		t.Helper()
		select {
		case got := <-started:
			if got != want {
				t.Fatalf("started request = %q, want %q", got, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for %s to start", want)
		}
	}

	startRequest("/first")
	waitStarted("/first")
	startRequest("/second")
	waitStarted("/second")
	startRequest("/third")

	select {
	case err := <-results:
		t.Fatalf("request completed while the h2 pool was saturated: %v", err)
	case got := <-started:
		t.Fatalf("request %q reached the server before a stream slot was available", got)
	case <-time.After(100 * time.Millisecond):
	}

	releaseOnce.Do(func() { close(release) })
	waitStarted("/third")
	for i := 0; i < 3; i++ {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("RoundTrip error: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for saturated pool requests to complete")
		}
	}
}

func TestUtlsRoundTripperRequestCancellationKeepsSharedH2ConnAlive(t *testing.T) {
	t.Parallel()

	peerStarted := make(chan struct{}, 1)
	cancelStarted := make(chan struct{}, 1)
	releasePeer := make(chan struct{})
	var releasePeerOnce sync.Once
	defer releasePeerOnce.Do(func() { close(releasePeer) })

	listener, closeServer := startUtlsH2TestServer(t, 100, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/peer":
			peerStarted <- struct{}{}
			<-releasePeer
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		case "/cancel":
			cancelStarted <- struct{}{}
			<-r.Context().Done()
		}
	})
	defer closeServer()

	rt := &utlsRoundTripper{
		pools:           make(map[string]*utlsH2HostPool),
		maxConnsPerHost: 1,
		newClientConn: func(host, addr string) (*http2.ClientConn, error) {
			return dialUtlsH2TestClientConn(listener.Addr().String())
		},
	}
	defer closeUtlsH2Pool(rt)

	peerResult := make(chan error, 1)
	go func() {
		req, err := http.NewRequest(http.MethodGet, "https://api.anthropic.com/peer", nil)
		if err != nil {
			peerResult <- err
			return
		}
		resp, err := rt.RoundTrip(req)
		if err != nil {
			peerResult <- err
			return
		}
		_, errCopy := io.Copy(io.Discard, resp.Body)
		errClose := resp.Body.Close()
		if errCopy != nil {
			peerResult <- errCopy
			return
		}
		peerResult <- errClose
	}()
	select {
	case <-peerStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for peer request to start")
	}

	cancelCtx, cancel := context.WithCancel(context.Background())
	cancelResult := make(chan error, 1)
	go func() {
		req, err := http.NewRequestWithContext(cancelCtx, http.MethodGet, "https://api.anthropic.com/cancel", nil)
		if err != nil {
			cancelResult <- err
			return
		}
		_, err = rt.RoundTrip(req)
		cancelResult <- err
	}()
	select {
	case <-cancelStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for cancellable request to start")
	}

	cancel()
	select {
	case err := <-cancelResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled RoundTrip error = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for cancelled request")
	}

	releasePeerOnce.Do(func() { close(releasePeer) })
	select {
	case err := <-peerResult:
		if err != nil {
			t.Fatalf("peer request failed after cancelling another stream: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for peer request to complete")
	}
}

func closeUtlsH2Pool(rt *utlsRoundTripper) {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for host, pool := range rt.pools {
		if pool == nil {
			continue
		}
		for _, conn := range pool.conns {
			if conn != nil {
				_ = conn.Close()
			}
		}
		pool.conns = nil
		delete(rt.pools, host)
	}
}

func startUtlsH2TestServer(t *testing.T, maxConcurrentStreams uint32, handler http.HandlerFunc) (net.Listener, func()) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen error: %v", err)
	}

	server := &http2.Server{MaxConcurrentStreams: maxConcurrentStreams}
	var (
		mu      sync.Mutex
		conns   []net.Conn
		accepts sync.WaitGroup
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, errAccept := listener.Accept()
			if errAccept != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
			accepts.Add(1)
			go func(c net.Conn) {
				defer accepts.Done()
				server.ServeConn(c, &http2.ServeConnOpts{
					Handler: http.HandlerFunc(handler),
				})
			}(conn)
		}
	}()

	closeServer := func() {
		_ = listener.Close()
		<-done
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		finished := make(chan struct{})
		go func() {
			accepts.Wait()
			close(finished)
		}()
		select {
		case <-finished:
		case <-time.After(500 * time.Millisecond):
		}
	}
	return listener, closeServer
}

func dialUtlsH2TestClientConn(addr string) (*http2.ClientConn, error) {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return nil, err
	}
	tr := &http2.Transport{
		AllowHTTP:                  true,
		StrictMaxConcurrentStreams: true,
	}
	h2Conn, err := tr.NewClientConn(conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return h2Conn, nil
}

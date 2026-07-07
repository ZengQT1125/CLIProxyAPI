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
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
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

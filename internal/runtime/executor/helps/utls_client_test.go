package helps

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	cryptotls "crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tls "github.com/refraction-networking/utls"
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

type claudeCodeTLSFingerprintFixture struct {
	ClientHelloLength   int
	JA3                 string
	JA3MD5              string
	ALPN                []string
	HTTPVersion         string
	CipherSuites        []uint16
	ExtensionTypes      []uint16
	ExtensionLengths    [][2]int
	SupportedGroups     []uint16
	PointFormats        []uint8
	SignatureAlgorithms []uint16
	SupportedVersions   []uint16
	KeyShareGroups      []uint16
}

func TestClaudeCodeTLSClientHelloSpecMatches220Capture(t *testing.T) {
	t.Parallel()

	fixture := claudeCodeTLSFingerprintFixture{
		ClientHelloLength: 508,
		JA3:               "771,4865-4866-4867-49195-49199-49196-49200-52393-52392-49161-49171-49162-49172-156-157-47-53,0-23-65281-10-11-35-16-5-13-18-51-45-43-21,29-23-24,0",
		JA3MD5:            "d871d02cecbde59abbf8f4806134addf",
		ALPN:              []string{"http/1.1"},
		HTTPVersion:       "HTTP/1.1",
		CipherSuites:      []uint16{4865, 4866, 4867, 49195, 49199, 49196, 49200, 52393, 52392, 49161, 49171, 49162, 49172, 156, 157, 47, 53},
		ExtensionTypes:    []uint16{0, 23, 65281, 10, 11, 35, 16, 5, 13, 18, 51, 45, 43, 21},
		ExtensionLengths: [][2]int{
			{0, 22}, {23, 0}, {65281, 1}, {10, 8}, {11, 2}, {35, 0}, {16, 11},
			{5, 5}, {13, 20}, {18, 0}, {51, 38}, {45, 2}, {43, 5}, {21, 231},
		},
		SupportedGroups:     []uint16{29, 23, 24},
		PointFormats:        []uint8{0},
		SignatureAlgorithms: []uint16{1027, 2052, 1025, 1283, 2053, 1281, 2054, 1537, 513},
		SupportedVersions:   []uint16{772, 771},
		KeyShareGroups:      []uint16{29},
	}

	record := captureClaudeCodeClientHello(t)
	if got := len(record) - 9; got != fixture.ClientHelloLength {
		t.Fatalf("ClientHello length = %d, want %d", got, fixture.ClientHelloLength)
	}
	if got := parseClientHelloExtensionLengths(t, record); !reflect.DeepEqual(got, fixture.ExtensionLengths) {
		t.Fatalf("extension lengths = %v, want %v", got, fixture.ExtensionLengths)
	}

	spec, errFingerprint := (&tls.Fingerprinter{}).FingerprintClientHello(record)
	if errFingerprint != nil {
		t.Fatal(errFingerprint)
	}
	actual := summarizeClaudeCodeClientHelloSpec(t, spec)
	if !reflect.DeepEqual(actual.CipherSuites, fixture.CipherSuites) {
		t.Fatalf("cipher suites = %v, want %v", actual.CipherSuites, fixture.CipherSuites)
	}
	if !reflect.DeepEqual(actual.ExtensionTypes, fixture.ExtensionTypes) {
		t.Fatalf("extension types = %v, want %v", actual.ExtensionTypes, fixture.ExtensionTypes)
	}
	if !reflect.DeepEqual(actual.ALPN, fixture.ALPN) {
		t.Fatalf("ALPN = %v, want %v", actual.ALPN, fixture.ALPN)
	}
	if !reflect.DeepEqual(actual.SupportedGroups, fixture.SupportedGroups) {
		t.Fatalf("supported groups = %v, want %v", actual.SupportedGroups, fixture.SupportedGroups)
	}
	if !reflect.DeepEqual(actual.PointFormats, fixture.PointFormats) {
		t.Fatalf("point formats = %v, want %v", actual.PointFormats, fixture.PointFormats)
	}
	if !reflect.DeepEqual(actual.SignatureAlgorithms, fixture.SignatureAlgorithms) {
		t.Fatalf("signature algorithms = %v, want %v", actual.SignatureAlgorithms, fixture.SignatureAlgorithms)
	}
	if !reflect.DeepEqual(actual.SupportedVersions, fixture.SupportedVersions) {
		t.Fatalf("supported versions = %v, want %v", actual.SupportedVersions, fixture.SupportedVersions)
	}
	if !reflect.DeepEqual(actual.KeyShareGroups, fixture.KeyShareGroups) {
		t.Fatalf("key share groups = %v, want %v", actual.KeyShareGroups, fixture.KeyShareGroups)
	}
	if actual.JA3 != fixture.JA3 || actual.JA3MD5 != fixture.JA3MD5 {
		t.Fatalf("JA3 = %q (%s), want %q (%s)", actual.JA3, actual.JA3MD5, fixture.JA3, fixture.JA3MD5)
	}

	transport, ok := newClaudeCodeRoundTripper("").(*http.Transport)
	if !ok {
		t.Fatalf("Claude Code transport type = %T, want *http.Transport", newClaudeCodeRoundTripper(""))
	}
	if transport.ForceAttemptHTTP2 {
		t.Fatal("Claude Code transport must not force HTTP/2")
	}
	if fixture.HTTPVersion != "HTTP/1.1" {
		t.Fatalf("fixture HTTP version = %q, want HTTP/1.1", fixture.HTTPVersion)
	}
}

func TestClaudeCodeTLSResumptionIsWireSafe(t *testing.T) {
	t.Parallel()

	// RFC 8446 4.2.11 requires pre_shared_key to be the final extension, after
	// the padding extension.
	spec := claudeCodeTLSClientHelloSpec()
	last := spec.Extensions[len(spec.Extensions)-1]
	if _, ok := last.(*tls.UtlsPreSharedKeyExtension); !ok {
		t.Fatalf("last inference extension = %T, want *tls.UtlsPreSharedKeyExtension", last)
	}
	if _, ok := spec.Extensions[len(spec.Extensions)-2].(*tls.UtlsPaddingExtension); !ok {
		t.Fatalf("extension before pre_shared_key = %T, want *tls.UtlsPaddingExtension", spec.Extensions[len(spec.Extensions)-2])
	}

	// Without OmitEmptyPsk uTLS refuses to marshal an empty PSK, and without
	// PreferSkipResumptionOnNilExtension a HelloCustom resumption attempt panics.
	cfg := newClaudeCodeTLSConfig("api.anthropic.com", tls.NewLRUClientSessionCache(claudeCodeSessionCacheCapacity))
	if cfg.ClientSessionCache == nil {
		t.Fatal("ClientSessionCache = nil, want a session cache so resumption is possible")
	}
	if !cfg.OmitEmptyPsk {
		t.Fatal("OmitEmptyPsk = false, want true so an unresumed ClientHello stays byte-identical")
	}
	if !cfg.PreferSkipResumptionOnNilExtension {
		t.Fatal("PreferSkipResumptionOnNilExtension = false, want true to avoid a HelloCustom resumption panic")
	}
}

func TestClaudeCodeRequestHeaderOrderMatchesNative220Capture(t *testing.T) {
	t.Parallel()

	if got, want := claudeCodeRequestHeaderOrder(http.MethodPost, "/v1/messages?beta=true"), claudeCodeMessagesHeaderOrder; !reflect.DeepEqual(got, want) {
		t.Fatalf("Messages header order = %v, want %v", got, want)
	}
	if got, want := claudeCodeRequestHeaderOrder(http.MethodPost, "/v1/messages/count_tokens?beta=true"), claudeCodeCountTokensHeaderOrder; !reflect.DeepEqual(got, want) {
		t.Fatalf("count_tokens header order = %v, want %v", got, want)
	}
	for _, name := range claudeCodeCountTokensHeaderOrder {
		if name == "X-Stainless-Timeout" {
			t.Fatal("count_tokens header order unexpectedly contains X-Stainless-Timeout")
		}
	}
}

func TestCachedClaudeCodeRoundTripperReusesTransport(t *testing.T) {
	t.Parallel()

	const proxyURL = "http://127.0.0.1:29653"
	first := cachedClaudeCodeRoundTripper(proxyURL)
	second := cachedClaudeCodeRoundTripper(proxyURL)
	if first != second {
		t.Fatal("Claude Code transport cache returned different transports for one proxy")
	}
}

func TestCachedClaudeCodeRoundTripperBoundsProxyCardinality(t *testing.T) {
	firstProxy := fmt.Sprintf("http://127.0.0.1:%d", 30000)
	first := cachedClaudeCodeRoundTripper(firstProxy)
	for index := 1; index <= claudeCodeRoundTripperCacheCapacity; index++ {
		cachedClaudeCodeRoundTripper(fmt.Sprintf("http://127.0.0.1:%d", 30000+index))
	}
	if got := claudeCodeRoundTripperCache.Len(); got > claudeCodeRoundTripperCacheCapacity {
		t.Fatalf("transport cache entries = %d, want at most %d", got, claudeCodeRoundTripperCacheCapacity)
	}
	if recreated := cachedClaudeCodeRoundTripper(firstProxy); recreated == first {
		t.Fatal("least recently used proxy transport was not evicted")
	}
}

func TestClaudeCodeTLSClientHelloCapture(t *testing.T) {
	proxyURL := os.Getenv("CPA_TLS_FP_PROXY")
	if proxyURL == "" {
		t.Skip("CPA_TLS_FP_PROXY is not set")
	}

	client := NewUtlsHTTPClient(t.Context(), nil, &cliproxyauth.Auth{ProxyURL: proxyURL}, 0)
	req, errRequest := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewBufferString(`{"model":"claude-opus-4-6","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`))
	if errRequest != nil {
		t.Fatal(errRequest)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "dummy-tls-fingerprint")
	resp, errDo := client.Do(req)
	if errDo != nil {
		t.Fatal(errDo)
	}
	if errClose := resp.Body.Close(); errClose != nil {
		t.Fatal(errClose)
	}
}

func TestFallbackRoundTripperSelectsProviderFingerprint(t *testing.T) {
	t.Parallel()

	route := func(label string) http.RoundTripper {
		return utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"X-Test-Route": []string{label}},
				Body:       io.NopCloser(strings.NewReader("{}")),
				Request:    req,
			}, nil
		})
	}
	roundTripper := &fallbackRoundTripper{
		anthropic: route("anthropic"),
		utls:      route("chrome"),
		fallback:  route("fallback"),
	}
	tests := []struct {
		name string
		url  string
		want string
	}{
		{name: "Anthropic HTTPS", url: "https://api.anthropic.com/v1/messages", want: "anthropic"},
		{name: "Anthropic explicit HTTPS port", url: "https://api.anthropic.com:443/v1/messages", want: "anthropic"},
		{name: "Anthropic custom port", url: "https://api.anthropic.com:8443/v1/messages", want: "fallback"},
		{name: "Anthropic userinfo", url: "https://caller@api.anthropic.com/v1/messages", want: "fallback"},
		{name: "Anthropic lookalike", url: "https://api.anthropic.com.example/v1/messages", want: "fallback"},
		{name: "ChatGPT HTTPS", url: "https://chatgpt.com/backend-api/codex/responses", want: "chrome"},
		{name: "Other HTTPS", url: "https://example.com/v1/messages", want: "fallback"},
		{name: "Anthropic HTTP", url: "http://api.anthropic.com/v1/messages", want: "fallback"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, errRequest := http.NewRequest(http.MethodGet, tt.url, nil)
			if errRequest != nil {
				t.Fatal(errRequest)
			}
			resp, errRoundTrip := roundTripper.RoundTrip(req)
			if errRoundTrip != nil {
				t.Fatal(errRoundTrip)
			}
			defer func() {
				if errClose := resp.Body.Close(); errClose != nil {
					t.Errorf("close response body: %v", errClose)
				}
			}()
			if got := resp.Header.Get("X-Test-Route"); got != tt.want {
				t.Fatalf("route = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewUtlsHTTPClientUsesContextRoundTripperForProviderFingerprints(t *testing.T) {
	t.Parallel()

	for _, targetURL := range []string{
		"https://api.anthropic.com/v1/messages",
		"https://chatgpt.com/backend-api/codex/responses",
	} {
		t.Run(targetURL, func(t *testing.T) {
			called := false
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				called = true
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("{}")),
					Request:    req,
				}, nil
			}))

			client := NewUtlsHTTPClient(ctx, nil, nil, 0)
			resp, err := client.Get(targetURL)
			if err != nil {
				t.Fatalf("client.Get returned error: %v", err)
			}
			if errClose := resp.Body.Close(); errClose != nil {
				t.Fatalf("response body close returned error: %v", errClose)
			}
			if !called {
				t.Fatal("expected context RoundTripper to handle protected host request")
			}
		})
	}
}

type claudeCodeClientHelloSummary struct {
	CipherSuites        []uint16
	ExtensionTypes      []uint16
	ALPN                []string
	SupportedGroups     []uint16
	PointFormats        []uint8
	SignatureAlgorithms []uint16
	SupportedVersions   []uint16
	KeyShareGroups      []uint16
	JA3                 string
	JA3MD5              string
}

func captureClaudeCodeClientHello(t *testing.T) []byte {
	t.Helper()

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		if errClose := clientConn.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
			t.Errorf("close client pipe: %v", errClose)
		}
		if errClose := serverConn.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
			t.Errorf("close server pipe: %v", errClose)
		}
	})
	// Use the production config so the captured bytes reflect the real dial path,
	// including the resumption settings.
	cfg := newClaudeCodeTLSConfig("api.anthropic.com", tls.NewLRUClientSessionCache(claudeCodeSessionCacheCapacity))
	tlsConn := tls.UClient(clientConn, cfg, tls.HelloCustom)
	if errPreset := tlsConn.ApplyPreset(claudeCodeTLSClientHelloSpec()); errPreset != nil {
		t.Fatal(errPreset)
	}
	handshakeDone := make(chan error, 1)
	go func() {
		handshakeDone <- tlsConn.Handshake()
	}()
	if errDeadline := serverConn.SetReadDeadline(time.Now().Add(5 * time.Second)); errDeadline != nil {
		t.Fatal(errDeadline)
	}
	header := make([]byte, 5)
	if _, errRead := io.ReadFull(serverConn, header); errRead != nil {
		t.Fatal(errRead)
	}
	payload := make([]byte, int(binary.BigEndian.Uint16(header[3:5])))
	if _, errRead := io.ReadFull(serverConn, payload); errRead != nil {
		t.Fatal(errRead)
	}
	if errClose := serverConn.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	select {
	case <-handshakeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("uTLS handshake did not exit after the capture connection closed")
	}
	return append(header, payload...)
}

func parseClientHelloExtensionLengths(t *testing.T, record []byte) [][2]int {
	t.Helper()
	if len(record) < 9 || record[0] != 22 || record[5] != 1 {
		t.Fatalf("invalid TLS ClientHello record")
	}
	body := record[9:]
	offset := 2 + 32
	if offset >= len(body) {
		t.Fatal("truncated ClientHello random")
	}
	sessionLength := int(body[offset])
	offset += 1 + sessionLength
	if offset+2 > len(body) {
		t.Fatal("truncated ClientHello cipher suites")
	}
	cipherLength := int(binary.BigEndian.Uint16(body[offset : offset+2]))
	offset += 2 + cipherLength
	if offset >= len(body) {
		t.Fatal("truncated ClientHello compression methods")
	}
	compressionLength := int(body[offset])
	offset += 1 + compressionLength
	if offset+2 > len(body) {
		t.Fatal("truncated ClientHello extensions")
	}
	extensionsLength := int(binary.BigEndian.Uint16(body[offset : offset+2]))
	offset += 2
	end := offset + extensionsLength
	if end > len(body) {
		t.Fatal("truncated ClientHello extension data")
	}
	lengths := make([][2]int, 0)
	for offset+4 <= end {
		extensionType := int(binary.BigEndian.Uint16(body[offset : offset+2]))
		extensionLength := int(binary.BigEndian.Uint16(body[offset+2 : offset+4]))
		lengths = append(lengths, [2]int{extensionType, extensionLength})
		offset += 4 + extensionLength
	}
	if offset != end {
		t.Fatal("misaligned ClientHello extension data")
	}
	return lengths
}

func summarizeClaudeCodeClientHelloSpec(t *testing.T, spec *tls.ClientHelloSpec) claudeCodeClientHelloSummary {
	t.Helper()
	summary := claudeCodeClientHelloSummary{CipherSuites: append([]uint16(nil), spec.CipherSuites...)}
	for _, extension := range spec.Extensions {
		switch ext := extension.(type) {
		case *tls.SNIExtension:
			summary.ExtensionTypes = append(summary.ExtensionTypes, 0)
		case *tls.ExtendedMasterSecretExtension:
			summary.ExtensionTypes = append(summary.ExtensionTypes, 23)
		case *tls.RenegotiationInfoExtension:
			summary.ExtensionTypes = append(summary.ExtensionTypes, 65281)
		case *tls.SupportedCurvesExtension:
			summary.ExtensionTypes = append(summary.ExtensionTypes, 10)
			for _, curve := range ext.Curves {
				summary.SupportedGroups = append(summary.SupportedGroups, uint16(curve))
			}
		case *tls.SupportedPointsExtension:
			summary.ExtensionTypes = append(summary.ExtensionTypes, 11)
			summary.PointFormats = append(summary.PointFormats, ext.SupportedPoints...)
		case *tls.SessionTicketExtension:
			summary.ExtensionTypes = append(summary.ExtensionTypes, 35)
		case *tls.ALPNExtension:
			summary.ExtensionTypes = append(summary.ExtensionTypes, 16)
			summary.ALPN = append(summary.ALPN, ext.AlpnProtocols...)
		case *tls.StatusRequestExtension:
			summary.ExtensionTypes = append(summary.ExtensionTypes, 5)
		case *tls.SignatureAlgorithmsExtension:
			summary.ExtensionTypes = append(summary.ExtensionTypes, 13)
			for _, algorithm := range ext.SupportedSignatureAlgorithms {
				summary.SignatureAlgorithms = append(summary.SignatureAlgorithms, uint16(algorithm))
			}
		case *tls.SCTExtension:
			summary.ExtensionTypes = append(summary.ExtensionTypes, 18)
		case *tls.KeyShareExtension:
			summary.ExtensionTypes = append(summary.ExtensionTypes, 51)
			for _, keyShare := range ext.KeyShares {
				summary.KeyShareGroups = append(summary.KeyShareGroups, uint16(keyShare.Group))
			}
		case *tls.PSKKeyExchangeModesExtension:
			summary.ExtensionTypes = append(summary.ExtensionTypes, 45)
		case *tls.SupportedVersionsExtension:
			summary.ExtensionTypes = append(summary.ExtensionTypes, 43)
			summary.SupportedVersions = append(summary.SupportedVersions, ext.Versions...)
		case *tls.UtlsPaddingExtension:
			summary.ExtensionTypes = append(summary.ExtensionTypes, 21)
		default:
			t.Fatalf("unexpected ClientHello extension type %T", extension)
		}
	}
	cipherStrings := make([]string, 0, len(summary.CipherSuites))
	for _, cipher := range summary.CipherSuites {
		cipherStrings = append(cipherStrings, strconv.Itoa(int(cipher)))
	}
	extensionStrings := make([]string, 0, len(summary.ExtensionTypes))
	for _, extensionType := range summary.ExtensionTypes {
		extensionStrings = append(extensionStrings, strconv.Itoa(int(extensionType)))
	}
	groupStrings := make([]string, 0, len(summary.SupportedGroups))
	for _, group := range summary.SupportedGroups {
		groupStrings = append(groupStrings, strconv.Itoa(int(group)))
	}
	pointStrings := make([]string, 0, len(summary.PointFormats))
	for _, point := range summary.PointFormats {
		pointStrings = append(pointStrings, strconv.Itoa(int(point)))
	}
	summary.JA3 = fmt.Sprintf("771,%s,%s,%s,%s", strings.Join(cipherStrings, "-"), strings.Join(extensionStrings, "-"), strings.Join(groupStrings, "-"), strings.Join(pointStrings, "-"))
	digest := md5.Sum([]byte(summary.JA3)) // #nosec G401 -- JA3 requires MD5.
	summary.JA3MD5 = hex.EncodeToString(digest[:])
	return summary
}

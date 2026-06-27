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
)

type utlsClientRoundTripFunc func(*http.Request) (*http.Response, error)

func (f utlsClientRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
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

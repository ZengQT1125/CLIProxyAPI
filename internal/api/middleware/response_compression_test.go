package middleware

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

func TestV0ResponseCompressionNegotiatesEncoding(t *testing.T) {
	body := []byte(`{"items":["` + strings.Repeat("compressible-data-", 256) + `"]}`)
	tests := []struct {
		name           string
		acceptEncoding string
		wantEncoding   string
	}{
		{name: "zstd preferred on equal quality", acceptEncoding: "gzip, zstd", wantEncoding: "zstd"},
		{name: "gzip preferred by quality", acceptEncoding: "gzip;q=1, zstd;q=0.5", wantEncoding: "gzip"},
		{name: "zstd accepted through wildcard", acceptEncoding: "*;q=0.8, gzip;q=0", wantEncoding: "zstd"},
		{name: "identity when unsupported", acceptEncoding: "br", wantEncoding: ""},
		{name: "identity when disabled", acceptEncoding: "gzip;q=0, zstd;q=0", wantEncoding: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := executeCompressionRequest(t, http.MethodGet, "/v0/test", tt.acceptEncoding, func(c *gin.Context) {
				c.Header("Content-Length", strconv.Itoa(len(body)))
				c.Data(http.StatusOK, "application/json", body)
			})

			if got := recorder.Header().Get("Content-Encoding"); got != tt.wantEncoding {
				t.Fatalf("Content-Encoding = %q, want %q", got, tt.wantEncoding)
			}
			if !headerTokenContains(recorder.Header().Values("Vary"), "Accept-Encoding") {
				t.Fatalf("Vary = %q, want Accept-Encoding", recorder.Header().Values("Vary"))
			}
			if tt.wantEncoding != "" && recorder.Header().Get("Content-Length") != "" {
				t.Fatalf("compressed Content-Length = %q, want empty", recorder.Header().Get("Content-Length"))
			}
			decoded := decodeResponseBody(t, tt.wantEncoding, recorder.Body.Bytes())
			if !bytes.Equal(decoded, body) {
				t.Fatalf("decoded body differs: got %d bytes, want %d", len(decoded), len(body))
			}
		})
	}
}

func TestV0ResponseCompressionBypassesIneligibleResponses(t *testing.T) {
	largeBody := []byte(strings.Repeat("stream-data-", 256))
	tests := []struct {
		name    string
		method  string
		path    string
		headers map[string]string
		status  int
		type_   string
		body    []byte
		setup   func(*gin.Context)
	}{
		{name: "outside v0", method: http.MethodGet, path: "/v1/test", status: http.StatusOK, type_: "application/json", body: largeBody},
		{name: "small response", method: http.MethodGet, path: "/v0/test", status: http.StatusOK, type_: "application/json", body: []byte(`{"ok":true}`)},
		{name: "server sent events", method: http.MethodGet, path: "/v0/test", status: http.StatusOK, type_: "text/event-stream", body: largeBody},
		{name: "binary response", method: http.MethodGet, path: "/v0/test", status: http.StatusOK, type_: "application/octet-stream", body: largeBody},
		{name: "range request", method: http.MethodGet, path: "/v0/test", headers: map[string]string{"Range": "bytes=0-99"}, status: http.StatusPartialContent, type_: "application/json", body: largeBody},
		{name: "head request", method: http.MethodHead, path: "/v0/test", status: http.StatusOK, type_: "application/json", body: largeBody},
		{name: "no content", method: http.MethodGet, path: "/v0/test", status: http.StatusNoContent, type_: "application/json"},
		{
			name:   "cache control no transform",
			method: http.MethodGet,
			path:   "/v0/test",
			status: http.StatusOK,
			type_:  "application/json",
			body:   largeBody,
			setup: func(c *gin.Context) {
				c.Header("Cache-Control", "public, no-transform")
			},
		},
		{
			name:   "already encoded",
			method: http.MethodGet,
			path:   "/v0/test",
			status: http.StatusOK,
			type_:  "application/json",
			body:   largeBody,
			setup: func(c *gin.Context) {
				c.Header("Content-Encoding", "br")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := executeCompressionRequest(t, tt.method, tt.path, "zstd, gzip", func(c *gin.Context) {
				if tt.setup != nil {
					tt.setup(c)
				}
				c.Data(tt.status, tt.type_, tt.body)
			}, tt.headers)

			wantEncoding := ""
			if tt.name == "already encoded" {
				wantEncoding = "br"
			}
			if got := recorder.Header().Get("Content-Encoding"); got != wantEncoding {
				t.Fatalf("Content-Encoding = %q, want %q", got, wantEncoding)
			}
		})
	}
}

func TestV0ResponseCompressionPreservesVaryHeader(t *testing.T) {
	body := []byte(strings.Repeat("compressible-data-", 256))
	recorder := executeCompressionRequest(t, http.MethodGet, "/v0/test", "gzip", func(c *gin.Context) {
		c.Header("Vary", "Origin")
		c.Data(http.StatusOK, "application/json", body)
	})

	for _, want := range []string{"Origin", "Accept-Encoding"} {
		if !headerTokenContains(recorder.Header().Values("Vary"), want) {
			t.Fatalf("Vary = %q, want token %q", recorder.Header().Values("Vary"), want)
		}
	}
}

func executeCompressionRequest(t *testing.T, method, path, acceptEncoding string, handler gin.HandlerFunc, requestHeaders ...map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(V0ResponseCompressionMiddleware())
	engine.Handle(method, path, handler)

	request := httptest.NewRequest(method, path, nil)
	if acceptEncoding != "" {
		request.Header.Set("Accept-Encoding", acceptEncoding)
	}
	for _, headers := range requestHeaders {
		for key, value := range headers {
			request.Header.Set(key, value)
		}
	}
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	return recorder
}

func decodeResponseBody(t *testing.T, encoding string, body []byte) []byte {
	t.Helper()
	switch encoding {
	case "":
		return body
	case "gzip":
		reader, errReader := gzip.NewReader(bytes.NewReader(body))
		if errReader != nil {
			t.Fatalf("gzip.NewReader: %v", errReader)
		}
		decoded, errRead := io.ReadAll(reader)
		if errRead != nil {
			t.Fatalf("read gzip response: %v", errRead)
		}
		if errClose := reader.Close(); errClose != nil {
			t.Fatalf("close gzip reader: %v", errClose)
		}
		return decoded
	case "zstd":
		reader, errReader := zstd.NewReader(bytes.NewReader(body))
		if errReader != nil {
			t.Fatalf("zstd.NewReader: %v", errReader)
		}
		decoded, errRead := io.ReadAll(reader)
		if errRead != nil {
			t.Fatalf("read zstd response: %v", errRead)
		}
		reader.Close()
		return decoded
	default:
		t.Fatalf("unsupported test response encoding %q", encoding)
		return nil
	}
}

func headerTokenContains(values []string, want string) bool {
	for _, value := range values {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), want) {
				return true
			}
		}
	}
	return false
}

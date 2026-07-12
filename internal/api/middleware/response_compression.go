package middleware

import (
	"compress/gzip"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	log "github.com/sirupsen/logrus"
)

const responseCompressionMinBytes = 1024

type responseCompressionEncoder interface {
	io.WriteCloser
	Flush() error
}

type responseCompressionWriter struct {
	gin.ResponseWriter
	encoding string
	status   int
	size     int
	buffer   []byte
	encoder  responseCompressionEncoder
	decided  bool
}

// V0ResponseCompressionMiddleware negotiates gzip or zstd compression for /v0 responses.
func V0ResponseCompressionMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c == nil || c.Request == nil || !isV0Path(c.Request.URL.Path) || shouldBypassCompressionRequest(c.Request) {
			c.Next()
			return
		}

		addVaryHeader(c.Writer.Header(), "Accept-Encoding")
		encoding := negotiateResponseEncoding(c.GetHeader("Accept-Encoding"))
		if encoding == "" {
			c.Next()
			return
		}

		writer := &responseCompressionWriter{
			ResponseWriter: c.Writer,
			encoding:       encoding,
			status:         -1,
			buffer:         make([]byte, 0, responseCompressionMinBytes),
		}
		c.Writer = writer
		c.Next()
		if errFinish := writer.finish(); errFinish != nil {
			log.WithError(errFinish).Warnf("failed to finish %s response compression", encoding)
		}
	}
}

func isV0Path(path string) bool {
	return path == "/v0" || strings.HasPrefix(path, "/v0/")
}

func shouldBypassCompressionRequest(req *http.Request) bool {
	if req == nil || req.Method == http.MethodHead || strings.TrimSpace(req.Header.Get("Range")) != "" {
		return true
	}
	if strings.TrimSpace(req.Header.Get("Upgrade")) != "" {
		return true
	}
	for _, token := range strings.Split(req.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
			return true
		}
	}
	return false
}

func negotiateResponseEncoding(header string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return ""
	}

	qualities := make(map[string]float64)
	for _, item := range strings.Split(header, ",") {
		parts := strings.Split(item, ";")
		name := strings.ToLower(strings.TrimSpace(parts[0]))
		if name == "" {
			continue
		}
		quality := 1.0
		for _, parameter := range parts[1:] {
			keyValue := strings.SplitN(strings.TrimSpace(parameter), "=", 2)
			if len(keyValue) != 2 || !strings.EqualFold(strings.TrimSpace(keyValue[0]), "q") {
				continue
			}
			parsed, errParse := strconv.ParseFloat(strings.TrimSpace(keyValue[1]), 64)
			if errParse != nil || parsed < 0 || parsed > 1 {
				quality = 0
			} else {
				quality = parsed
			}
		}
		qualities[name] = quality
	}

	wildcardQuality := qualities["*"]
	zstdQuality, zstdExplicit := qualities["zstd"]
	if !zstdExplicit {
		zstdQuality = wildcardQuality
	}
	gzipQuality, gzipExplicit := qualities["gzip"]
	if !gzipExplicit {
		gzipQuality = wildcardQuality
	}

	switch {
	case zstdQuality <= 0 && gzipQuality <= 0:
		return ""
	case zstdQuality >= gzipQuality:
		return "zstd"
	default:
		return "gzip"
	}
}

func (w *responseCompressionWriter) WriteHeader(statusCode int) {
	if w.status >= 0 {
		return
	}
	w.status = statusCode
}

func (w *responseCompressionWriter) WriteHeaderNow() {
	if !w.decided {
		if errIdentity := w.startIdentity(); errIdentity != nil {
			return
		}
		if errBuffer := w.writeBuffered(); errBuffer != nil {
			return
		}
	}
	w.ResponseWriter.WriteHeaderNow()
}

func (w *responseCompressionWriter) Write(payload []byte) (int, error) {
	if len(payload) == 0 {
		if w.status < 0 {
			w.status = http.StatusOK
		}
		return 0, nil
	}
	if w.status < 0 {
		w.status = http.StatusOK
	}
	if w.decided {
		written, errWrite := w.destination().Write(payload)
		w.size += written
		return written, errWrite
	}

	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", http.DetectContentType(payload))
	}
	if !w.compressionEligible() {
		if errIdentity := w.startIdentity(); errIdentity != nil {
			return 0, errIdentity
		}
		if errBuffer := w.writeBuffered(); errBuffer != nil {
			return 0, errBuffer
		}
		written, errWrite := w.ResponseWriter.Write(payload)
		w.size += written
		return written, errWrite
	}

	if len(w.buffer)+len(payload) < responseCompressionMinBytes {
		w.buffer = append(w.buffer, payload...)
		w.size += len(payload)
		return len(payload), nil
	}
	if errCompressed := w.startCompressed(); errCompressed != nil {
		if errIdentity := w.startIdentity(); errIdentity != nil {
			return 0, errIdentity
		}
	}
	if errBuffer := w.writeBuffered(); errBuffer != nil {
		return 0, errBuffer
	}
	written, errWrite := w.destination().Write(payload)
	w.size += written
	return written, errWrite
}

func (w *responseCompressionWriter) WriteString(value string) (int, error) {
	return w.Write([]byte(value))
}

func (w *responseCompressionWriter) Flush() {
	if !w.decided {
		if errIdentity := w.startIdentity(); errIdentity != nil {
			return
		}
		if errBuffer := w.writeBuffered(); errBuffer != nil {
			return
		}
	}
	if w.encoder != nil {
		if errFlush := w.encoder.Flush(); errFlush != nil {
			return
		}
	}
	w.ResponseWriter.Flush()
}

func (w *responseCompressionWriter) Status() int {
	if w.status >= 0 {
		return w.status
	}
	return w.ResponseWriter.Status()
}

func (w *responseCompressionWriter) Size() int {
	return w.size
}

func (w *responseCompressionWriter) Written() bool {
	return w.status >= 0 || w.size > 0 || w.ResponseWriter.Written()
}

func (w *responseCompressionWriter) finish() error {
	if !w.decided {
		if len(w.buffer) > 0 {
			if errIdentity := w.startIdentity(); errIdentity != nil {
				return errIdentity
			}
			if errBuffer := w.writeBuffered(); errBuffer != nil {
				return errBuffer
			}
		} else if w.status >= 0 {
			w.ResponseWriter.WriteHeader(w.status)
			w.decided = true
		}
	}
	if w.encoder != nil {
		return w.encoder.Close()
	}
	return nil
}

func (w *responseCompressionWriter) compressionEligible() bool {
	if responseStatusHasNoBody(w.status) || strings.TrimSpace(w.Header().Get("Content-Encoding")) != "" || strings.TrimSpace(w.Header().Get("Content-Range")) != "" {
		return false
	}
	for _, directive := range strings.Split(w.Header().Get("Cache-Control"), ",") {
		if strings.EqualFold(strings.TrimSpace(directive), "no-transform") {
			return false
		}
	}
	mediaType, _, errMediaType := mime.ParseMediaType(w.Header().Get("Content-Type"))
	if errMediaType != nil {
		mediaType = strings.TrimSpace(strings.SplitN(w.Header().Get("Content-Type"), ";", 2)[0])
	}
	return isCompressibleMediaType(strings.ToLower(mediaType))
}

func responseStatusHasNoBody(status int) bool {
	return status < http.StatusOK || status == http.StatusNoContent || status == http.StatusNotModified
}

func isCompressibleMediaType(mediaType string) bool {
	if strings.HasPrefix(mediaType, "text/") {
		return mediaType != "text/event-stream"
	}
	if strings.HasSuffix(mediaType, "+json") || strings.HasSuffix(mediaType, "+xml") {
		return true
	}
	switch mediaType {
	case "application/json",
		"application/javascript",
		"application/xml",
		"application/yaml",
		"application/x-yaml",
		"image/svg+xml":
		return true
	default:
		return false
	}
}

func (w *responseCompressionWriter) startCompressed() error {
	if w.decided {
		return nil
	}
	var (
		encoder responseCompressionEncoder
		err     error
	)
	switch w.encoding {
	case "gzip":
		encoder, err = gzip.NewWriterLevel(w.ResponseWriter, gzip.BestSpeed)
	case "zstd":
		encoder, err = zstd.NewWriter(w.ResponseWriter, zstd.WithEncoderLevel(zstd.SpeedFastest))
	default:
		return fmt.Errorf("unsupported response encoding %q", w.encoding)
	}
	if err != nil {
		return err
	}
	addVaryHeader(w.Header(), "Accept-Encoding")
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Encoding", w.encoding)
	w.ResponseWriter.WriteHeader(w.statusCode())
	w.encoder = encoder
	w.decided = true
	return nil
}

func (w *responseCompressionWriter) startIdentity() error {
	if w.decided {
		return nil
	}
	addVaryHeader(w.Header(), "Accept-Encoding")
	w.ResponseWriter.WriteHeader(w.statusCode())
	w.decided = true
	return nil
}

func (w *responseCompressionWriter) writeBuffered() error {
	if len(w.buffer) == 0 {
		return nil
	}
	_, errWrite := w.destination().Write(w.buffer)
	w.buffer = w.buffer[:0]
	return errWrite
}

func (w *responseCompressionWriter) destination() io.Writer {
	if w.encoder != nil {
		return w.encoder
	}
	return w.ResponseWriter
}

func (w *responseCompressionWriter) statusCode() int {
	if w.status >= 0 {
		return w.status
	}
	return http.StatusOK
}

func addVaryHeader(header http.Header, value string) {
	for _, current := range header.Values("Vary") {
		for _, token := range strings.Split(current, ",") {
			if token == "*" || strings.EqualFold(strings.TrimSpace(token), value) {
				return
			}
		}
	}
	header.Add("Vary", value)
}

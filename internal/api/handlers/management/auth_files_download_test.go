package management

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestDownloadAuthFile_ReturnsFile(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	fileName := "download-user.json"
	expected := []byte(`{"type":"codex"}`)
	if err := os.WriteFile(filepath.Join(authDir, fileName), expected, 0o600); err != nil {
		t.Fatalf("failed to write auth file: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/download?name="+url.QueryEscape(fileName), nil)
	h.DownloadAuthFile(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected download status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if got := rec.Body.Bytes(); string(got) != string(expected) {
		t.Fatalf("unexpected download content: %q", string(got))
	}
}

func TestDownloadAuthFile_RejectsPathSeparators(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)

	for _, name := range []string{
		"../external/secret.json",
		`..\\external\\secret.json`,
		"nested/secret.json",
		`nested\\secret.json`,
	} {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/download?name="+url.QueryEscape(name), nil)
		h.DownloadAuthFile(ctx)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected %d for name %q, got %d with body %s", http.StatusBadRequest, name, rec.Code, rec.Body.String())
		}
	}
}

func TestDownloadAuthFile_All_ReturnsZipArchive(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	files := map[string]string{
		"beta.json":  `{"type":"beta"}`,
		"alpha.json": `{"type":"alpha"}`,
		"notes.txt":  `ignore me`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(authDir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("failed to seed %s: %v", name, err)
		}
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/download?all=true", nil)
	h.DownloadAuthFile(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected batch download status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, `filename="auth-files.zip"`) {
		t.Fatalf("unexpected content disposition: %q", got)
	}

	reader, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("expected zip payload: %v", err)
	}
	if len(reader.File) != 2 {
		t.Fatalf("expected 2 json files in zip, got %d", len(reader.File))
	}

	contents := make(map[string]string, len(reader.File))
	for _, file := range reader.File {
		rc, errOpen := file.Open()
		if errOpen != nil {
			t.Fatalf("open zip entry %s: %v", file.Name, errOpen)
		}
		body, errRead := io.ReadAll(rc)
		_ = rc.Close()
		if errRead != nil {
			t.Fatalf("read zip entry %s: %v", file.Name, errRead)
		}
		contents[file.Name] = string(body)
	}

	if got := contents["alpha.json"]; got != files["alpha.json"] {
		t.Fatalf("unexpected alpha.json contents: %q", got)
	}
	if got := contents["beta.json"]; got != files["beta.json"] {
		t.Fatalf("unexpected beta.json contents: %q", got)
	}
	if _, exists := contents["notes.txt"]; exists {
		t.Fatalf("did not expect notes.txt in zip")
	}
}

package management

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func buildAuthZipArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	archive := zip.NewWriter(&buf)
	for name, content := range files {
		writer, errCreate := archive.Create(name)
		if errCreate != nil {
			t.Fatalf("create zip entry %s: %v", name, errCreate)
		}
		if _, errWrite := writer.Write([]byte(content)); errWrite != nil {
			t.Fatalf("write zip entry %s: %v", name, errWrite)
		}
	}
	if errClose := archive.Close(); errClose != nil {
		t.Fatalf("close zip archive: %v", errClose)
	}
	return buf.Bytes()
}

func buildAuthTarArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	archive := tar.NewWriter(&buf)
	for name, content := range files {
		payload := []byte(content)
		header := &tar.Header{
			Name: name,
			Mode: 0o600,
			Size: int64(len(payload)),
		}
		if errWrite := archive.WriteHeader(header); errWrite != nil {
			t.Fatalf("write tar header %s: %v", name, errWrite)
		}
		if _, errWrite := archive.Write(payload); errWrite != nil {
			t.Fatalf("write tar entry %s: %v", name, errWrite)
		}
	}
	if errClose := archive.Close(); errClose != nil {
		t.Fatalf("close tar archive: %v", errClose)
	}
	return buf.Bytes()
}

func buildAuthTarGzArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	tarBytes := buildAuthTarArchive(t, files)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, errWrite := gz.Write(tarBytes); errWrite != nil {
		t.Fatalf("write gzip payload: %v", errWrite)
	}
	if errClose := gz.Close(); errClose != nil {
		t.Fatalf("close gzip writer: %v", errClose)
	}
	return buf.Bytes()
}

func TestDetectAuthArchiveFormat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		filename    string
		contentType string
		want        authArchiveFormat
	}{
		{name: "zip-ext", filename: "bundle.zip", want: authArchiveZip},
		{name: "tar-ext", filename: "bundle.tar", want: authArchiveTar},
		{name: "tar-gz-ext", filename: "bundle.tar.gz", want: authArchiveTarGz},
		{name: "tgz-ext", filename: "bundle.tgz", want: authArchiveTarGz},
		{name: "json-ext", filename: "auth.json", want: authArchiveUnknown},
		{name: "zip-ct", contentType: "application/zip", want: authArchiveZip},
		{name: "tar-ct", contentType: "application/x-tar", want: authArchiveTar},
		{name: "gzip-ct", contentType: "application/gzip", want: authArchiveTarGz},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := detectAuthArchiveFormat(tc.filename, tc.contentType); got != tc.want {
				t.Fatalf("detectAuthArchiveFormat(%q, %q) = %v, want %v", tc.filename, tc.contentType, got, tc.want)
			}
		})
	}
}

func TestUploadAuthFile_ZipMultipart(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	zipBytes := buildAuthZipArchive(t, map[string]string{
		"nested/xai-alpha@example.com.json": `{"type":"xai","email":"alpha@example.com","access_token":"a"}`,
		"xai-beta@example.com.json":         `{"type":"xai","email":"beta@example.com","access_token":"b"}`,
		"readme.txt":                        "ignore me",
		"__MACOSX/._xai-skip.json":          `{"type":"xai"}`,
	})

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, errCreate := writer.CreateFormFile("file", "auths.zip")
	if errCreate != nil {
		t.Fatalf("create multipart file: %v", errCreate)
	}
	if _, errWrite := part.Write(zipBytes); errWrite != nil {
		t.Fatalf("write multipart zip: %v", errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart writer: %v", errClose)
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	ctx.Request = req

	h.UploadAuthFile(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if got := int(payload["uploaded"].(float64)); got != 2 {
		t.Fatalf("uploaded = %d, want 2; body = %s", got, rec.Body.String())
	}

	for _, name := range []string{"xai-alpha@example.com.json", "xai-beta@example.com.json"} {
		if _, errStat := os.Stat(filepath.Join(authDir, name)); errStat != nil {
			t.Fatalf("expected extracted auth file %s: %v", name, errStat)
		}
	}
	if got := len(manager.List()); got != 2 {
		t.Fatalf("registered auth count = %d, want 2", got)
	}
}

func TestUploadAuthFile_ZipRawBody(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	zipBytes := buildAuthZipArchive(t, map[string]string{
		"xai-one@example.com.json": `{"type":"xai","email":"one@example.com","access_token":"1"}`,
		"xai-two@example.com.json": `{"type":"xai","email":"two@example.com","access_token":"2"}`,
	})

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(
		http.MethodPost,
		"/v0/management/auth-files?name="+url.QueryEscape("bundle.zip"),
		bytes.NewReader(zipBytes),
	)
	req.Header.Set("Content-Type", "application/zip")
	ctx.Request = req

	h.UploadAuthFile(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if got := int(payload["uploaded"].(float64)); got != 2 {
		t.Fatalf("uploaded = %d, want 2; body = %s", got, rec.Body.String())
	}
	if got := len(manager.List()); got != 2 {
		t.Fatalf("registered auth count = %d, want 2", got)
	}
}

func TestUploadAuthFile_ZipRejectsTraversal(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	// Entry path looks nested/absolute-ish; we only persist basename and reject unsafe basenames.
	zipBytes := buildAuthZipArchive(t, map[string]string{
		"../escape.json":               `{"type":"xai","access_token":"bad"}`,
		"ok/xai-safe@example.com.json": `{"type":"xai","email":"safe@example.com","access_token":"ok"}`,
	})

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files?name=bundle.zip", bytes.NewReader(zipBytes))
	req.Header.Set("Content-Type", "application/zip")
	ctx.Request = req

	h.UploadAuthFile(ctx)

	// escape.json basename is safe after filepath.Base, so both may import — ensure no file lands outside authDir.
	entries, errRead := os.ReadDir(authDir)
	if errRead != nil {
		t.Fatalf("read auth dir: %v", errRead)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "..") || strings.ContainsAny(entry.Name(), `/\`) {
			t.Fatalf("unsafe file name written: %q", entry.Name())
		}
	}
	// Parent of authDir must not gain escape.json
	if _, errStat := os.Stat(filepath.Join(filepath.Dir(authDir), "escape.json")); errStat == nil {
		t.Fatalf("zip slip wrote outside auth dir")
	}
	_ = rec
}

func TestUploadAuthFile_TarMultipart(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	tarBytes := buildAuthTarArchive(t, map[string]string{
		"nested/xai-alpha@example.com.json": `{"type":"xai","email":"alpha@example.com","access_token":"a"}`,
		"xai-beta@example.com.json":         `{"type":"xai","email":"beta@example.com","access_token":"b"}`,
		"readme.txt":                        "ignore me",
	})

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, errCreate := writer.CreateFormFile("file", "auths.tar")
	if errCreate != nil {
		t.Fatalf("create multipart file: %v", errCreate)
	}
	if _, errWrite := part.Write(tarBytes); errWrite != nil {
		t.Fatalf("write multipart tar: %v", errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart writer: %v", errClose)
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	ctx.Request = req

	h.UploadAuthFile(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if got := int(payload["uploaded"].(float64)); got != 2 {
		t.Fatalf("uploaded = %d, want 2; body = %s", got, rec.Body.String())
	}
	for _, name := range []string{"xai-alpha@example.com.json", "xai-beta@example.com.json"} {
		if _, errStat := os.Stat(filepath.Join(authDir, name)); errStat != nil {
			t.Fatalf("expected extracted auth file %s: %v", name, errStat)
		}
	}
	if got := len(manager.List()); got != 2 {
		t.Fatalf("registered auth count = %d, want 2", got)
	}
}

func TestUploadAuthFile_TarGzRawBody(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	archiveBytes := buildAuthTarGzArchive(t, map[string]string{
		"nested/xai-one@example.com.json": `{"type":"xai","email":"one@example.com","access_token":"1"}`,
		"xai-two@example.com.json":        `{"type":"xai","email":"two@example.com","access_token":"2"}`,
		"notes.md":                        "skip",
	})

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(
		http.MethodPost,
		"/v0/management/auth-files?name="+url.QueryEscape("bundle.tgz"),
		bytes.NewReader(archiveBytes),
	)
	req.Header.Set("Content-Type", "application/gzip")
	ctx.Request = req

	h.UploadAuthFile(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if got := int(payload["uploaded"].(float64)); got != 2 {
		t.Fatalf("uploaded = %d, want 2; body = %s", got, rec.Body.String())
	}
	if got := len(manager.List()); got != 2 {
		t.Fatalf("registered auth count = %d, want 2", got)
	}
}

func TestUploadAuthFile_TarRejectsTraversal(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	tarBytes := buildAuthTarArchive(t, map[string]string{
		"../escape.json":               `{"type":"xai","access_token":"bad"}`,
		"ok/xai-safe@example.com.json": `{"type":"xai","email":"safe@example.com","access_token":"ok"}`,
	})

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files?name=bundle.tar", bytes.NewReader(tarBytes))
	req.Header.Set("Content-Type", "application/x-tar")
	ctx.Request = req

	h.UploadAuthFile(ctx)

	entries, errRead := os.ReadDir(authDir)
	if errRead != nil {
		t.Fatalf("read auth dir: %v", errRead)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "..") || strings.ContainsAny(entry.Name(), `/\`) {
			t.Fatalf("unsafe file name written: %q", entry.Name())
		}
	}
	if _, errStat := os.Stat(filepath.Join(filepath.Dir(authDir), "escape.json")); errStat == nil {
		t.Fatalf("tar slip wrote outside auth dir")
	}
	_ = rec
}

func TestUploadAuthFile_BatchMultipartExceedsDefaultPartLimit(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	manager := coreauth.NewManager(store, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	const fileCount = 2465
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for fileIndex := 0; fileIndex < fileCount; fileIndex++ {
		part, errCreate := writer.CreateFormFile("file", fmt.Sprintf("xai-%04d.json", fileIndex))
		if errCreate != nil {
			t.Fatalf("create multipart file: %v", errCreate)
		}
		if _, errWrite := part.Write([]byte(`{"type":"xai","access_token":"token"}`)); errWrite != nil {
			t.Fatalf("write multipart file: %v", errWrite)
		}
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart writer: %v", errClose)
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	ctx.Request = req

	h.UploadAuthFile(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := len(manager.List()); got != fileCount {
		t.Fatalf("registered auth count = %d, want %d", got, fileCount)
	}
}

func TestUploadAuthFile_BatchMultipart(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	files := []struct {
		name    string
		content string
	}{
		{name: "alpha.json", content: `{"type":"codex","email":"alpha@example.com"}`},
		{name: "beta.json", content: `{"type":"claude","email":"beta@example.com"}`},
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, file := range files {
		part, err := writer.CreateFormFile("file", file.name)
		if err != nil {
			t.Fatalf("failed to create multipart file: %v", err)
		}
		if _, err = part.Write([]byte(file.content)); err != nil {
			t.Fatalf("failed to write multipart content: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("failed to close multipart writer: %v", err)
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	ctx.Request = req

	h.UploadAuthFile(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected upload status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if got, ok := payload["uploaded"].(float64); !ok || int(got) != len(files) {
		t.Fatalf("expected uploaded=%d, got %#v", len(files), payload["uploaded"])
	}

	for _, file := range files {
		fullPath := filepath.Join(authDir, file.name)
		data, err := os.ReadFile(fullPath)
		if err != nil {
			t.Fatalf("expected uploaded file %s to exist: %v", file.name, err)
		}
		if string(data) != file.content {
			t.Fatalf("expected file %s content %q, got %q", file.name, file.content, string(data))
		}
	}

	auths := manager.List()
	if len(auths) != len(files) {
		t.Fatalf("expected %d auth entries, got %d", len(files), len(auths))
	}
}

func TestUploadAuthFile_WatcherBackedBatchDefersRuntimeLoading(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.SetAuthLoadStatusProvider(func() watcher.AuthLoadStatus {
		return watcher.AuthLoadStatus{State: watcher.AuthLoadStateReady}
	})

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, name := range []string{"alpha.json", "beta.json"} {
		part, errCreate := writer.CreateFormFile("file", name)
		if errCreate != nil {
			t.Fatalf("create multipart file: %v", errCreate)
		}
		if _, errWrite := part.Write([]byte(`{"type":"codex","access_token":"token"}`)); errWrite != nil {
			t.Fatalf("write multipart file: %v", errWrite)
		}
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart writer: %v", errClose)
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	ctx.Request = req

	h.UploadAuthFile(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := len(manager.List()); got != 0 {
		t.Fatalf("synchronously registered auth count = %d, want 0", got)
	}
	for _, name := range []string{"alpha.json", "beta.json"} {
		if _, errStat := os.Stat(filepath.Join(authDir, name)); errStat != nil {
			t.Fatalf("uploaded file %s was not persisted: %v", name, errStat)
		}
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if got := int(payload["uploaded"].(float64)); got != 2 {
		t.Fatalf("uploaded count = %d, want 2", got)
	}
	if _, returnedFiles := payload["files"]; returnedFiles {
		t.Fatalf("response returned uploaded file details: %s", rec.Body.String())
	}
}

func TestUploadAuthFile_BatchMultipart_InvalidJSONDoesNotOverwriteExistingFile(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	existingName := "alpha.json"
	existingContent := `{"type":"codex","email":"alpha@example.com"}`
	if err := os.WriteFile(filepath.Join(authDir, existingName), []byte(existingContent), 0o600); err != nil {
		t.Fatalf("failed to seed existing auth file: %v", err)
	}

	files := []struct {
		name    string
		content string
	}{
		{name: existingName, content: `{"type":"codex"`},
		{name: "beta.json", content: `{"type":"claude","email":"beta@example.com"}`},
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, file := range files {
		part, err := writer.CreateFormFile("file", file.name)
		if err != nil {
			t.Fatalf("failed to create multipart file: %v", err)
		}
		if _, err = part.Write([]byte(file.content)); err != nil {
			t.Fatalf("failed to write multipart content: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("failed to close multipart writer: %v", err)
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	ctx.Request = req

	h.UploadAuthFile(ctx)

	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("expected upload status %d, got %d with body %s", http.StatusMultiStatus, rec.Code, rec.Body.String())
	}

	data, err := os.ReadFile(filepath.Join(authDir, existingName))
	if err != nil {
		t.Fatalf("expected existing auth file to remain readable: %v", err)
	}
	if string(data) != existingContent {
		t.Fatalf("expected existing auth file to remain %q, got %q", existingContent, string(data))
	}

	betaData, err := os.ReadFile(filepath.Join(authDir, "beta.json"))
	if err != nil {
		t.Fatalf("expected valid auth file to be created: %v", err)
	}
	if string(betaData) != files[1].content {
		t.Fatalf("expected beta auth file content %q, got %q", files[1].content, string(betaData))
	}
}

func TestDeleteAuthFile_BatchQuery(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	files := []string{"alpha.json", "beta.json"}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(authDir, name), []byte(`{"type":"codex"}`), 0o600); err != nil {
			t.Fatalf("failed to write auth file %s: %v", name, err)
		}
	}

	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(
		http.MethodDelete,
		"/v0/management/auth-files?name="+url.QueryEscape(files[0])+"&name="+url.QueryEscape(files[1]),
		nil,
	)
	ctx.Request = req

	h.DeleteAuthFile(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected delete status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if got, ok := payload["deleted"].(float64); !ok || int(got) != len(files) {
		t.Fatalf("expected deleted=%d, got %#v", len(files), payload["deleted"])
	}

	for _, name := range files {
		if _, err := os.Stat(filepath.Join(authDir, name)); !os.IsNotExist(err) {
			t.Fatalf("expected auth file %s to be removed, stat err: %v", name, err)
		}
	}
}

package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestDeleteAuthFilesFiltered(t *testing.T) {
	h, paths := newFilteredDeleteHandler(t)

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodDelete,
		"/v0/management/auth-files?all=true&type=codex&disabled_only=true", nil)
	h.DeleteAuthFile(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Deleted int      `json:"deleted"`
		Files   []string `json:"files"`
		Failed  []any    `json:"failed"`
	}
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &response); errDecode != nil {
		t.Fatal(errDecode)
	}
	if response.Deleted != 1 || len(response.Files) != 1 || response.Files[0] != "codex-disabled.json" || len(response.Failed) != 0 {
		t.Fatalf("unexpected filtered delete response: %#v", response)
	}
	if _, errStat := os.Stat(paths["codex-disabled.json"]); !os.IsNotExist(errStat) {
		t.Fatalf("disabled Codex auth must be deleted, stat error = %v", errStat)
	}
	for _, name := range []string{"codex-enabled.json", "claude-enabled.json"} {
		if _, errStat := os.Stat(paths[name]); errStat != nil {
			t.Fatalf("non-matching auth %q must remain, stat error = %v", name, errStat)
		}
	}
}

func TestDeleteAuthFilesFiltered_RejectsSearch(t *testing.T) {
	h, paths := newFilteredDeleteHandler(t)

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodDelete,
		"/v0/management/auth-files?all=true&search=*codex*", nil)
	h.DeleteAuthFile(ginCtx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	for name, path := range paths {
		if _, errStat := os.Stat(path); errStat != nil {
			t.Fatalf("auth %q must remain after rejected search delete, stat error = %v", name, errStat)
		}
	}
}

func newFilteredDeleteHandler(t *testing.T) (*Handler, map[string]string) {
	t.Helper()
	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	paths := make(map[string]string)
	for _, auth := range []*coreauth.Auth{
		{ID: "codex-disabled.json", FileName: "codex-disabled.json", Provider: "codex", Disabled: true, Status: coreauth.StatusDisabled},
		{ID: "codex-enabled.json", FileName: "codex-enabled.json", Provider: "codex"},
		{ID: "claude-enabled.json", FileName: "claude-enabled.json", Provider: "claude"},
	} {
		path := filepath.Join(authDir, auth.FileName)
		paths[auth.FileName] = path
		contents := []byte(`{"type":"` + auth.Provider + `"}`)
		if errWrite := os.WriteFile(path, contents, 0o600); errWrite != nil {
			t.Fatal(errWrite)
		}
		auth.Attributes = map[string]string{"path": path}
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}
	return h, paths
}

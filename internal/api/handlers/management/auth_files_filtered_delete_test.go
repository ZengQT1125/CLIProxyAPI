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
	for _, rawQuery := range []string{"search=*codex*", "search=", "search=%20"} {
		t.Run(rawQuery, func(t *testing.T) {
			h, paths := newFilteredDeleteHandler(t)

			recorder := httptest.NewRecorder()
			ginCtx, _ := gin.CreateTestContext(recorder)
			ginCtx.Request = httptest.NewRequest(http.MethodDelete,
				"/v0/management/auth-files?all=true&"+rawQuery, nil)
			h.DeleteAuthFile(ginCtx)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			for name, path := range paths {
				if _, errStat := os.Stat(path); errStat != nil {
					t.Fatalf("auth %q must remain after rejected search delete, stat error = %v", name, errStat)
				}
			}
		})
	}
}

func TestDeleteAuthFilesFiltered_SkipsHiddenAuthWithoutPath(t *testing.T) {
	authDir := t.TempDir()
	fileName := "hidden.json"
	filePath := filepath.Join(authDir, fileName)
	if errWrite := os.WriteFile(filePath, []byte(`{"type":"codex"}`), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{
		ID: fileName, FileName: fileName, Provider: "codex", Disabled: true, Status: coreauth.StatusDisabled,
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}

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
	}
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &response); errDecode != nil {
		t.Fatal(errDecode)
	}
	if response.Deleted != 0 || len(response.Files) != 0 {
		t.Fatalf("hidden auth must not be a delete candidate: %#v", response)
	}
	if _, errStat := os.Stat(filePath); errStat != nil {
		t.Fatalf("hidden auth file must remain, stat error = %v", errStat)
	}
}

func TestDeleteAuthFilesFiltered_UsesPhysicalPathForAbsoluteFileName(t *testing.T) {
	authDir := t.TempDir()
	filePath := filepath.Join(authDir, "absolute-name.json")
	if errWrite := os.WriteFile(filePath, []byte(`{"type":"codex"}`), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "absolute-name.json",
		FileName: filePath,
		Provider: "codex",
		Attributes: map[string]string{
			"path": filePath,
		},
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/auth-files?all=true&type=codex", nil)
	h.DeleteAuthFile(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Deleted int      `json:"deleted"`
		Files   []string `json:"files"`
	}
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &response); errDecode != nil {
		t.Fatal(errDecode)
	}
	if response.Deleted != 1 || len(response.Files) != 1 || response.Files[0] != "absolute-name.json" {
		t.Fatalf("unexpected absolute filename delete response: %#v", response)
	}
	if _, errStat := os.Stat(filePath); !os.IsNotExist(errStat) {
		t.Fatalf("physical auth file must be deleted, stat error = %v", errStat)
	}
}

func TestDeleteAuthFilesFiltered_SkipsPathOutsideAuthDir(t *testing.T) {
	authDir := t.TempDir()
	externalPath := filepath.Join(t.TempDir(), "external.json")
	if errWrite := os.WriteFile(externalPath, []byte(`{"type":"codex"}`), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "external.json",
		FileName: externalPath,
		Provider: "codex",
		Attributes: map[string]string{
			"path": externalPath,
		},
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/auth-files?all=true&type=codex", nil)
	h.DeleteAuthFile(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Deleted int `json:"deleted"`
	}
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &response); errDecode != nil {
		t.Fatal(errDecode)
	}
	if response.Deleted != 0 {
		t.Fatalf("outside auth path must not be deleted: %#v", response)
	}
	if _, errStat := os.Stat(externalPath); errStat != nil {
		t.Fatalf("outside auth file must remain, stat error = %v", errStat)
	}
	if _, ok := manager.GetByID("external.json"); !ok {
		t.Fatal("outside auth record must remain")
	}
}

func TestDeleteAuthFilesFiltered_DeletesExactNestedPath(t *testing.T) {
	authDir := t.TempDir()
	targetPath := filepath.Join(authDir, "a", "shared.json")
	foreignPath := filepath.Join(authDir, "b", "shared.json")
	for _, path := range []string{targetPath, foreignPath} {
		if errMkdir := os.MkdirAll(filepath.Dir(path), 0o700); errMkdir != nil {
			t.Fatal(errMkdir)
		}
		if errWrite := os.WriteFile(path, []byte(`{"type":"codex"}`), 0o600); errWrite != nil {
			t.Fatal(errWrite)
		}
	}
	manager := coreauth.NewManager(nil, nil, nil)
	for _, auth := range []*coreauth.Auth{
		{ID: "target-auth", FileName: targetPath, Provider: "codex", Attributes: map[string]string{"path": targetPath}},
		{ID: "shared.json", FileName: foreignPath, Provider: "claude", Attributes: map[string]string{"path": foreignPath}},
	} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/auth-files?all=true&type=codex", nil)
	h.DeleteAuthFile(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if _, errStat := os.Stat(targetPath); !os.IsNotExist(errStat) {
		t.Fatalf("matched nested auth must be deleted, stat error = %v", errStat)
	}
	if _, errStat := os.Stat(foreignPath); errStat != nil {
		t.Fatalf("same-basename auth from another provider must remain, stat error = %v", errStat)
	}
	if _, ok := manager.GetByID("target-auth"); ok {
		t.Fatal("matched auth record must be removed")
	}
	if _, ok := manager.GetByID("shared.json"); !ok {
		t.Fatal("non-matching auth record must remain")
	}
}

func TestDeleteAuthFilesFiltered_PartialFailure(t *testing.T) {
	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	existingPath := filepath.Join(authDir, "existing.json")
	if errWrite := os.WriteFile(existingPath, []byte(`{"type":"codex"}`), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	for _, auth := range []*coreauth.Auth{
		{ID: "existing.json", FileName: "existing.json", Provider: "codex", Attributes: map[string]string{"path": existingPath}},
		{ID: "missing.json", FileName: "missing.json", Provider: "codex", Attributes: map[string]string{"path": filepath.Join(authDir, "missing.json")}},
	} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/auth-files?all=true&type=codex", nil)
	h.DeleteAuthFile(ginCtx)

	if recorder.Code != http.StatusMultiStatus {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Deleted int      `json:"deleted"`
		Files   []string `json:"files"`
		Failed  []struct {
			Name  string `json:"name"`
			Error string `json:"error"`
		} `json:"failed"`
	}
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &response); errDecode != nil {
		t.Fatal(errDecode)
	}
	if response.Deleted != 1 || len(response.Files) != 1 || response.Files[0] != "existing.json" || len(response.Failed) != 1 || response.Failed[0].Name != "missing.json" || response.Failed[0].Error == "" {
		t.Fatalf("unexpected partial delete response: %#v", response)
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

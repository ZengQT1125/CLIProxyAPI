package management

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginhost"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestPluginLoginPollAuthsExpandsMultipleAuths(t *testing.T) {
	host := pluginhost.New()
	resp := pluginapi.AuthLoginPollResponse{
		Status: pluginapi.AuthLoginStatusSuccess,
		Auths: []pluginapi.AuthData{
			{
				Provider:    "gemini-cli",
				ID:          "geminicli.json",
				FileName:    "geminicli.json",
				StorageJSON: []byte(`{"type":"gemini-cli"}`),
			},
			{
				Provider:    "gemini-cli",
				ID:          "geminicli-project-a.json",
				FileName:    "geminicli-project-a.json",
				StorageJSON: []byte(`{"type":"gemini-cli","project_id":"project-a"}`),
				Metadata:    map[string]any{"project_id": "project-a"},
			},
		},
	}

	records := pluginLoginPollAuths(host, resp)
	if len(records) != 2 {
		t.Fatalf("pluginLoginPollAuths() len = %d, want two records", len(records))
	}
	if records[0].ID != "geminicli.json" || records[1].ID != "geminicli-project-a.json" {
		t.Fatalf("records = %#v, want both plugin auths", records)
	}
	if gotProject := records[1].Metadata["project_id"]; gotProject != "project-a" {
		t.Fatalf("project_id = %#v, want project-a", gotProject)
	}
}

func TestSavePluginLoginRecordsRollsBackSavedAuthsOnFailure(t *testing.T) {
	store := &pluginLoginRollbackStore{failAt: 2}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	h.tokenStore = store

	records := []*coreauth.Auth{
		{
			ID:       "geminicli.json",
			FileName: "geminicli.json",
			Provider: "gemini-cli",
			Metadata: map[string]any{"type": "gemini-cli"},
		},
		{
			ID:       "geminicli-project-a.json",
			FileName: "geminicli-project-a.json",
			Provider: "gemini-cli",
			Metadata: map[string]any{"type": "gemini-cli", "project_id": "project-a"},
		},
	}

	errSave := h.savePluginLoginRecords(context.Background(), records)
	if errSave == nil {
		t.Fatal("savePluginLoginRecords() error = nil, want rollback-triggering error")
	}
	if len(store.saved) != 2 {
		t.Fatalf("saved len = %d, want two attempted saves", len(store.saved))
	}
	if !store.deleted["geminicli.json"] || !store.deleted["geminicli-project-a.json"] {
		t.Fatalf("deleted = %#v, want both saved auths rolled back", store.deleted)
	}
}

func TestSavePluginLoginRecordsRollbackNotifiesOnlySuccessfulDeletes(t *testing.T) {
	authDir := t.TempDir()
	firstPath := filepath.Join(authDir, "geminicli.json")
	secondPath := filepath.Join(authDir, "geminicli-project-a.json")
	store := &pluginLoginRollbackStore{
		failAt:  2,
		baseDir: authDir,
		deleteErrors: map[string]error{
			secondPath: errors.New("delete failed"),
		},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	for _, record := range []*coreauth.Auth{
		{ID: "geminicli.json", FileName: "geminicli.json", Provider: "gemini-cli", Attributes: map[string]string{"path": firstPath}},
		{ID: "geminicli-project-a.json", FileName: "geminicli-project-a.json", Provider: "gemini-cli", Attributes: map[string]string{"path": secondPath}},
	} {
		if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
			t.Fatalf("register auth %s: %v", record.ID, errRegister)
		}
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = store
	notifications := make(map[string]int)
	h.SetAuthFileMutationHook(func(path string) {
		notifications[path]++
		if _, ok := manager.GetByID(filepath.Base(path)); !ok {
			t.Errorf("runtime auth %q changed before mutation hook", filepath.Base(path))
		}
	})

	records := []*coreauth.Auth{
		{ID: "geminicli.json", FileName: "geminicli.json", Provider: "gemini-cli", Metadata: map[string]any{"type": "gemini-cli"}},
		{ID: "geminicli-project-a.json", FileName: "geminicli-project-a.json", Provider: "gemini-cli", Metadata: map[string]any{"type": "gemini-cli", "project_id": "project-a"}},
	}
	errSave := h.savePluginLoginRecords(context.Background(), records)
	if errSave == nil {
		t.Fatal("savePluginLoginRecords() error = nil, want rollback-triggering error")
	}
	normalizedAuthDir, errEval := filepath.EvalSymlinks(authDir)
	if errEval != nil {
		t.Fatalf("resolve auth dir: %v", errEval)
	}
	normalizedFirstPath := filepath.Join(normalizedAuthDir, filepath.Base(firstPath))
	normalizedSecondPath := filepath.Join(normalizedAuthDir, filepath.Base(secondPath))
	if got := notifications[normalizedFirstPath]; got != 2 {
		t.Fatalf("successful path notifications = %d, want save and rollback delete notifications", got)
	}
	if got := notifications[normalizedSecondPath]; got != 0 {
		t.Fatalf("failed delete path notifications = %d, want 0", got)
	}
	if _, ok := manager.GetByID(filepath.Base(firstPath)); ok {
		t.Fatal("successfully rolled back runtime auth still registered")
	}
	if _, ok := manager.GetByID(filepath.Base(secondPath)); !ok {
		t.Fatal("runtime auth removed despite token-store delete failure")
	}
}

func TestPatchPluginVirtualAuthStatusReturnsConflictForVirtualChild(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth := pluginVirtualAuthForTest(t.TempDir(), "source.json", "auth-1")
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register virtual auth: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/status", strings.NewReader(`{"name":"auth-1","disabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req

	h.PatchAuthFileStatus(ctx)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusConflict, rec.Body.String())
	}
}

func TestPatchPluginVirtualSourceStatusDisablesAllExpandedAuths(t *testing.T) {
	authDir := t.TempDir()
	fileName := "source.json"
	filePath := filepath.Join(authDir, fileName)
	if errWrite := os.WriteFile(filePath, []byte(`{"type":"gemini-cli","disabled":false}`), 0o600); errWrite != nil {
		t.Fatalf("write source auth file: %v", errWrite)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	for _, id := range []string{"source.json", "virtual-project-a"} {
		auth := pluginVirtualAuthForTest(authDir, fileName, id)
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register virtual auth %s: %v", id, errRegister)
		}
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/status", strings.NewReader(`{"name":"source.json","disabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req

	h.PatchAuthFileStatus(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	raw, errRead := os.ReadFile(filePath)
	if errRead != nil {
		t.Fatalf("read source auth file: %v", errRead)
	}
	if !strings.Contains(string(raw), `"disabled":true`) {
		t.Fatalf("source auth file = %s, want disabled:true", string(raw))
	}
	for _, id := range []string{"source.json", "virtual-project-a"} {
		auth, ok := manager.GetByID(id)
		if !ok || auth == nil {
			t.Fatalf("expected auth %s to remain registered", id)
		}
		if !auth.Disabled || auth.Status != coreauth.StatusDisabled {
			t.Fatalf("auth %s disabled/status = %v/%s, want disabled", id, auth.Disabled, auth.Status)
		}
	}
}

func TestPatchPluginVirtualAuthFieldsReturnsConflict(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth := pluginVirtualAuthForTest(t.TempDir(), "source.json", "auth-1")
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register virtual auth: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/fields", strings.NewReader(`{"name":"auth-1","note":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req

	h.PatchAuthFileFields(ctx)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusConflict, rec.Body.String())
	}
}

func TestDeletePluginVirtualSourceRemovesExpandedRuntimeAuths(t *testing.T) {
	authDir := t.TempDir()
	fileName := "source.json"
	filePath := filepath.Join(authDir, fileName)
	if errWrite := os.WriteFile(filePath, []byte(`{"type":"gemini-cli"}`), 0o600); errWrite != nil {
		t.Fatalf("write source auth file: %v", errWrite)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	for _, id := range []string{"auth-1", "auth-2"} {
		auth := pluginVirtualAuthForTest(authDir, fileName, id)
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register virtual auth %s: %v", id, errRegister)
		}
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodDelete, "/v0/management/auth-files?name="+url.QueryEscape(fileName), nil)
	ctx.Request = req

	h.DeleteAuthFile(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if _, errStat := os.Stat(filePath); !os.IsNotExist(errStat) {
		t.Fatalf("expected source auth file to be removed, stat err: %v", errStat)
	}
	for _, id := range []string{"auth-1", "auth-2"} {
		if _, ok := manager.GetByID(id); ok {
			t.Fatalf("expected virtual auth %s to be removed", id)
		}
	}
}

func pluginVirtualAuthForTest(authDir, fileName, id string) *coreauth.Auth {
	filePath := filepath.Join(authDir, fileName)
	auth := &coreauth.Auth{
		ID:       id,
		FileName: fileName,
		Provider: "gemini-cli",
		Attributes: map[string]string{
			"path": filePath,
		},
		Metadata: map[string]any{
			"type": "gemini-cli",
		},
	}
	coreauth.MarkPluginVirtualAuth(auth, filePath, 0)
	return auth
}

type pluginLoginRollbackStore struct {
	failAt       int
	baseDir      string
	saved        []string
	deleted      map[string]bool
	deleteErrors map[string]error
}

func (s *pluginLoginRollbackStore) List(context.Context) ([]*coreauth.Auth, error) {
	return nil, nil
}

func (s *pluginLoginRollbackStore) Save(_ context.Context, auth *coreauth.Auth) (string, error) {
	path := strings.TrimSpace(auth.FileName)
	if path == "" {
		path = strings.TrimSpace(auth.ID)
	}
	if s.baseDir != "" && !filepath.IsAbs(path) {
		path = filepath.Join(s.baseDir, path)
	}
	s.saved = append(s.saved, path)
	if len(s.saved) == s.failAt {
		return path, errors.New("save failed after write")
	}
	return path, nil
}

func (s *pluginLoginRollbackStore) Delete(_ context.Context, id string) error {
	if errDelete := s.deleteErrors[id]; errDelete != nil {
		return errDelete
	}
	if s.deleted == nil {
		s.deleted = make(map[string]bool)
	}
	s.deleted[id] = true
	return nil
}

func (s *pluginLoginRollbackStore) SetBaseDir(string) {}

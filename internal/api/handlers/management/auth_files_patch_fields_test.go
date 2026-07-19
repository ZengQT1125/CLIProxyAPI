package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	fileauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestPatchAuthFileFields_MergeHeadersAndDeleteEmptyValues(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:       "test.json",
		FileName: "test.json",
		Provider: "claude",
		Attributes: map[string]string{
			"path":            "/tmp/test.json",
			"header:X-Old":    "old",
			"header:X-Remove": "gone",
		},
		Metadata: map[string]any{
			"type": "claude",
			"headers": map[string]any{
				"X-Old":    "old",
				"X-Remove": "gone",
			},
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	body := `{"name":"test.json","prefix":"p1","proxy_url":"http://proxy.local","headers":{"X-Old":"new","X-New":"v","X-Remove":"  ","X-Nope":""}}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/fields", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.PatchAuthFileFields(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	updated, ok := manager.GetByID("test.json")
	if !ok || updated == nil {
		t.Fatalf("expected auth record to exist after patch")
	}

	if updated.Prefix != "p1" {
		t.Fatalf("prefix = %q, want %q", updated.Prefix, "p1")
	}
	if updated.ProxyURL != "http://proxy.local" {
		t.Fatalf("proxy_url = %q, want %q", updated.ProxyURL, "http://proxy.local")
	}

	if updated.Metadata == nil {
		t.Fatalf("expected metadata to be non-nil")
	}
	if got, _ := updated.Metadata["prefix"].(string); got != "p1" {
		t.Fatalf("metadata.prefix = %q, want %q", got, "p1")
	}
	if got, _ := updated.Metadata["proxy_url"].(string); got != "http://proxy.local" {
		t.Fatalf("metadata.proxy_url = %q, want %q", got, "http://proxy.local")
	}

	headersMeta, ok := updated.Metadata["headers"].(map[string]any)
	if !ok {
		raw, _ := json.Marshal(updated.Metadata["headers"])
		t.Fatalf("metadata.headers = %T (%s), want map[string]any", updated.Metadata["headers"], string(raw))
	}
	if got := headersMeta["X-Old"]; got != "new" {
		t.Fatalf("metadata.headers.X-Old = %#v, want %q", got, "new")
	}
	if got := headersMeta["X-New"]; got != "v" {
		t.Fatalf("metadata.headers.X-New = %#v, want %q", got, "v")
	}
	if _, ok := headersMeta["X-Remove"]; ok {
		t.Fatalf("expected metadata.headers.X-Remove to be deleted")
	}
	if _, ok := headersMeta["X-Nope"]; ok {
		t.Fatalf("expected metadata.headers.X-Nope to be absent")
	}

	if got := updated.Attributes["header:X-Old"]; got != "new" {
		t.Fatalf("attrs header:X-Old = %q, want %q", got, "new")
	}
	if got := updated.Attributes["header:X-New"]; got != "v" {
		t.Fatalf("attrs header:X-New = %q, want %q", got, "v")
	}
	if _, ok := updated.Attributes["header:X-Remove"]; ok {
		t.Fatalf("expected attrs header:X-Remove to be deleted")
	}
	if _, ok := updated.Attributes["header:X-Nope"]; ok {
		t.Fatalf("expected attrs header:X-Nope to be absent")
	}
}

func TestPatchAuthFileFields_HeadersEmptyMapIsNoop(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:       "noop.json",
		FileName: "noop.json",
		Provider: "claude",
		Attributes: map[string]string{
			"path":         "/tmp/noop.json",
			"header:X-Kee": "1",
		},
		Metadata: map[string]any{
			"type": "claude",
			"headers": map[string]any{
				"X-Kee": "1",
			},
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	body := `{"name":"noop.json","note":"hello","headers":{}}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/fields", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.PatchAuthFileFields(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	updated, ok := manager.GetByID("noop.json")
	if !ok || updated == nil {
		t.Fatalf("expected auth record to exist after patch")
	}
	if got := updated.Attributes["header:X-Kee"]; got != "1" {
		t.Fatalf("attrs header:X-Kee = %q, want %q", got, "1")
	}
	headersMeta, ok := updated.Metadata["headers"].(map[string]any)
	if !ok {
		t.Fatalf("expected metadata.headers to remain a map, got %T", updated.Metadata["headers"])
	}
	if got := headersMeta["X-Kee"]; got != "1" {
		t.Fatalf("metadata.headers.X-Kee = %#v, want %q", got, "1")
	}
}

func TestPatchAuthFileFields_WebsocketsFalseIsUpdate(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:       "codex.json",
		FileName: "codex.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path":       "/tmp/codex.json",
			"websockets": "true",
		},
		Metadata: map[string]any{
			"type":       "codex",
			"websockets": true,
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	body := `{"name":"codex.json","websockets":false}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/fields", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.PatchAuthFileFields(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	updated, ok := manager.GetByID("codex.json")
	if !ok || updated == nil {
		t.Fatalf("expected auth record to exist after patch")
	}
	if got := updated.Attributes["websockets"]; got != "false" {
		t.Fatalf("attrs websockets = %q, want %q", got, "false")
	}
	if got, ok := updated.Metadata["websockets"].(bool); !ok || got {
		t.Fatalf("metadata.websockets = %#v, want false", updated.Metadata["websockets"])
	}
}

func TestPatchAuthFileFields_ArbitraryFieldsPersistToFile(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	fileName := "generic.json"
	filePath := filepath.Join(authDir, fileName)
	store := fileauth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:       fileName,
		FileName: fileName,
		Provider: "codex",
		Attributes: map[string]string{
			"path": filePath,
		},
		Metadata: map[string]any{
			"type": "codex",
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	body := `{"name":"generic.json","abc":true,"nested.cde":true,"fgh":{"ijk":true}}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/fields", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.PatchAuthFileFields(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	raw, errRead := os.ReadFile(filePath)
	if errRead != nil {
		t.Fatalf("failed to read updated auth file: %v", errRead)
	}
	var data map[string]any
	if errUnmarshal := json.Unmarshal(raw, &data); errUnmarshal != nil {
		t.Fatalf("failed to unmarshal updated auth file: %v", errUnmarshal)
	}
	if got := data["abc"]; got != true {
		t.Fatalf("abc = %#v, want true", got)
	}
	nested, ok := data["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested = %#v, want object", data["nested"])
	}
	if got := nested["cde"]; got != true {
		t.Fatalf("nested.cde = %#v, want true", got)
	}
	fgh, ok := data["fgh"].(map[string]any)
	if !ok {
		t.Fatalf("fgh = %#v, want object", data["fgh"])
	}
	if got := fgh["ijk"]; got != true {
		t.Fatalf("fgh.ijk = %#v, want true", got)
	}
}

func TestPatchAuthFileFieldsBatch_UpdatesValidItemsAndReportsFailures(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	for _, record := range []*coreauth.Auth{
		{
			ID:       "first.json",
			FileName: "first.json",
			Provider: "claude",
			Metadata: map[string]any{"type": "claude"},
		},
		{
			ID:       "second.json",
			FileName: "second.json",
			Provider: "codex",
			Metadata: map[string]any{"type": "codex", "websockets": true},
		},
	} {
		if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
			t.Fatalf("failed to register auth record: %v", errRegister)
		}
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	body := `{"updates":[{"name":"first.json","fields":{"note":"updated"}},{"name":"missing.json","fields":{"note":"ignored"}},{"name":"second.json","fields":{"websockets":false}}]}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/fields/batch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.PatchAuthFileFieldsBatch(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var response struct {
		Updated int `json:"updated"`
		Failed  []struct {
			Name  string `json:"name"`
			Error string `json:"error"`
		} `json:"failed"`
	}
	if errDecode := json.NewDecoder(rec.Body).Decode(&response); errDecode != nil {
		t.Fatalf("failed to decode response: %v", errDecode)
	}
	if response.Updated != 2 {
		t.Fatalf("updated = %d, want 2", response.Updated)
	}
	if len(response.Failed) != 1 {
		t.Fatalf("failed = %#v, want one item", response.Failed)
	}
	if response.Failed[0].Name != "missing.json" || response.Failed[0].Error != "auth file not found" {
		t.Fatalf("failed item = %#v", response.Failed[0])
	}

	first, ok := manager.GetByID("first.json")
	if !ok || first == nil {
		t.Fatal("first auth record not found")
	}
	if got := first.Metadata["note"]; got != "updated" {
		t.Fatalf("first note = %#v, want updated", got)
	}

	second, ok := manager.GetByID("second.json")
	if !ok || second == nil {
		t.Fatal("second auth record not found")
	}
	if got := second.Metadata["websockets"]; got != false {
		t.Fatalf("second websockets = %#v, want false", got)
	}
}

func TestPatchAuthFileFieldsBatch_PriorityZeroIsImmediatelyListed(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	for _, record := range []*coreauth.Auth{
		{
			ID:         "selected.json",
			FileName:   "selected.json",
			Provider:   "codex",
			Attributes: map[string]string{"path": "/tmp/selected.json", "priority": "1"},
			Metadata:   map[string]any{"type": "codex", "priority": float64(1)},
		},
		{
			ID:         "untouched.json",
			FileName:   "untouched.json",
			Provider:   "codex",
			Attributes: map[string]string{"path": "/tmp/untouched.json", "priority": "7"},
			Metadata:   map[string]any{"type": "codex", "priority": float64(7)},
		},
	} {
		if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
			t.Fatalf("failed to register auth record: %v", errRegister)
		}
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	patchRecorder := httptest.NewRecorder()
	patchContext, _ := gin.CreateTestContext(patchRecorder)
	patchRequest := httptest.NewRequest(
		http.MethodPatch,
		"/v0/management/auth-files/fields/batch",
		strings.NewReader(`{"updates":[{"name":"selected.json","fields":{"priority":0}}]}`),
	)
	patchRequest.Header.Set("Content-Type", "application/json")
	patchContext.Request = patchRequest
	h.PatchAuthFileFieldsBatch(patchContext)

	if patchRecorder.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, patchRecorder.Code, patchRecorder.Body.String())
	}

	listRecorder := httptest.NewRecorder()
	listContext, _ := gin.CreateTestContext(listRecorder)
	listContext.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	h.ListAuthFiles(listContext)

	if listRecorder.Code != http.StatusOK {
		t.Fatalf("expected list status %d, got %d with body %s", http.StatusOK, listRecorder.Code, listRecorder.Body.String())
	}
	var response struct {
		Files []map[string]any `json:"files"`
	}
	if errDecode := json.NewDecoder(listRecorder.Body).Decode(&response); errDecode != nil {
		t.Fatalf("failed to decode list response: %v", errDecode)
	}

	priorities := make(map[string]any, len(response.Files))
	for _, file := range response.Files {
		name, _ := file["name"].(string)
		priorities[name] = file["priority"]
	}
	if got := priorities["selected.json"]; got != float64(0) {
		t.Fatalf("selected priority = %#v, want 0", got)
	}
	if got := priorities["untouched.json"]; got != float64(7) {
		t.Fatalf("untouched priority = %#v, want 7", got)
	}
}

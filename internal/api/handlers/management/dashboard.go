package management

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type dashboardProviderSummary struct {
	Gemini int `json:"gemini"`
	Codex  int `json:"codex"`
	Claude int `json:"claude"`
	OpenAI int `json:"openai"`
}

type dashboardSummaryResponse struct {
	APIKeys   int                      `json:"api_keys"`
	AuthFiles int                      `json:"auth_files"`
	Models    int                      `json:"models"`
	Providers dashboardProviderSummary `json:"providers"`
}

// GetDashboardSummary returns the management dashboard counts without exposing
// credential contents or serializing full resource lists.
func (h *Handler) GetDashboardSummary(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}

	h.mu.Lock()
	if h.cfg == nil {
		h.mu.Unlock()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "config not initialized"})
		return
	}
	response := dashboardSummaryResponse{
		APIKeys:   len(h.cfg.APIKeys),
		Providers: dashboardProviderCounts(h.cfg),
	}
	authManager := h.authManager
	authDir := h.cfg.AuthDir
	h.mu.Unlock()

	authFileCount, errCount := dashboardAuthFileCount(authManager, authDir)
	if errCount != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errCount.Error()})
		return
	}
	response.AuthFiles = authFileCount
	response.Models = len(registry.GetGlobalRegistry().GetAvailableModels("openai"))

	c.JSON(http.StatusOK, response)
}

func dashboardProviderCounts(cfg *config.Config) dashboardProviderSummary {
	var counts dashboardProviderSummary
	for _, item := range cfg.GeminiKey {
		if strings.TrimSpace(item.APIKey) != "" {
			counts.Gemini++
		}
	}
	for _, item := range cfg.CodexKey {
		if strings.TrimSpace(item.APIKey) != "" {
			counts.Codex++
		}
	}
	for _, item := range cfg.ClaudeKey {
		if strings.TrimSpace(item.APIKey) != "" {
			counts.Claude++
		}
	}
	for _, item := range cfg.OpenAICompatibility {
		if strings.TrimSpace(item.Name) != "" && strings.TrimSpace(item.BaseURL) != "" {
			counts.OpenAI++
		}
	}
	return counts
}

func dashboardAuthFileCount(manager *coreauth.Manager, authDir string) (int, error) {
	if manager != nil {
		count := 0
		for _, auth := range manager.List() {
			if authFileListVisible(auth) {
				count++
			}
		}
		return count, nil
	}

	entries, errRead := os.ReadDir(authDir)
	if errRead != nil {
		return 0, fmt.Errorf("failed to read auth dir: %w", errRead)
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		if _, errInfo := entry.Info(); errInfo == nil {
			count++
		}
	}
	return count, nil
}

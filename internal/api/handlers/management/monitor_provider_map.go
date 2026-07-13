package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// authTypeToProviderDisplay mirrors the frontend MonitorPage loadProviderMap OAuth map.
var authTypeToProviderDisplay = map[string]string{
	"claude":      "Claude",
	"gemini":      "Gemini",
	"gemini-cli":  "Gemini",
	"codex":       "Codex",
	"vertex":      "Vertex",
	"aistudio":    "AI Studio",
	"qwen":        "Qwen",
	"antigravity": "Antigravity",
	"iflow":       "iFlow",
}

// GetMonitorProviderMap returns source→display-name and source→models maps for the monitor UI.
// Built server-side from in-memory config + auth list so the management center no longer
// storms auth-files / *-api-key endpoints on first paint.
func (h *Handler) GetMonitorProviderMap(c *gin.Context) {
	providers, models := buildMonitorProviderMap(h.cfg, h.listAuthsForProviderMap())
	c.JSON(http.StatusOK, gin.H{
		"providers": providers,
		"models":    models,
	})
}

func (h *Handler) listAuthsForProviderMap() []*coreauth.Auth {
	if h == nil || h.authManager == nil {
		return nil
	}
	return h.authManager.List()
}

func buildMonitorProviderMap(cfg *config.Config, auths []*coreauth.Auth) (map[string]string, map[string][]string) {
	providers := make(map[string]string)
	models := make(map[string][]string)
	if cfg == nil && len(auths) == 0 {
		return providers, models
	}

	putModels := func(key string, names []string) {
		if key == "" || len(names) == 0 {
			return
		}
		// Preserve first write if already set (OpenAI name + api-key share same set).
		if existing, ok := models[key]; ok && len(existing) > 0 {
			return
		}
		models[key] = names
	}
	putProvider := func(key, name string) {
		key = strings.TrimSpace(key)
		if key == "" {
			return
		}
		if name == "" {
			name = "unknown"
		}
		providers[key] = name
	}

	if cfg != nil {
		// OpenAI-compatible providers
		for i := range cfg.OpenAICompatibility {
			p := cfg.OpenAICompatibility[i]
			providerName := strings.TrimSpace(p.Name)
			if p.Headers != nil {
				if v := strings.TrimSpace(headerValueCI(p.Headers, "X-Provider")); v != "" {
					providerName = v
				}
			}
			if providerName == "" {
				providerName = "unknown"
			}
			modelNames := collectModelNames(len(p.Models), func(j int) (string, string) {
				return p.Models[j].Name, p.Models[j].Alias
			})
			for _, entry := range p.APIKeyEntries {
				apiKey := strings.TrimSpace(entry.APIKey)
				if apiKey == "" {
					continue
				}
				putProvider(apiKey, providerName)
				putModels(apiKey, modelNames)
			}
			if name := strings.TrimSpace(p.Name); name != "" {
				putProvider(name, providerName)
				putModels(name, modelNames)
			}
		}

		// Gemini API keys (display only; frontend never stored models for these)
		for i := range cfg.GeminiKey {
			k := cfg.GeminiKey[i]
			apiKey := strings.TrimSpace(k.APIKey)
			if apiKey == "" {
				continue
			}
			name := strings.TrimSpace(k.Prefix)
			if name == "" {
				name = "Gemini"
			}
			putProvider(apiKey, name)
		}

		// Claude
		for i := range cfg.ClaudeKey {
			k := cfg.ClaudeKey[i]
			apiKey := strings.TrimSpace(k.APIKey)
			if apiKey == "" {
				continue
			}
			name := strings.TrimSpace(k.Prefix)
			if name == "" {
				name = "Claude"
			}
			putProvider(apiKey, name)
			putModels(apiKey, collectModelNames(len(k.Models), func(j int) (string, string) {
				return k.Models[j].Name, k.Models[j].Alias
			}))
		}

		// Codex
		for i := range cfg.CodexKey {
			k := cfg.CodexKey[i]
			apiKey := strings.TrimSpace(k.APIKey)
			if apiKey == "" {
				continue
			}
			name := strings.TrimSpace(k.Prefix)
			if name == "" {
				name = "Codex"
			}
			putProvider(apiKey, name)
			putModels(apiKey, collectModelNames(len(k.Models), func(j int) (string, string) {
				return k.Models[j].Name, k.Models[j].Alias
			}))
		}

		// Vertex-compatible
		for i := range cfg.VertexCompatAPIKey {
			k := cfg.VertexCompatAPIKey[i]
			apiKey := strings.TrimSpace(k.APIKey)
			if apiKey == "" {
				continue
			}
			name := strings.TrimSpace(k.Prefix)
			if name == "" {
				name = "Vertex"
			}
			putProvider(apiKey, name)
			putModels(apiKey, collectModelNames(len(k.Models), func(j int) (string, string) {
				return k.Models[j].Name, k.Models[j].Alias
			}))
		}
	}

	// OAuth / auth files (visible entries only — same visibility as ListAuthFiles)
	for _, auth := range auths {
		if auth == nil || !authFileListVisible(auth) {
			continue
		}
		name := strings.TrimSpace(auth.FileName)
		if name == "" {
			name = strings.TrimSpace(auth.ID)
		}
		if name == "" {
			continue
		}
		fileType := strings.TrimSpace(auth.Provider)
		if fileType == "" {
			fileType = "unknown"
		}
		display, ok := authTypeToProviderDisplay[fileType]
		if !ok {
			display = fileType
		}
		putProvider(name, display)
	}

	return providers, models
}

func headerValueCI(headers map[string]string, key string) string {
	if headers == nil {
		return ""
	}
	if v, ok := headers[key]; ok {
		return v
	}
	lower := strings.ToLower(key)
	for k, v := range headers {
		if strings.ToLower(k) == lower {
			return v
		}
	}
	return ""
}

func collectModelNames(n int, get func(i int) (name, alias string)) []string {
	if n <= 0 {
		return nil
	}
	seen := make(map[string]struct{}, n*2)
	out := make([]string, 0, n)
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for i := 0; i < n; i++ {
		name, alias := get(i)
		add(alias)
		add(name)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

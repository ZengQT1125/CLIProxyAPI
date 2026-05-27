package codexapi

import "github.com/router-for-me/CLIProxyAPI/v7/internal/util"

const (
	BaseURLEnv     = "CODEX_BASE_URL"
	DefaultBaseURL = "https://chatgpt.com/backend-api/codex"
)

func BaseURLFromEnv() string {
	if baseURL := util.GetEnvTrimmed(BaseURLEnv, "codex_base_url"); baseURL != "" {
		return baseURL
	}
	return DefaultBaseURL
}

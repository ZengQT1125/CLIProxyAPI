package executor

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/codexapi"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func resolveCodexBaseURL(auth *cliproxyauth.Auth) string {
	if auth != nil && auth.Attributes != nil {
		if baseURL := strings.TrimSpace(auth.Attributes["base_url"]); baseURL != "" {
			return baseURL
		}
	}
	return codexapi.BaseURLFromEnv()
}

func resolveCodexBaseURLValue(baseURL string, auth *cliproxyauth.Auth) string {
	if baseURL = strings.TrimSpace(baseURL); baseURL != "" {
		return baseURL
	}
	return resolveCodexBaseURL(auth)
}

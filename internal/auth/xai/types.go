// Package xai provides OAuth2 authentication helpers for xAI Grok.
package xai

import (
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
)

const (
	// DefaultAPIBaseURL is the default official xAI API base URL.
	// Used for OAuth credential defaults, websocket, media (image/video),
	// and non-media HTTP chat when auth using_api is true or non-OAuth.
	DefaultAPIBaseURL = "https://api.x.ai/v1"
	// CLIChatProxyBaseURL is the Grok CLI chat-proxy base URL for non-image/video
	// HTTP chat when auth using_api is false, including the OAuth default.
	CLIChatProxyBaseURL = "https://cli-chat-proxy.grok.com/v1"
	// CLIChatProxyBaseURLEnv overrides CLIChatProxyBaseURL.
	CLIChatProxyBaseURLEnv = "XAI_CLI_CHAT_PROXY_BASE_URL"
	// CLIBillingPath is the Grok CLI billing endpoint used for quota checks.
	CLIBillingPath = "/billing"
	// CLITokenAuthHeader identifies the Grok CLI token-auth scheme.
	CLITokenAuthHeader = "X-XAI-Token-Auth"
	// CLITokenAuthValue is the required Grok CLI token-auth value.
	CLITokenAuthValue = "xai-grok-cli"
	// CLIClientVersionHeader carries the Grok CLI protocol version.
	CLIClientVersionHeader = "x-grok-client-version"
	// CLIClientVersion must stay aligned with the Grok CLI version expected by chat-proxy.
	CLIClientVersion = "0.2.93"
	// CLIUserAgent identifies CLIProxyAPI requests to Grok chat-proxy endpoints.
	CLIUserAgent = "xai-grok-workspace/" + CLIClientVersion
	// Issuer is xAI's OAuth issuer.
	Issuer = "https://auth.x.ai"
	// DiscoveryURL is the OIDC discovery endpoint used to resolve OAuth endpoints.
	DiscoveryURL = Issuer + "/.well-known/openid-configuration"
	// ClientID is the public xAI Grok CLI OAuth client ID.
	ClientID = "b1a00492-073a-47ea-816f-4c329264a828"
	// Scope is the OAuth scope set required for xAI API access.
	Scope = "openid profile email offline_access grok-cli:access api:access"
	// DeviceCodeGrantType is the OAuth2 device authorization grant type (RFC 8628).
	DeviceCodeGrantType = "urn:ietf:params:oauth:grant-type:device_code"
	// defaultPollInterval is used when the device endpoint omits interval.
	defaultPollInterval = 5 * time.Second
	// httpClientTimeout bounds credential-acquisition HTTP calls (device/token/refresh).
	httpClientTimeout = 30 * time.Second
	// MaxPollDuration is the upper bound for waiting on user authorization.
	MaxPollDuration = 30 * time.Minute
)

var refreshLead = 5 * time.Minute

// CLIChatProxyBaseURLFromEnv returns the configured Grok CLI chat-proxy base URL.
func CLIChatProxyBaseURLFromEnv() string {
	if baseURL := CLIChatProxyBaseURLOverride(); baseURL != "" {
		return baseURL
	}
	return CLIChatProxyBaseURL
}

// CLIChatProxyBaseURLOverride returns the explicit environment override, if any.
func CLIChatProxyBaseURLOverride() string {
	return util.GetEnvTrimmed(CLIChatProxyBaseURLEnv, "xai_cli_chat_proxy_base_url")
}

// RefreshLead returns the refresh lead time for xAI OAuth credentials.
func RefreshLead() time.Duration {
	return refreshLead
}

// Discovery contains OAuth endpoints resolved from xAI OIDC discovery.
type Discovery struct {
	DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
	TokenEndpoint               string `json:"token_endpoint"`
}

// DeviceCodeResponse represents xAI's device authorization response.
type DeviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
	TokenEndpoint           string `json:"-"`
}

// TokenData holds xAI OAuth token data.
type TokenData struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
	Expire       string `json:"expired,omitempty"`
	Email        string `json:"email,omitempty"`
	Subject      string `json:"sub,omitempty"`
}

// AuthBundle aggregates token data and OAuth metadata for persistence.
type AuthBundle struct {
	TokenData     TokenData
	LastRefresh   string
	BaseURL       string
	RedirectURI   string
	TokenEndpoint string
}

package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

func (e *ClaudeExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("claude executor: refresh called")
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	if auth == nil {
		return nil, fmt.Errorf("claude executor: auth is nil")
	}
	var refreshToken string
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["refresh_token"].(string); ok && v != "" {
			refreshToken = v
		}
	}
	if refreshToken == "" {
		return auth, nil
	}
	svc := claudeauth.NewClaudeAuthWithProxyURL(e.cfg, auth.ProxyURL)
	td, err := svc.RefreshTokensWithRetry(ctx, refreshToken, 3)
	if err != nil {
		return nil, err
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = td.AccessToken
	if td.RefreshToken != "" {
		auth.Metadata["refresh_token"] = td.RefreshToken
	}
	auth.Metadata["email"] = td.Email
	auth.Metadata["expired"] = td.Expire
	auth.Metadata["type"] = "claude"
	now := time.Now().Format(time.RFC3339)
	auth.Metadata["last_refresh"] = now
	return auth, nil
}

func (e *ClaudeExecutor) claudeStatusErr(ctx context.Context, httpClient *http.Client, apiKey, baseURL string, code int, headers http.Header, body []byte) statusErr {
	now := time.Now()
	errStatus := statusErr{
		code:       code,
		msg:        string(body),
		retryAfter: claudeRetryAfterFromHeader(headers, now),
	}
	if code != http.StatusTooManyRequests || errStatus.retryAfter != nil {
		return errStatus
	}
	if retryAfter := claudeRetryAfterFromJSON(body, now); retryAfter != nil {
		errStatus.retryAfter = retryAfter
		return errStatus
	}
	if !isClaudeOAuthToken(apiKey) {
		return errStatus
	}
	errStatus.retryAfter = e.claudeOAuthProfileRetryAfter(ctx, httpClient, apiKey, baseURL, now)
	return errStatus
}

func claudeRetryAfterFromHeader(headers http.Header, now time.Time) *time.Duration {
	if headers == nil {
		return nil
	}
	raw := strings.TrimSpace(headers.Get("Retry-After"))
	if raw != "" {
		if seconds, errParse := strconv.ParseFloat(raw, 64); errParse == nil {
			duration := time.Duration(seconds * float64(time.Second))
			if duration < 0 {
				duration = 0
			}
			return &duration
		}
		resetAt, errParse := http.ParseTime(raw)
		if errParse == nil {
			duration := resetAt.Sub(now)
			if duration < 0 {
				duration = 0
			}
			return &duration
		}
	}

	var best time.Time
	for key, values := range headers {
		if !claudeRateLimitResetHeader(key) {
			continue
		}
		for _, value := range values {
			resetAt, ok := claudeResetTimeFromRaw(key, value, now)
			if !ok || !resetAt.After(now) {
				continue
			}
			if best.IsZero() || resetAt.Before(best) {
				best = resetAt
			}
		}
	}
	if best.IsZero() {
		return nil
	}
	duration := best.Sub(now)
	return &duration
}

func claudeRateLimitResetHeader(header string) bool {
	key := strings.ToLower(strings.TrimSpace(header))
	return strings.Contains(key, "reset") &&
		(strings.Contains(key, "ratelimit") ||
			strings.Contains(key, "rate-limit") ||
			strings.Contains(key, "quota"))
}

func claudeRetryAfterFromJSON(body []byte, now time.Time) *time.Duration {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return nil
	}
	resetAt, ok := claudeResetTimeFromJSON(gjson.ParseBytes(body), "", now)
	if !ok {
		return nil
	}
	duration := resetAt.Sub(now)
	if duration <= 0 {
		return nil
	}
	return &duration
}

func claudeResetTimeFromJSON(value gjson.Result, path string, now time.Time) (time.Time, bool) {
	if value.IsObject() {
		var best time.Time
		value.ForEach(func(key, child gjson.Result) bool {
			childPath := key.String()
			if path != "" {
				childPath = path + "." + childPath
			}
			if candidate, ok := claudeResetTimeFromJSON(child, childPath, now); ok && (best.IsZero() || candidate.Before(best)) {
				best = candidate
			}
			return true
		})
		return best, !best.IsZero()
	}
	if value.IsArray() {
		var best time.Time
		value.ForEach(func(_, child gjson.Result) bool {
			if candidate, ok := claudeResetTimeFromJSON(child, path, now); ok && (best.IsZero() || candidate.Before(best)) {
				best = candidate
			}
			return true
		})
		return best, !best.IsZero()
	}
	if !claudeQuotaResetKey(path) {
		return time.Time{}, false
	}
	resetAt, ok := claudeResetTimeFromValue(path, value, now)
	if !ok || !resetAt.After(now) {
		return time.Time{}, false
	}
	return resetAt, true
}

func claudeQuotaResetKey(path string) bool {
	key := strings.ToLower(strings.TrimSpace(path))
	return strings.Contains(key, "reset") ||
		strings.Contains(key, "retry_after") ||
		strings.Contains(key, "retry-after") ||
		strings.Contains(key, "cooldown") ||
		strings.Contains(key, "available_at") ||
		strings.Contains(key, "available_after")
}

func claudeResetTimeFromValue(path string, value gjson.Result, now time.Time) (time.Time, bool) {
	switch value.Type {
	case gjson.Number:
		return claudeResetTimeFromNumber(path, value.Float(), now)
	case gjson.String:
		return claudeResetTimeFromRaw(path, value.String(), now)
	}
	return time.Time{}, false
}

func claudeResetTimeFromRaw(path string, raw string, now time.Time) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if parsed, errParse := time.Parse(time.RFC3339Nano, raw); errParse == nil {
		return parsed, true
	}
	if parsed, errParse := http.ParseTime(raw); errParse == nil {
		return parsed, true
	}
	if number, errParse := strconv.ParseFloat(raw, 64); errParse == nil {
		return claudeResetTimeFromNumber(path, number, now)
	}
	return time.Time{}, false
}

func claudeResetTimeFromNumber(path string, number float64, now time.Time) (time.Time, bool) {
	if number <= 0 {
		return time.Time{}, false
	}
	if number > 1e12 {
		return time.UnixMilli(int64(number)), true
	}
	if number > 1e9 {
		return time.Unix(int64(number), 0), true
	}
	key := strings.ToLower(path)
	duration := time.Duration(number * float64(time.Second))
	if strings.Contains(key, "millisecond") || strings.Contains(key, "_ms") || strings.HasSuffix(key, "ms") {
		duration = time.Duration(number * float64(time.Millisecond))
	}
	if strings.Contains(key, "second") ||
		strings.Contains(key, "millisecond") ||
		strings.Contains(key, "_ms") ||
		strings.HasSuffix(key, "ms") ||
		strings.Contains(key, "after") ||
		strings.Contains(key, "duration") ||
		strings.Contains(key, "cooldown") {
		return now.Add(duration), true
	}
	return time.Time{}, false
}

func (e *ClaudeExecutor) claudeOAuthProfileRetryAfter(ctx context.Context, httpClient *http.Client, apiKey, baseURL string, now time.Time) *time.Duration {
	if httpClient == nil || strings.TrimSpace(apiKey) == "" {
		return nil
	}
	profileURL := claudeOAuthProfileURL(baseURL)
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, profileURL, nil)
	if errReq != nil {
		helps.LogWithRequestID(ctx).WithError(errReq).Debug("claude executor: failed to build oauth profile request after rate limit")
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")

	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		helps.LogWithRequestID(ctx).WithError(errDo).Debug("claude executor: failed to query oauth profile after rate limit")
		return nil
	}
	if resp == nil {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.Body != nil {
			if errClose := resp.Body.Close(); errClose != nil {
				log.Errorf("response body close error: %v", errClose)
			}
		}
		helps.LogWithRequestID(ctx).Debugf("claude executor: oauth profile returned status %d after rate limit", resp.StatusCode)
		return nil
	}
	if resp.Body == nil {
		return nil
	}
	decodedBody, errDecode := decodeResponseBody(resp.Body, resp.Header.Get("Content-Encoding"))
	if errDecode != nil {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		helps.LogWithRequestID(ctx).WithError(errDecode).Debug("claude executor: failed to decode oauth profile after rate limit")
		return nil
	}
	defer func() {
		if errClose := decodedBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()
	body, errRead := io.ReadAll(decodedBody)
	if errRead != nil {
		helps.LogWithRequestID(ctx).WithError(errRead).Debug("claude executor: failed to read oauth profile after rate limit")
		return nil
	}
	return claudeRetryAfterFromJSON(body, now)
}

func claudeOAuthProfileURL(baseURL string) string {
	rawBase := strings.TrimSpace(baseURL)
	if rawBase == "" {
		rawBase = "https://api.anthropic.com"
	}
	parsed, errParse := url.Parse(rawBase)
	if errParse != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "https://api.anthropic.com" + claudeOAuthProfilePath
	}
	parsed.Path = claudeOAuthProfilePath
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

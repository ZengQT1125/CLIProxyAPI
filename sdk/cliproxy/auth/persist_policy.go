package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
)

type skipPersistContextKey struct{}
type deferAPIKeyModelAliasRebuildContextKey struct{}
type authMaterialReplacementContextKey struct{}

// WithSkipPersist returns a derived context that disables persistence for Manager Update/Register calls.
// It is intended for code paths that are reacting to file watcher events, where the file on disk is
// already the source of truth and persisting again would create a write-back loop.
func WithSkipPersist(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, skipPersistContextKey{}, true)
}

func shouldSkipPersist(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v := ctx.Value(skipPersistContextKey{})
	enabled, ok := v.(bool)
	return ok && enabled
}

// WithDeferredAPIKeyModelAliasRebuild returns a derived context that defers API-key model alias table rebuilds.
// Callers that use this for a batch of Register/Update/Remove operations must call RefreshAPIKeyModelAlias once.
func WithDeferredAPIKeyModelAliasRebuild(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, deferAPIKeyModelAliasRebuildContextKey{}, true)
}

func shouldDeferAPIKeyModelAliasRebuild(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v := ctx.Value(deferAPIKeyModelAliasRebuildContextKey{})
	enabled, ok := v.(bool)
	return ok && enabled
}

// WithAuthMaterialReplacement marks an Update call as potentially replacing persisted credential material.
// Manager compares the material atomically and only resets runtime state when it actually changed.
func WithAuthMaterialReplacement(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, authMaterialReplacementContextKey{}, true)
}

func shouldResetRuntimeStateOnUpdate(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v := ctx.Value(authMaterialReplacementContextKey{})
	enabled, ok := v.(bool)
	return ok && enabled
}

type persistedAuthMaterial struct {
	ID         string            `json:"id"`
	Provider   string            `json:"provider"`
	Prefix     string            `json:"prefix,omitempty"`
	ProxyURL   string            `json:"proxy_url,omitempty"`
	Disabled   bool              `json:"disabled"`
	Attributes map[string]string `json:"attributes,omitempty"`
	Metadata   map[string]any    `json:"metadata"`
	Storage    any               `json:"storage,omitempty"`
}

func samePersistedAuthMaterial(existing, incoming *Auth) bool {
	if existing == nil || incoming == nil {
		return existing == incoming
	}
	existingMaterial, errExistingMaterial := newPersistedAuthMaterial(existing)
	if errExistingMaterial != nil {
		return false
	}
	existingJSON, errExisting := json.Marshal(existingMaterial)
	if errExisting != nil {
		return false
	}
	incomingMaterial, errIncomingMaterial := newPersistedAuthMaterial(incoming)
	if errIncomingMaterial != nil {
		return false
	}
	incomingJSON, errIncoming := json.Marshal(incomingMaterial)
	if errIncoming != nil {
		return false
	}
	return bytes.Equal(existingJSON, incomingJSON)
}

func newPersistedAuthMaterial(auth *Auth) (persistedAuthMaterial, error) {
	metadata := make(map[string]any, len(auth.Metadata)+1)
	for key, value := range auth.Metadata {
		metadata[key] = value
	}
	// All persistent stores normalize this field before writing credential material.
	metadata["disabled"] = auth.Disabled

	var storage any
	if rawStorage, ok := auth.Storage.(interface{ RawJSON() []byte }); ok {
		rawJSON := bytes.TrimSpace(rawStorage.RawJSON())
		if len(rawJSON) > 0 {
			if errUnmarshal := json.Unmarshal(rawJSON, &storage); errUnmarshal != nil {
				return persistedAuthMaterial{}, errUnmarshal
			}
		}
	}

	return persistedAuthMaterial{
		ID:         auth.ID,
		Provider:   strings.ToLower(strings.TrimSpace(auth.Provider)),
		Prefix:     auth.Prefix,
		ProxyURL:   auth.ProxyURL,
		Disabled:   auth.Disabled,
		Attributes: auth.Attributes,
		Metadata:   metadata,
		Storage:    storage,
	}, nil
}

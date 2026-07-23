package cliproxy

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestNormalizedRoutingRuntimeStateRecognizesSequentialFill(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		strategy string
		want     string
	}{
		{name: "empty defaults to round-robin", strategy: "", want: coreauth.RoutingStrategyRoundRobin},
		{name: "round-robin alias", strategy: "rr", want: coreauth.RoutingStrategyRoundRobin},
		{name: "fill-first alias", strategy: "ff", want: coreauth.RoutingStrategyFillFirst},
		{name: "sequential-fill canonical", strategy: "sequential-fill", want: coreauth.RoutingStrategySequentialFill},
		{name: "sequential-fill alias sf", strategy: "sf", want: coreauth.RoutingStrategySequentialFill},
		{name: "sequential-fill alias sequentialfill", strategy: "sequentialfill", want: coreauth.RoutingStrategySequentialFill},
		{name: "unknown falls back to round-robin", strategy: "bogus", want: coreauth.RoutingStrategyRoundRobin},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := normalizedRoutingRuntimeState(&config.Config{
				Routing: internalconfig.RoutingConfig{Strategy: tt.strategy},
			})
			if state.strategy != tt.want {
				t.Fatalf("strategy = %q, want %q", state.strategy, tt.want)
			}
		})
	}
}

func TestNewRoutingSelectorUsesSequentialFill(t *testing.T) {
	t.Parallel()

	selector := newRoutingSelector(routingRuntimeState{strategy: coreauth.RoutingStrategySequentialFill})
	if _, ok := selector.(*coreauth.SequentialFillSelector); !ok {
		t.Fatalf("selector = %T, want *SequentialFillSelector", selector)
	}

	// Alias must not silently degrade to round-robin — that was the production bug.
	aliased := newRoutingSelector(routingRuntimeState{strategy: "sf"})
	if _, ok := aliased.(*coreauth.SequentialFillSelector); !ok {
		t.Fatalf("selector for raw alias sf = %T, want *SequentialFillSelector", aliased)
	}
}

func registerRoutingTestModels(t *testing.T, provider, model string, authIDs ...string) {
	t.Helper()
	reg := registry.GetGlobalRegistry()
	for _, id := range authIDs {
		reg.RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
	}
	t.Cleanup(func() {
		for _, id := range authIDs {
			reg.UnregisterClient(id)
		}
	})
}

// Config alias "sf" must wire into Manager and keep sticky picks across successful
// requests (the user-visible SF contract). SelectAuth exercises the same selector
// pick path used when the scheduler fast path is not taken.
func TestConfigSFWiresStickySchedulerPicks(t *testing.T) {
	t.Parallel()

	const model = "grok-4.5"
	authIDs := []string{"xai-a", "xai-b", "xai-c"}
	registerRoutingTestModels(t, "xai", model, authIDs...)

	state := normalizedRoutingRuntimeState(&config.Config{
		Routing: internalconfig.RoutingConfig{Strategy: "sf"},
	})
	if state.strategy != coreauth.RoutingStrategySequentialFill {
		t.Fatalf("normalized strategy = %q, want %q", state.strategy, coreauth.RoutingStrategySequentialFill)
	}

	selector := newRoutingSelector(state)
	if _, ok := selector.(*coreauth.SequentialFillSelector); !ok {
		t.Fatalf("selector = %T, want *SequentialFillSelector", selector)
	}

	manager := coreauth.NewManager(nil, selector, nil)
	manager.RegisterExecutor(progressiveBatchTestExecutor{})
	ctx := context.Background()
	for _, id := range authIDs {
		if _, errRegister := manager.Register(coreauth.WithSkipPersist(ctx), &coreauth.Auth{
			ID:       id,
			Provider: "xai",
			Status:   coreauth.StatusActive,
		}); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", id, errRegister)
		}
	}

	first, errSelect := manager.SelectAuth(ctx, "xai", model, cliproxyexecutor.Options{})
	if errSelect != nil {
		t.Fatalf("first SelectAuth error = %v", errSelect)
	}
	if first == nil || first.ID == "" {
		t.Fatalf("first SelectAuth returned empty auth")
	}

	for i := 0; i < 8; i++ {
		got, errPick := manager.SelectAuth(ctx, "xai", model, cliproxyexecutor.Options{})
		if errPick != nil {
			t.Fatalf("SelectAuth #%d error = %v", i+2, errPick)
		}
		if got == nil || got.ID != first.ID {
			t.Fatalf("SelectAuth #%d auth = %v, want sticky %q (RR would rotate)", i+2, got, first.ID)
		}
	}
}

func TestConfigSFWithSessionAffinityKeepsSequentialFallback(t *testing.T) {
	t.Parallel()

	const model = "grok-4.5"
	authIDs := []string{"auth-1", "auth-2", "auth-3"}
	registerRoutingTestModels(t, "xai", model, authIDs...)

	state := normalizedRoutingRuntimeState(&config.Config{
		Routing: internalconfig.RoutingConfig{
			Strategy:           "sf",
			SessionAffinity:    true,
			SessionAffinityTTL: "1h",
		},
	})
	if state.strategy != coreauth.RoutingStrategySequentialFill {
		t.Fatalf("strategy = %q, want sequential-fill", state.strategy)
	}

	selector := newRoutingSelector(state)
	affinity, ok := selector.(*coreauth.SessionAffinitySelector)
	if !ok {
		t.Fatalf("selector = %T, want *SessionAffinitySelector wrapping SF", selector)
	}
	t.Cleanup(affinity.Stop)

	// Session affinity is a wrapper; without a session id it must fall back to SF sticky,
	// not silently become round-robin underneath.
	manager := coreauth.NewManager(nil, selector, nil)
	manager.RegisterExecutor(progressiveBatchTestExecutor{})
	ctx := context.Background()
	for _, id := range authIDs {
		if _, errRegister := manager.Register(coreauth.WithSkipPersist(ctx), &coreauth.Auth{
			ID:       id,
			Provider: "xai",
			Status:   coreauth.StatusActive,
		}); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", id, errRegister)
		}
	}

	first, errSelect := manager.SelectAuth(ctx, "xai", model, cliproxyexecutor.Options{})
	if errSelect != nil {
		t.Fatalf("first SelectAuth error = %v", errSelect)
	}
	if first == nil {
		t.Fatal("first SelectAuth returned nil")
	}
	for i := 0; i < 5; i++ {
		got, errPick := manager.SelectAuth(ctx, "xai", model, cliproxyexecutor.Options{})
		if errPick != nil {
			t.Fatalf("SelectAuth #%d error = %v", i+2, errPick)
		}
		if got == nil || got.ID != first.ID {
			t.Fatalf("SelectAuth #%d auth = %v, want sticky fallback %q", i+2, got, first.ID)
		}
	}
}

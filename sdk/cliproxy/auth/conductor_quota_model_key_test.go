package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// Quota state must be keyed by the canonical model name so that the scheduler
// fast path (which shards by canonicalModelKey) sees the cooldown even when
// the route model carries a thinking suffix like "gpt-5.3-codex(high)".
func TestMarkResult_SuffixedModelQuotaBlocksFastPathPick(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	model := "gpt-5.3-codex"
	routeModel := "gpt-5.3-codex(high)"
	registerSchedulerModels(t, "codex", model, "codex-suffix-1", "codex-suffix-2")

	m := NewManager(nil, &RoundRobinSelector{}, nil)
	m.executors["codex"] = schedulerTestExecutor{}
	for _, id := range []string{"codex-suffix-1", "codex-suffix-2"} {
		if _, err := m.Register(context.Background(), &Auth{ID: id, Provider: "codex"}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}

	retryAfter := 3 * time.Hour
	m.MarkResult(context.Background(), Result{
		AuthID:     "codex-suffix-1",
		Provider:   "codex",
		Model:      routeModel,
		Success:    false,
		Error:      &Error{Code: "quota", Message: "usage limit reached", HTTPStatus: http.StatusTooManyRequests},
		RetryAfter: &retryAfter,
	})

	for i := range 10 {
		auth, _, err := m.pickNext(context.Background(), "codex", routeModel, cliproxyexecutor.Options{}, map[string]struct{}{})
		if err != nil {
			t.Fatalf("pickNext #%d: %v", i, err)
		}
		if auth.ID == "codex-suffix-1" {
			t.Fatalf("pickNext #%d returned quota-exhausted auth codex-suffix-1", i)
		}
	}
}

// A success on a suffixed route model must clear the canonical cooldown state.
func TestMarkResult_SuffixedModelSuccessClearsCanonicalQuota(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	model := "gpt-5.3-codex"
	routeModel := "gpt-5.3-codex(high)"
	registerSchedulerModels(t, "codex", model, "codex-clear-1")

	m := NewManager(nil, &RoundRobinSelector{}, nil)
	m.executors["codex"] = schedulerTestExecutor{}
	if _, err := m.Register(context.Background(), &Auth{ID: "codex-clear-1", Provider: "codex"}); err != nil {
		t.Fatalf("register: %v", err)
	}

	retryAfter := 3 * time.Hour
	m.MarkResult(context.Background(), Result{
		AuthID:     "codex-clear-1",
		Provider:   "codex",
		Model:      routeModel,
		Success:    false,
		Error:      &Error{Code: "quota", Message: "usage limit reached", HTTPStatus: http.StatusTooManyRequests},
		RetryAfter: &retryAfter,
	})
	m.MarkResult(context.Background(), Result{
		AuthID:   "codex-clear-1",
		Provider: "codex",
		Model:    routeModel,
		Success:  true,
	})

	updated, ok := m.GetByID("codex-clear-1")
	if !ok || updated == nil {
		t.Fatalf("auth missing")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected canonical model state %q to exist, states: %v", model, updated.ModelStates)
	}
	if state.Quota.Exceeded {
		t.Fatalf("expected quota cleared after success, got %+v", state.Quota)
	}
	if _, exists := updated.ModelStates[routeModel]; exists {
		t.Fatalf("model state must be keyed canonically, found suffixed key %q", routeModel)
	}
}

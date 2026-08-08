package auth

import (
	"context"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func TestManagerSelectAuthBuiltInSelectorCooldownPreservesRouteModel(t *testing.T) {
	const provider = "gemini"
	const registeredModel = "client-opus"
	const routeModel = "client-opus(high)"

	selectors := map[string]Selector{
		"round-robin":          &RoundRobinSelector{},
		"weighted-round-robin": &WeightedRoundRobinSelector{},
		"fill-first":           &FillFirstSelector{},
	}
	for name, selector := range selectors {
		t.Run(name, func(t *testing.T) {
			authID := "cooling-auth-" + name
			next := time.Now().Add(time.Hour)
			registerSchedulerModels(t, provider, registeredModel, authID)

			manager := NewManager(nil, selector, nil)
			manager.RegisterExecutor(schedulerProviderTestExecutor{provider: provider})
			if _, errRegister := manager.Register(WithSkipPersist(context.Background()), &Auth{
				ID:             authID,
				Provider:       provider,
				Unavailable:    true,
				NextRetryAfter: next,
				Quota: QuotaState{
					Exceeded:      true,
					NextRecoverAt: next,
				},
				ModelStates: map[string]*ModelState{
					"other-model": {Status: StatusActive},
				},
			}); errRegister != nil {
				t.Fatalf("Register() error = %v", errRegister)
			}

			_, errPick := manager.SelectAuth(
				context.Background(),
				provider,
				routeModel,
				cliproxyexecutor.Options{},
			)
			if errPick == nil {
				t.Fatal("SelectAuth() error = nil, want model cooldown")
			}
			if got := gjson.Get(errPick.Error(), "error.code").String(); got != "model_cooldown" {
				t.Fatalf("SelectAuth() error code = %q, want model_cooldown; error=%v", got, errPick)
			}
			if got := gjson.Get(errPick.Error(), "error.model").String(); got != routeModel {
				t.Fatalf("SelectAuth() cooldown model = %q, want %q; error=%v", got, routeModel, errPick)
			}
		})
	}
}

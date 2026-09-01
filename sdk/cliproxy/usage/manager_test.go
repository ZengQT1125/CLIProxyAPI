package usage

import (
	"context"
	"testing"
)

type authUsageCleanerRecorder struct {
	authIndexes []string
}

func (r *authUsageCleanerRecorder) HandleUsage(context.Context, Record) {}

func (r *authUsageCleanerRecorder) DeleteAuthUsage(_ context.Context, authIndexes []string) error {
	r.authIndexes = append([]string(nil), authIndexes...)
	return nil
}

func TestStreamFromContextDefaultsMissingToFalse(t *testing.T) {
	if StreamFromContext(context.Background()) {
		t.Fatalf("StreamFromContext(background) = true, want false")
	}
}

func TestStreamFromContextHonorsExplicitTrue(t *testing.T) {
	ctx := WithStream(context.Background(), true)
	if !StreamFromContext(ctx) {
		t.Fatalf("StreamFromContext(true) = false, want true")
	}
}

func TestRecordStreamField(t *testing.T) {
	record := Record{
		Provider: "openai",
		Model:    "gpt-5.4",
		Stream:   true,
	}
	if !record.Stream {
		t.Fatalf("Record.Stream = false, want true")
	}
}

func TestGenerateEnabledDefaultsNilToTrue(t *testing.T) {
	if !GenerateEnabled(nil) {
		t.Fatalf("GenerateEnabled(nil) = false, want true")
	}
}

func TestGenerateEnabledHonorsExplicitFalse(t *testing.T) {
	if GenerateEnabled(GenerateFlag(false)) {
		t.Fatalf("GenerateEnabled(false) = true, want false")
	}
}

func TestGenerateEnabledHonorsExplicitTrue(t *testing.T) {
	if !GenerateEnabled(GenerateFlag(true)) {
		t.Fatalf("GenerateEnabled(true) = false, want true")
	}
}

func TestGenerateFromContextDefaultsMissingToTrue(t *testing.T) {
	if !GenerateFromContext(context.Background()) {
		t.Fatalf("GenerateFromContext(background) = false, want true")
	}
}

func TestGenerateFromContextHonorsExplicitFalse(t *testing.T) {
	ctx := WithGenerate(context.Background(), false)
	if GenerateFromContext(ctx) {
		t.Fatalf("GenerateFromContext(false) = true, want false")
	}
}

func TestRecordOmittedGenerateIsEnabled(t *testing.T) {
	// Existing callers construct Record without setting Generate.
	// Omission must remain distinguishable from explicit false and default to true.
	record := Record{
		Provider: "openai",
		Model:    "gpt-5.4",
	}
	if record.Generate != nil {
		t.Fatalf("Record.Generate = %v, want nil for omitted field", record.Generate)
	}
	if !GenerateEnabled(record.Generate) {
		t.Fatalf("GenerateEnabled(omitted) = false, want true")
	}
}

func TestIsFastMode(t *testing.T) {
	tests := []struct {
		name        string
		provider    string
		serviceTier string
		want        bool
	}{
		{name: "codex priority", provider: "codex", serviceTier: "priority", want: true},
		{name: "normalized codex priority", provider: " CODEX ", serviceTier: " PRIORITY ", want: true},
		{name: "codex standard", provider: "codex", serviceTier: "default", want: false},
		{name: "non-codex priority", provider: "openai", serviceTier: "priority", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsFastMode(tt.provider, tt.serviceTier); got != tt.want {
				t.Fatalf("IsFastMode(%q, %q) = %v, want %v", tt.provider, tt.serviceTier, got, tt.want)
			}
		})
	}
}

func TestManagerDeleteAuthUsageNotifiesCleaners(t *testing.T) {
	manager := NewManager(1)
	cleaner := &authUsageCleanerRecorder{}
	manager.Register(cleaner)

	if err := manager.DeleteAuthUsage(context.Background(), []string{"auth-index-a", "auth-index-b"}); err != nil {
		t.Fatalf("DeleteAuthUsage returned error: %v", err)
	}
	if len(cleaner.authIndexes) != 2 || cleaner.authIndexes[0] != "auth-index-a" || cleaner.authIndexes[1] != "auth-index-b" {
		t.Fatalf("cleaner auth indexes = %v, want [auth-index-a auth-index-b]", cleaner.authIndexes)
	}
}

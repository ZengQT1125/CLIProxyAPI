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

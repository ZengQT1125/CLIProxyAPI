package config

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestConfigLoadersWarnAndContinueOnTypeErrors(t *testing.T) {
	raw := []byte("port: 9001\n" +
		"oauth-excluded-models:\n" +
		"  codex:\n" +
		"    - valid-model\n" +
		"    - name: invalid-model-secret\n" +
		"auth-load-workers: worker-secret\n" +
		"debug: true\n")

	loaders := []struct {
		name string
		load func(*testing.T, []byte) (*Config, error)
	}{
		{
			name: "bytes",
			load: func(_ *testing.T, data []byte) (*Config, error) {
				return ParseConfigBytes(data)
			},
		},
		{
			name: "file",
			load: func(t *testing.T, data []byte) (*Config, error) {
				configPath := filepath.Join(t.TempDir(), "config.yaml")
				if errWrite := os.WriteFile(configPath, data, 0o600); errWrite != nil {
					t.Fatal(errWrite)
				}
				return LoadConfig(configPath)
			},
		},
	}

	for _, loader := range loaders {
		t.Run(loader.name, func(t *testing.T) {
			hook := captureConfigWarnings(t)
			cfg, errLoad := loader.load(t, raw)
			if errLoad != nil {
				t.Fatalf("load config error = %v", errLoad)
			}
			if cfg.Port != 9001 || !cfg.Debug {
				t.Fatalf("valid config fields were not preserved: port=%d debug=%t", cfg.Port, cfg.Debug)
			}
			if cfg.AuthLoadWorkers != DefaultAuthLoadWorkers {
				t.Fatalf("AuthLoadWorkers = %d, want default %d", cfg.AuthLoadWorkers, DefaultAuthLoadWorkers)
			}
			if got := cfg.OAuthExcludedModels["codex"]; !reflect.DeepEqual(got, []string{"valid-model"}) {
				t.Fatalf("OAuthExcludedModels[codex] = %#v, want valid entry only", got)
			}

			entries := hook.AllEntries()
			if len(entries) != 2 {
				t.Fatalf("warning count = %d, want 2", len(entries))
			}
			wantContent := map[int]string{
				5: "- name: <redacted>",
				6: "auth-load-workers: <redacted>",
			}
			wantDetail := map[int]string{
				5: "cannot unmarshal !!map into string",
				6: "cannot unmarshal !!str into int",
			}
			for _, entry := range entries {
				if entry.Level != log.WarnLevel || entry.Message != "ignoring invalid config value" {
					t.Fatalf("unexpected log entry: level=%s message=%q", entry.Level, entry.Message)
				}
				line, okLine := entry.Data["line"].(int)
				if !okLine {
					t.Fatalf("warning line = %#v, want int", entry.Data["line"])
				}
				content, okContent := entry.Data["content"].(string)
				if !okContent || content != wantContent[line] {
					t.Fatalf("warning content at line %d = %#v, want %q", line, entry.Data["content"], wantContent[line])
				}
				detail, okDetail := entry.Data["error"].(string)
				if !okDetail || detail != wantDetail[line] {
					t.Fatalf("warning error at line %d = %#v, want %q", line, entry.Data["error"], wantDetail[line])
				}
				for _, secret := range []string{"invalid-model-secret", "worker-secret", "worker-"} {
					if strings.Contains(content, secret) || strings.Contains(detail, secret) {
						t.Fatalf("warning leaked %q: content=%q error=%q", secret, content, detail)
					}
				}
			}
		})
	}
}

func TestConfigLoadersRejectNonTypeMismatchYAMLErrors(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
	}{
		{name: "malformed collection", raw: []byte("auth-load-workers: [\n")},
		{name: "duplicate key", raw: []byte("port: 9001\nport: 9002\n")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, errParse := ParseConfigBytes(test.raw); errParse == nil {
				t.Fatal("ParseConfigBytes() error = nil")
			}
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			if errWrite := os.WriteFile(configPath, test.raw, 0o600); errWrite != nil {
				t.Fatal(errWrite)
			}
			if _, errLoad := LoadConfig(configPath); errLoad == nil {
				t.Fatal("LoadConfig() error = nil")
			}
		})
	}
}

func captureConfigWarnings(t *testing.T) *logtest.Hook {
	t.Helper()
	logger := log.StandardLogger()
	previousHooks := logger.ReplaceHooks(make(log.LevelHooks))
	previousOutput := logger.Out
	logger.SetOutput(io.Discard)
	hook := logtest.NewLocal(logger)
	t.Cleanup(func() {
		logger.ReplaceHooks(previousHooks)
		logger.SetOutput(previousOutput)
	})
	return hook
}

func TestParseConfigBytesAuthLoadWorkers(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want int
	}{
		{name: "default", yaml: "port: 8317\n", want: 16},
		{name: "minimum", yaml: "auth-load-workers: 1\n", want: 1},
		{name: "maximum", yaml: "auth-load-workers: 64\n", want: 64},
		{name: "below minimum", yaml: "auth-load-workers: -8\n", want: 1},
		{name: "above maximum", yaml: "auth-load-workers: 128\n", want: 64},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, errParse := ParseConfigBytes([]byte(tt.yaml))
			if errParse != nil {
				t.Fatalf("ParseConfigBytes() error = %v", errParse)
			}
			if cfg.AuthLoadWorkers != tt.want {
				t.Fatalf("AuthLoadWorkers = %d, want %d", cfg.AuthLoadWorkers, tt.want)
			}
		})
	}
}

func TestLoadConfigOptionalAuthLoadWorkers(t *testing.T) {
	tests := []struct {
		name    string
		content []byte
		create  bool
	}{
		{name: "missing file"},
		{name: "empty file", create: true},
		{name: "invalid yaml", content: []byte("auth-load-workers: [\n"), create: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			if tt.create {
				if errWrite := os.WriteFile(configPath, tt.content, 0o600); errWrite != nil {
					t.Fatalf("os.WriteFile() error = %v", errWrite)
				}
			}

			cfg, errLoad := LoadConfigOptional(configPath, true)
			if errLoad != nil {
				t.Fatalf("LoadConfigOptional() error = %v", errLoad)
			}
			if cfg.AuthLoadWorkers != DefaultAuthLoadWorkers {
				t.Fatalf("AuthLoadWorkers = %d, want %d", cfg.AuthLoadWorkers, DefaultAuthLoadWorkers)
			}
		})
	}
}

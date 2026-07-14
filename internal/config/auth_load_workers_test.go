package config

import (
	"os"
	"path/filepath"
	"testing"
)

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

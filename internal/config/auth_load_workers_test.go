package config

import "testing"

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

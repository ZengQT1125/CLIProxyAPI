package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPanelRepositoryConfigIsPersisted(t *testing.T) {
	const source = `remote-management:
  allow-remote: true
  panel-github-repository: https://github.com/acme/incompatible-panel
  panel-repo: https://github.com/acme/legacy-panel
`

	cfg, errParse := ParseConfigBytes([]byte(source))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}
	if got := cfg.RemoteManagement.PanelGitHubRepository; got != "https://github.com/acme/incompatible-panel" {
		t.Fatalf("panel repository = %q", got)
	}
	rendered, errMarshal := yaml.Marshal(cfg)
	if errMarshal != nil {
		t.Fatalf("yaml.Marshal() error = %v", errMarshal)
	}
	if !strings.Contains(string(rendered), "panel-github-repository: https://github.com/acme/incompatible-panel") {
		t.Fatalf("rendered config lost panel repository:\n%s", rendered)
	}
	if strings.Contains(string(rendered), "panel-repo:") {
		t.Fatalf("rendered config retained legacy panel repository key:\n%s", rendered)
	}

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(configPath, []byte(source), 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}
	if errSave := SaveConfigPreserveComments(configPath, cfg); errSave != nil {
		t.Fatalf("SaveConfigPreserveComments() error = %v", errSave)
	}
	persisted, errRead := os.ReadFile(configPath)
	if errRead != nil {
		t.Fatalf("read config: %v", errRead)
	}
	if !strings.Contains(string(persisted), "panel-github-repository: https://github.com/acme/incompatible-panel") {
		t.Fatalf("persisted config lost panel repository:\n%s", persisted)
	}
	if strings.Contains(string(persisted), "panel-repo:") {
		t.Fatalf("persisted config retained legacy panel repository key:\n%s", persisted)
	}
	if !strings.Contains(string(persisted), "allow-remote: true") {
		t.Fatalf("persisted config lost supported remote management settings:\n%s", persisted)
	}
}

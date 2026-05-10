package cliproxyapi_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestGoImportsUseCurrentModuleMajorVersion(t *testing.T) {
	modulePath := readModulePath(t)
	modulePattern := regexp.MustCompile(`github\.com/router-for-me/CLIProxyAPI/v[0-9]+`)

	var mismatches []string
	err := filepath.WalkDir(".", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch path {
			case ".git", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}

		content, errRead := os.ReadFile(path)
		if errRead != nil {
			return errRead
		}
		for _, match := range modulePattern.FindAllString(string(content), -1) {
			if match != modulePath {
				mismatches = append(mismatches, path+": "+match)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repository: %v", err)
	}
	if len(mismatches) > 0 {
		t.Fatalf("found imports using a stale module path:\n%s", strings.Join(mismatches, "\n"))
	}
}

func readModulePath(t *testing.T) string {
	t.Helper()

	content, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "module" {
			return fields[1]
		}
	}
	t.Fatal("go.mod does not declare a module path")
	return ""
}

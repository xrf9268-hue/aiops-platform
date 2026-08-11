package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestExampleWorkflowsWireTrackerSecretPerKind guards the documentation
// promise made by the README quick start and docs/runbooks/local-dev.md
// (#780): every shipped example workflow that declares a tracker kind wires
// adapter-owned secret as the whole-value env reference the docs name for that
// kind. The Gitea example once shipped without its token line, so a
// reader following the docs got a worker that failed every poll with no CI
// signal; this test makes that regression loud. Loader-side whole-value
// $VAR expansion for documented provider keys is pinned separately by
// TestLoadResolvesExactEnvironmentReferences.
func TestExampleWorkflowsWireTrackerSecretPerKind(t *testing.T) {
	apiKeyByKind := map[string]string{
		"linear": "$LINEAR_API_KEY",
		"gitea":  "$GITEA_TOKEN",
		"github": "$GITHUB_TOKEN",
	}

	examplesDir := filepath.Join("..", "..", "examples")
	entries, err := os.ReadDir(examplesDir)
	if err != nil {
		t.Fatalf("ReadDir(%q) = %v; want example workflows readable from the repo", examplesDir, err)
	}

	checked := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(examplesDir, entry.Name())
		front := exampleFrontMatter(t, path)
		if front.Tracker.Kind == "" {
			continue
		}
		want, ok := apiKeyByKind[front.Tracker.Kind]
		if !ok {
			t.Errorf("%s: tracker.kind = %q; want one of linear/gitea/github", entry.Name(), front.Tracker.Kind)
			continue
		}
		secretKey := "token"
		if front.Tracker.Kind == "linear" {
			secretKey = "api_key"
		}
		if got := front.Tracker.Provider[secretKey]; got != want {
			t.Errorf("%s (kind=%s): tracker.provider.%s = %q; want %q per the README/local-dev per-kind mapping", entry.Name(), front.Tracker.Kind, secretKey, got, want)
		}
		checked++
	}
	if checked == 0 {
		t.Fatalf("checked %d example workflows in %q; want at least one with tracker.kind set", checked, examplesDir)
	}
}

// exampleFrontMatter extracts and decodes the YAML front matter of one
// example workflow. It fails the test rather than skipping so a malformed
// example cannot silently drop out of the wiring check.
func exampleFrontMatter(t *testing.T, path string) (front struct {
	Tracker struct {
		Kind     string         `yaml:"kind"`
		Provider map[string]any `yaml:"provider"`
	} `yaml:"tracker"`
}) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) = %v", path, err)
	}
	body := strings.TrimPrefix(string(raw), "---\n")
	end := strings.Index(body, "\n---")
	if !strings.HasPrefix(string(raw), "---\n") || end < 0 {
		t.Fatalf("exampleFrontMatter(%q): no leading YAML front matter block; every shipped example is expected to carry one", path)
	}
	if err := yaml.Unmarshal([]byte(body[:end]), &front); err != nil {
		t.Fatalf("yaml.Unmarshal(%q front matter) = %v", path, err)
	}
	return front
}

package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestExampleWorkflowsWireTrackerAPIKeyPerKind guards the documentation
// promise made by the README quick start and docs/runbooks/local-dev.md
// (#780): every shipped example workflow that declares a tracker kind wires
// tracker.api_key as the whole-value env reference the docs name for that
// kind. The gitea example shipped without the api_key line at all, so a
// reader following the docs got a worker that failed every poll with no CI
// signal; this test makes that regression loud. Loader-side whole-value
// $VAR expansion (api_key and endpoint alike) is pinned separately by
// TestLoadResolvesExactEnvironmentReferences.
func TestExampleWorkflowsWireTrackerAPIKeyPerKind(t *testing.T) {
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
		if got := front.Tracker.APIKey; got != want {
			t.Errorf("%s (kind=%s): tracker.api_key = %q; want %q per the README/local-dev per-kind mapping", entry.Name(), front.Tracker.Kind, got, want)
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
		Kind   string `yaml:"kind"`
		APIKey string `yaml:"api_key"`
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

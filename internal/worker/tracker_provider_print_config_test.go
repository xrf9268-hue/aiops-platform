package worker

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPrintConfigMasksOnlyAdapterDeclaredProviderSecrets(t *testing.T) {
	t.Setenv("AIOPS_TEST_GITEA_PROVIDER_TOKEN", "gitea-provider-secret")
	dir := t.TempDir()
	body := `---
repo:
  owner: acme
  name: widgets
  clone_url: git@example.com:acme/widgets.git
tracker:
  kind: gitea
  provider:
    base_url: https://gitea.example.test
    token: $AIOPS_TEST_GITEA_PROVIDER_TOKEN
    repo: acme/widgets
    future_scope:
      board: delivery
---
prompt
`
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := printConfig(dir, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("printConfig exit = %d, stderr = %s", code, stderr.String())
	}
	if bytes.Contains(stdout.Bytes(), []byte("gitea-provider-secret")) {
		t.Fatalf("provider token leaked in print-config: %s", stdout.String())
	}
	var out struct {
		Config struct {
			Tracker struct {
				Provider map[string]any `json:"provider"`
			} `json:"tracker"`
		} `json:"config"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode print-config: %v\n%s", err, stdout.String())
	}
	provider := out.Config.Tracker.Provider
	if got := provider["token"]; got != maskedSecret {
		t.Fatalf("provider.token = %#v; want %q", got, maskedSecret)
	}
	if got := provider["base_url"]; got != "https://gitea.example.test" {
		t.Fatalf("provider.base_url = %#v; want observable endpoint", got)
	}
	if got := provider["repo"]; got != "acme/widgets" {
		t.Fatalf("provider.repo = %#v; want observable scope", got)
	}
	future, _ := provider["future_scope"].(map[string]any)
	if got := future["board"]; got != "delivery" {
		t.Fatalf("provider.future_scope = %#v; want unknown key preserved", provider["future_scope"])
	}
}

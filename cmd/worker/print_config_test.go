package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPrintConfig_MasksTrackerAPIKey verifies the secret-masking
// contract from the spec. Even when --print-config is used legitimately
// for debugging, the API key must never reach stdout. We test with the
// env-var indirection style that examples/WORKFLOW.md uses, since that
// is the realistic source of a non-empty key.
func TestPrintConfig_MasksTrackerAPIKey(t *testing.T) {
	t.Setenv("AIOPS_TEST_LINEAR_KEY", "lin_super_secret_value")
	dir := t.TempDir()
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\ntracker:\n  kind: linear\n  api_key: $AIOPS_TEST_LINEAR_KEY\n---\nprompt\n"
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := printConfig(dir, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	got := stdout.String()
	if strings.Contains(got, "lin_super_secret_value") {
		t.Fatalf("api_key value leaked into stdout:\n%s", got)
	}
	if !strings.Contains(got, `"api_key": "***"`) {
		t.Fatalf("api_key not masked; stdout:\n%s", got)
	}
}

// TestPrintConfig_DefaultSource verifies the simplest case: an empty
// workdir resolves to source=default, and the JSON output reports it
// without a path or shadowed_by field.
func TestPrintConfig_DefaultSource(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := printConfig(dir, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	var out struct {
		Resolution struct {
			Source     string   `json:"source"`
			Path       string   `json:"path,omitempty"`
			ShadowedBy []string `json:"shadowed_by,omitempty"`
		} `json:"resolution"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v\nstdout: %s", err, stdout.String())
	}
	if out.Resolution.Source != "default" {
		t.Fatalf("resolution.source = %q, want %q", out.Resolution.Source, "default")
	}
	if out.Resolution.Path != "" {
		t.Fatalf("resolution.path = %q, want empty", out.Resolution.Path)
	}
}

// TestPrintConfig_FileSourceWithPromptCanary covers two contracts at once:
//
//  1. Source=file populates config from the front matter (here: a
//     non-default agent.default and tracker.kind).
//  2. The prompt body is summarized rather than echoed. We embed a
//     recognizable canary string in the body and assert it never reaches
//     stdout. This is the spec's safety contract — see "Why prompt body
//     is summarized, not printed".
func TestPrintConfig_FileSourceWithPromptCanary(t *testing.T) {
	dir := t.TempDir()
	canary := "SHOULD_NOT_LEAK_xyz"
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\nagent:\n  default: codex\ntracker:\n  kind: linear\n---\nFirst line of prompt template.\nSecond line includes canary " + canary + " in the middle.\nMore body...\n"
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := printConfig(dir, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}

	if strings.Contains(stdout.String(), canary) {
		t.Fatalf("canary %q leaked into stdout:\n%s", canary, stdout.String())
	}

	var out struct {
		Resolution struct {
			Source string `json:"source"`
		} `json:"resolution"`
		Config struct {
			Agent struct {
				Default string `json:"default"`
			} `json:"agent"`
		} `json:"config"`
		PromptTemplate struct {
			Length    int    `json:"length"`
			FirstLine string `json:"first_line"`
		} `json:"prompt_template"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v\nstdout: %s", err, stdout.String())
	}
	if out.Resolution.Source != "file" {
		t.Fatalf("source = %q, want %q", out.Resolution.Source, "file")
	}
	if out.Config.Agent.Default != "codex" {
		t.Fatalf("agent.default = %q, want %q", out.Config.Agent.Default, "codex")
	}
	if out.PromptTemplate.FirstLine != "First line of prompt template." {
		t.Fatalf("first_line = %q", out.PromptTemplate.FirstLine)
	}
	if out.PromptTemplate.Length <= 0 {
		t.Fatalf("length = %d, want > 0", out.PromptTemplate.Length)
	}
}

// TestPrintConfig_SchemaErrorReturnsExitOne pins the contract that
// schema validation failures produce a non-zero exit and route the
// human-readable error to stderr. Stdout must remain empty so a script
// piping the JSON elsewhere does not feed it a malformed document.
func TestPrintConfig_SchemaErrorReturnsExitOne(t *testing.T) {
	dir := t.TempDir()
	body := "---\nrepo:\n  owner: o\n  name: r\n---\nprompt\n" // no clone_url
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := printConfig(dir, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout not empty on error: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "repo.clone_url") {
		t.Fatalf("stderr missing field name: %s", stderr.String())
	}
}

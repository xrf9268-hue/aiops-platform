package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

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

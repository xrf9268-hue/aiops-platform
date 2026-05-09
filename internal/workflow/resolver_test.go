package workflow

import (
	"testing"
)

// TestResolve_NoFileReturnsDefaults pins the contract from spec section
// "Discovery Contract": when no WORKFLOW.md exists in any of the search
// locations, Resolve must succeed with Source=default and the schema
// defaults applied (so a fresh repo can run with the mock runner).
func TestResolve_NoFileReturnsDefaults(t *testing.T) {
	dir := t.TempDir()
	wf, res, err := Resolve(dir)
	if err != nil {
		t.Fatalf("Resolve: unexpected error: %v", err)
	}
	if res.Source != SourceDefault {
		t.Fatalf("Source = %q, want %q", res.Source, SourceDefault)
	}
	if res.Path != "" {
		t.Fatalf("Path = %q, want empty (no file)", res.Path)
	}
	if len(res.ShadowedBy) != 0 {
		t.Fatalf("ShadowedBy = %v, want empty", res.ShadowedBy)
	}
	if wf == nil || wf.Config.Agent.Default != "mock" {
		t.Fatalf("default Agent.Default = %q, want %q", wf.Config.Agent.Default, "mock")
	}
}

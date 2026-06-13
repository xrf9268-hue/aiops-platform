package main

import "testing"

// TestResolveVersionPrefersStamp pins the -ldflags -X path for the TUI: a
// stamped main.version wins over the VCS fallback (#796).
func TestResolveVersionPrefersStamp(t *testing.T) {
	orig := version
	t.Cleanup(func() { version = orig })
	version = "v1.2.3"
	if got := resolveVersion(); got != "v1.2.3" {
		t.Fatalf("resolveVersion() = %q, want v1.2.3 (the -X stamp must win)", got)
	}
}

package main

import (
	"encoding/json"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/orchestrator"
)

// TestResolveVersionPrefersStamp pins the -ldflags -X path: a stamped
// main.version wins over the VCS fallback (#796).
func TestResolveVersionPrefersStamp(t *testing.T) {
	orig := version
	t.Cleanup(func() { version = orig })
	version = "v1.2.3"
	if got := resolveVersion(); got != "v1.2.3" {
		t.Fatalf("resolveVersion() = %q, want v1.2.3 (the -X stamp must win)", got)
	}
}

// TestStateAPIStampsVersion mutation-verifies the wiring seam: the version
// stamp must reach /api/v1/state. Dropping `Version: resolveVersion()` from
// apiStateFromView omits the (omitempty) key and fails this test (#796).
func TestStateAPIStampsVersion(t *testing.T) {
	orig := version
	t.Cleanup(func() { version = orig })
	version = "v9.9.9-test"
	raw, err := json.Marshal(apiStateFromView(orchestrator.StateView{}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["version"] != "v9.9.9-test" {
		t.Fatalf("/api/v1/state version = %#v, want v9.9.9-test (mapper must stamp resolveVersion into the response)", got["version"])
	}
}

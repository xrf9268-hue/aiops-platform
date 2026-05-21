package workflow

import (
	"strings"
	"testing"
)

func TestLoadRejectsLinearTrackerWithoutProjectSlug(t *testing.T) {
	path := writeTempWorkflow(t, `---
repo:
  owner: xrf9268-hue
  name: aiops-platform
  clone_url: https://github.com/xrf9268-hue/aiops-platform.git
tracker:
  kind: linear
---
Prompt body
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load(linear without tracker.project_slug) = nil, want validation error")
	}
	for _, want := range []string{path, "tracker.project_slug", "required", "tracker.kind is linear"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Load error = %q, want substring %q", err, want)
		}
	}
}

func TestLoadRejectsLinearServiceRouteWithoutAnyProjectSlug(t *testing.T) {
	path := writeTempWorkflow(t, `---
repo:
  owner: xrf9268-hue
  name: aiops-platform
  clone_url: https://github.com/xrf9268-hue/aiops-platform.git
tracker:
  kind: linear
services:
  - name: api
    repo:
      owner: acme
      name: api
      clone_url: git@example.com:acme/api.git
    tracker:
      team_key: ENG
---
Prompt body
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load(linear service route without any project slug) = nil, want validation error")
	}
	for _, want := range []string{path, "services[0].tracker.project_slug", "tracker.project_slug", "required"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Load error = %q, want substring %q", err, want)
		}
	}
}

func TestLoadRejectsNonPositiveHooksTimeoutMs(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "top-level negative",
			body: `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
hooks:
  timeout_ms: -1
---
Prompt body
`,
		},
		{
			name: "top-level zero",
			body: `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
hooks:
  timeout_ms: 0
---
Prompt body
`,
		},
		{
			name: "legacy negative",
			body: `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
workspace:
  hooks:
    timeout_ms: -1
---
Prompt body
`,
		},
		{
			name: "legacy zero",
			body: `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
workspace:
  hooks:
    timeout_ms: 0
---
Prompt body
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTempWorkflow(t, tt.body)
			_, err := Load(path)
			if err == nil {
				t.Fatal("Load succeeded with explicit non-positive hooks timeout, want validation error")
			}
			for _, want := range []string{path, "timeout_ms", "positive integer"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("Load error = %q, want substring %q", err, want)
				}
			}
		})
	}
}

package workflow

import (
	"strings"
	"testing"
)

func TestRenderRejectsMissingVariable(t *testing.T) {
	_, err := Render("work on {{ missing }}", map[string]any{})
	if err == nil {
		t.Fatal("Render succeeded, want missing variable error")
	}
	if !strings.Contains(err.Error(), "template_render_error") || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("Render error = %q, want typed missing variable error", err)
	}
}

func TestRenderRejectsUnknownFilter(t *testing.T) {
	_, err := Render("work on {{ task.title | definitely_missing_filter }}", map[string]any{
		"task": map[string]any{"title": "strict templates"},
	})
	if err == nil {
		t.Fatal("Render succeeded, want unknown filter error")
	}
	if !strings.Contains(err.Error(), "template_render_error") || !strings.Contains(err.Error(), "definitely_missing_filter") {
		t.Fatalf("Render error = %q, want typed unknown filter error", err)
	}
}

func TestRenderSupportsLiquidEscapeFilter(t *testing.T) {
	got, err := Render("{{ task.title | escape }}", map[string]any{
		"task": map[string]any{"title": "strict <templates> & prompts"},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != "strict &lt;templates&gt; &amp; prompts" {
		t.Fatalf("Render = %q", got)
	}
}

func TestRenderSupportsLiquidDefaultFilter(t *testing.T) {
	got, err := Render(`{{ task.description | default: "no description" }}`, map[string]any{
		"task": map[string]any{"description": ""},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != "no description" {
		t.Fatalf("Render = %q", got)
	}
}

func TestRenderSupportsLiquidDefaultFilterForMissingVariable(t *testing.T) {
	got, err := Render(`{{ missing | default: "fallback" }}`, map[string]any{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != "fallback" {
		t.Fatalf("Render = %q", got)
	}
}

func TestRenderSupportsChainedLiquidFilters(t *testing.T) {
	got, err := Render(`{{ missing | default: "<fallback>" | escape }}`, map[string]any{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != "&lt;fallback&gt;" {
		t.Fatalf("Render = %q", got)
	}
}

func TestRenderRejectsLiquidTags(t *testing.T) {
	_, err := Render("{% if task.title %}work{% endif %}", map[string]any{
		"task": map[string]any{"title": "strict templates"},
	})
	if err == nil {
		t.Fatal("Render succeeded, want unsupported tag error")
	}
	if !strings.Contains(err.Error(), "template_render_error") || !strings.Contains(err.Error(), "unsupported tag") {
		t.Fatalf("Render error = %q, want typed unsupported tag error", err)
	}
}

func TestRenderSupportsNestedTaskAndAttemptVariables(t *testing.T) {
	got, err := Render("{{ task.title }} attempt {{ attempt }} for {{ repo.owner }}/{{ repo.name }}", map[string]any{
		"task":    map[string]any{"title": "strict templates"},
		"repo":    map[string]any{"owner": "acme", "name": "demo"},
		"attempt": 2,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != "strict templates attempt 2 for acme/demo" {
		t.Fatalf("Render = %q", got)
	}
}

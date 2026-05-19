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

func TestRenderSupportsLiquidIfTags(t *testing.T) {
	got, err := Render("{% if attempt %}retry {{ attempt }}{% endif %}", map[string]any{
		"attempt": 2,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != "retry 2" {
		t.Fatalf("Render = %q", got)
	}
}

func TestRenderSkipsLiquidIfTagsForBlankValues(t *testing.T) {
	got, err := Render("first{% if attempt %} retry {{ attempt }}{% endif %}", map[string]any{
		"attempt": nil,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != "first" {
		t.Fatalf("Render = %q", got)
	}
}

func TestRenderLeavesBareNilAttemptEmpty(t *testing.T) {
	got, err := Render(`attempt={{ attempt }}`, map[string]any{"attempt": nil})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != "attempt=" {
		t.Fatalf("Render = %q, want bare nil attempt to render empty", got)
	}
}

func TestRenderEscapesNilAsEmpty(t *testing.T) {
	got, err := Render(`attempt={{ attempt | escape }}`, map[string]any{"attempt": nil})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != "attempt=" {
		t.Fatalf("Render = %q, want nil to escape as empty", got)
	}
}

func TestRenderLiquidIfRejectsMissingVariable(t *testing.T) {
	_, err := Render(`{% if missing %}yes{% endif %}`, map[string]any{})
	if err == nil {
		t.Fatal("Render succeeded, want strict missing variable error")
	}
	if !strings.Contains(err.Error(), "template_render_error") || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("Render error = %q, want typed missing variable error", err)
	}
}

func TestRenderLiquidIfTreatsOnlyNilAndFalseAsFalsy(t *testing.T) {
	for name, value := range map[string]any{
		"empty string": "",
		"zero":         0,
		"empty slice":  []string{},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Render(`start{% if value %} yes{% endif %}`, map[string]any{"value": value})
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if got != "start yes" {
				t.Fatalf("Render = %q, want Liquid truthy value to render body", got)
			}
		})
	}
}

func TestRenderSupportsNestedLiquidIfTags(t *testing.T) {
	got, err := Render(`a{% if outer %}b{% if inner %}c{% endif %}d{% endif %}e`, map[string]any{
		"outer": false,
		"inner": true,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != "ae" {
		t.Fatalf("Render = %q, want nested if body skipped cleanly", got)
	}
}

func TestRenderSupportsLiquidIfElseForFirstRunAttempt(t *testing.T) {
	got, err := Render(`{% if attempt %}retry {{ attempt }}{% else %}first run{% endif %}`, map[string]any{
		"attempt": nil,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != "first run" {
		t.Fatalf("Render = %q", got)
	}
}

func TestRenderSupportsLiquidIfComparison(t *testing.T) {
	got, err := Render(`{% if attempt > 1 %}later retry{% else %}first retry{% endif %}`, map[string]any{
		"attempt": 2,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != "later retry" {
		t.Fatalf("Render = %q", got)
	}
}

func TestRenderSupportsLiquidElsif(t *testing.T) {
	got, err := Render(`{% if attempt > 2 %}late{% elsif attempt == 2 %}second{% else %}first{% endif %}`, map[string]any{
		"attempt": 2,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != "second" {
		t.Fatalf("Render = %q", got)
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

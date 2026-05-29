package workflow

import "testing"

// FuzzRender guards the hand-written Liquid renderer that builds agent
// prompts. A silently mis-expanded template is a prompt-injection boundary
// risk (#411), so the fuzz target asserts two invariants beyond "does not
// panic / hang": Render never returns partial output alongside an error,
// and a successful render is deterministic for the same inputs.
func FuzzRender(f *testing.F) {
	seeds := []string{
		"",
		"   ",
		"{{ title }}",
		"{{ task.title }}",
		"{{ task.title | escape }}",
		`{{ task.description | default: "no description" }}`,
		`{{ a | default: "\"" }}`,
		"{% if x %}yes{% endif %}",
		"{% if a %}A{% elsif b %}B{% else %}C{% endif %}",
		"{% if x %}{% if a %}nested{% endif %}{% endif %}",
		"{% if count > 3 %}many{% endif %}",
		`{% if name == "v" %}match{% endif %}`,
		"{% endif %}",
		"{% if x %}",
		"{{",
		"{%",
		"{{ a.b.c.d }}",
		"{% if a == b %}{% elsif a != b %}{% endif %}",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	vars := map[string]any{
		"title": "T",
		"x":     true,
		"a":     "v",
		"b":     false,
		"count": 5,
		"name":  "v",
		"task": map[string]any{
			"title":       "strict <templates> & prompts",
			"description": "",
		},
	}
	f.Fuzz(func(t *testing.T, tpl string) {
		out, err := Render(tpl, vars)
		if err != nil {
			// An explicit error is a valid outcome; the contract is that
			// the renderer must not leak a half-expanded prompt with it.
			if out != "" {
				t.Fatalf("Render(%q) = (%q, %v); want empty output on error", tpl, out, err)
			}
			return
		}
		out2, err2 := Render(tpl, vars)
		if err2 != nil || out2 != out {
			t.Fatalf("Render(%q) not deterministic: first=(%q, nil), second=(%q, %v)", tpl, out, out2, err2)
		}
	})
}

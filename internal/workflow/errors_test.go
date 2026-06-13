package workflow

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestLoadSurfacesWorkflowErrorCategories(t *testing.T) {
	t.Run("missing workflow file", func(t *testing.T) {
		_, err := Load(filepath.Join(t.TempDir(), "WORKFLOW.md"))
		if !errors.Is(err, ErrMissingWorkflowFile) {
			t.Fatalf("Load missing file error = %T %[1]v, want ErrMissingWorkflowFile", err)
		}
		if got, ok := ErrorCategory(err); !ok || got != CategoryMissingWorkflowFile {
			t.Fatalf("ErrorCategory = %q, %v; want %q, true", got, ok, CategoryMissingWorkflowFile)
		}
	})

	t.Run("non-missing read error", func(t *testing.T) {
		_, err := Load(t.TempDir())
		if err == nil {
			t.Fatalf("Load directory path error = nil, want read error")
		}
		if errors.Is(err, ErrMissingWorkflowFile) {
			t.Fatalf("Load directory path error = %T %[1]v, must not match ErrMissingWorkflowFile", err)
		}
		if got, ok := ErrorCategory(err); ok {
			t.Fatalf("ErrorCategory(directory read error) = %q, true; want uncategorized", got)
		}
	})

	t.Run("front matter parse error", func(t *testing.T) {
		path := writeTempWorkflow(t, "---\nrepo: [\n---\nbody\n")
		_, err := Load(path)
		if !errors.Is(err, ErrWorkflowParse) {
			t.Fatalf("Load malformed YAML error = %T %[1]v, want ErrWorkflowParse", err)
		}
	})

	t.Run("front matter root is not map", func(t *testing.T) {
		path := writeTempWorkflow(t, "---\n- repo\n---\nbody\n")
		_, err := Load(path)
		if !errors.Is(err, ErrWorkflowFrontMatterNotMap) {
			t.Fatalf("Load non-map YAML error = %T %[1]v, want ErrWorkflowFrontMatterNotMap", err)
		}
	})
}

func TestRenderSurfacesTemplateErrorCategories(t *testing.T) {
	_, err := Render("{% definitely_not_supported %}", map[string]any{})
	if !errors.Is(err, ErrTemplateParse) {
		t.Fatalf("Render syntax error = %T %[1]v, want ErrTemplateParse", err)
	}

	_, err = Render("work on {{ missing }}", map[string]any{})
	if !errors.Is(err, ErrTemplateRender) {
		t.Fatalf("Render missing variable error = %T %[1]v, want ErrTemplateRender", err)
	}
	if got, ok := ErrorCategory(err); !ok || got != CategoryTemplateRenderError {
		t.Fatalf("ErrorCategory = %q, %v; want %q, true", got, ok, CategoryTemplateRenderError)
	}
}

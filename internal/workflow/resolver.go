package workflow

import (
	"os"
	"path/filepath"
	"strings"
)

// Source describes where the effective workflow came from on a given
// load. SourceFile means a WORKFLOW.md with valid YAML front matter was
// found and parsed; SourcePromptOnly means a file existed but had no
// front matter (the body became the prompt template, config came from
// schema defaults); SourceDefault means no file was found at all.
type Source string

const (
	SourceFile       Source = "file"
	SourcePromptOnly Source = "prompt_only"
	SourceDefault    Source = "default"
)

// Resolution carries the runtime fact of how a workflow was loaded for
// a single task. It is intentionally separate from Workflow because
// "where did this come from on this invocation" is not a property of
// the configuration itself; it is per-load metadata that the worker
// uses to populate the workflow_resolved event.
type Resolution struct {
	Source     Source
	Path       string   // repo-relative; "" when Source == SourceDefault
	ShadowedBy []string // other repo-relative paths that exist but lost precedence
}

var resolveCandidates = []string{
	"WORKFLOW.md",
	".aiops/WORKFLOW.md",
	".github/WORKFLOW.md",
}

// Resolve discovers WORKFLOW.md inside workdir (expected to be an absolute
// path), applying the documented precedence (root > .aiops/ > .github/),
// and returns the loaded Workflow alongside a Resolution describing the
// source. When no file is found, the schema defaults are returned with
// Source=default.
func Resolve(workdir string) (*Workflow, *Resolution, error) {
	var found string
	for _, rel := range resolveCandidates {
		abs := filepath.Join(workdir, rel)
		info, err := os.Stat(abs)
		if err == nil && !info.IsDir() {
			if found == "" {
				found = rel
			}
			continue
		}
		if err != nil && !os.IsNotExist(err) {
			return nil, nil, err
		}
	}
	if found == "" {
		cfg := DefaultConfig()
		expandConfig(&cfg)
		wf := &Workflow{Config: cfg, PromptTemplate: DefaultPrompt()}
		return wf, &Resolution{Source: SourceDefault}, nil
	}
	abs := filepath.Join(workdir, found)
	wf, err := Load(abs)
	if err != nil {
		return nil, nil, err
	}
	return wf, &Resolution{Source: SourceFile, Path: found}, nil
}

// hasFrontMatterAt returns true when path begins with a YAML front
// matter fence (`---\n` or `---\r\n`) followed somewhere by a closing
// fence. Used by Resolve to distinguish prompt-only files from full
// workflow files without threading a flag out of Load.
func hasFrontMatterAt(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	s := string(b)
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return false
	}
	trimmed := strings.TrimPrefix(strings.TrimPrefix(s, "---\r\n"), "---\n")
	return strings.Contains(trimmed, "\n---")
}

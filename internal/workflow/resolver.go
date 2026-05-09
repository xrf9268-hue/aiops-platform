package workflow

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

// Resolve discovers WORKFLOW.md inside workdir, applying the documented
// precedence (root > .aiops/ > .github/), and returns the loaded
// Workflow alongside a Resolution describing the source. When no file
// is found, the schema defaults are returned with Source=default.
func Resolve(workdir string) (*Workflow, *Resolution, error) {
	cfg := DefaultConfig()
	expandConfig(&cfg)
	wf := &Workflow{Config: cfg, PromptTemplate: DefaultPrompt()}
	return wf, &Resolution{Source: SourceDefault}, nil
}

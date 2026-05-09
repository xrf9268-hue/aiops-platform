package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// printConfigOutput is the JSON shape of `worker --print-config <dir>`.
// Stable: external tooling may consume it.
type printConfigOutput struct {
	Resolution     printConfigResolution `json:"resolution"`
	Config         workflow.Config       `json:"config"`
	PromptTemplate promptSummary         `json:"prompt_template"`
}

type printConfigResolution struct {
	Source     string   `json:"source"`
	Path       string   `json:"path,omitempty"`
	ShadowedBy []string `json:"shadowed_by,omitempty"`
}

// promptSummary is intentionally not the full prompt body. See spec
// section "Why prompt body is summarized, not printed" for the rationale.
type promptSummary struct {
	Length    int    `json:"length"`
	FirstLine string `json:"first_line"`
}

// printConfig writes the effective workflow for workdir as JSON to
// stdout. Returns the process exit code (0 on success, 1 on schema
// validation error). Used both by main()'s --print-config dispatch and
// by tests; stdout/stderr are explicit io.Writer parameters so tests
// can capture the output without subprocessing.
func printConfig(workdir string, stdout, stderr io.Writer) int {
	wf, res, err := workflow.Resolve(workdir)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	out := printConfigOutput{
		Resolution: printConfigResolution{
			Source:     string(res.Source),
			Path:       res.Path,
			ShadowedBy: res.ShadowedBy,
		},
		Config:         wf.Config,
		PromptTemplate: promptSummary{}, // populated in Task 12
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	return 0
}

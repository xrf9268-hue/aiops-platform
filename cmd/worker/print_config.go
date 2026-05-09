package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

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

const promptFirstLineMaxBytes = 200

// summarizePrompt produces the bounded prompt summary published by
// --print-config. Length is the byte length of the trimmed body so a
// reader can sanity-check completeness; FirstLine is truncated to keep
// debug output cheap to paste even when an author writes a long single
// line. The full body is never echoed (see spec safety contract).
func summarizePrompt(body string) promptSummary {
	trimmed := strings.TrimSpace(body)
	first := trimmed
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	if len(first) > promptFirstLineMaxBytes {
		first = first[:promptFirstLineMaxBytes]
	}
	return promptSummary{
		Length:    len(trimmed),
		FirstLine: first,
	}
}

const maskedSecret = "***"

// maskSecrets rewrites secret-bearing fields on a Config to a fixed
// placeholder before serialization. The function takes its argument by
// value; the workflow.Config used by the running worker is never
// touched. Currently only Tracker.APIKey is masked — extend this list
// when new secret-bearing fields are added to the schema.
func maskSecrets(cfg workflow.Config) workflow.Config {
	if cfg.Tracker.APIKey != "" {
		cfg.Tracker.APIKey = maskedSecret
	}
	return cfg
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
		Config:         maskSecrets(wf.Config),
		PromptTemplate: summarizePrompt(wf.PromptTemplate),
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	return 0
}

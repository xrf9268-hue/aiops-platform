package workflow

import "strings"

func DefaultPrompt() string {
	return `You are working on an AI coding task.

Read the task context, inspect the repository before editing, make the smallest safe change, run verification commands, and produce a clear summary.

Handoff:
- Push branches, open pull requests, and write tracker updates yourself using the tools available in the runtime environment.
- If a linear_graphql tool is available, use it for Linear state transitions, comments, and PR-link handoff updates; the orchestrator keeps the Linear token isolated from your process.
- The orchestrator is a scheduler/runner and tracker reader. Do not expect new workflow designs to rely on orchestrator-side ticket moves, comments, pushes, or pull-request handoffs after you exit.

Rules:
- Do not touch secrets, credentials, production deployment files, or database migrations unless explicitly requested.
- Prefer a small change over a broad refactor.
- If blocked, explain the blocker and stop.`
}

func Render(template string, vars map[string]string) string {
	if strings.TrimSpace(template) == "" {
		template = DefaultPrompt()
	}
	out := template
	for k, v := range vars {
		out = strings.ReplaceAll(out, "{{ "+k+" }}", v)
		out = strings.ReplaceAll(out, "{{"+k+"}}", v)
	}
	return out
}

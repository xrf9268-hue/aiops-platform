package workflow

import "strings"

func DefaultPrompt() string {
	return `You are working on an AI coding task.

Read the task context, inspect the repository before editing, make the smallest safe change, run verification commands, and produce a clear summary.

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

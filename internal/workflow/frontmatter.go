package workflow

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// splitFrontMatter peels the SPEC §5.2 YAML front-matter block off
// the start of a workflow file. The opening fence is `---` followed
// by a newline; the closing fence is a line that is **exactly**
// `---` (with an optional CR before the LF) and nothing else. The
// earlier substring-based scan would mis-match `---` lines that
// appear inside YAML block scalars or quoted strings — see #231,
// where `description: |` blocks legitimately contained a `---` line
// and silently truncated the parsed Config.
//
// Returns (front, body). When no opening fence is present or no
// closing fence can be found, returns ("", s) so the caller treats
// the whole file as the prompt body.
func splitFrontMatter(s string) (string, string) {
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return "", s
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(s, "---\r\n"), "---\n")
	lines := strings.SplitAfter(rest, "\n")
	var front strings.Builder
	for i, line := range lines {
		// Strip only the line-ending (CR/LF) for the fence comparison.
		// Trailing spaces, indentation, or any other content disqualify
		// the line from being the closing fence.
		if strings.TrimRight(line, "\r\n") == "---" {
			return front.String(), strings.Join(lines[i+1:], "")
		}
		front.WriteString(line)
	}
	return "", s
}

// hasNestedKey reports whether the YAML front matter contains the value at the
// nested key path. It is the shared presence probe behind the *Set flags,
// migration aliasing, and removed-field rejection — distinguishing "key absent"
// from "key present but zero-valued", which the typed decoder cannot.
func hasNestedKey(front []byte, path ...string) bool {
	var raw map[string]any
	if err := yaml.Unmarshal(front, &raw); err != nil {
		return false
	}
	var current any = raw
	for _, key := range path {
		m, ok := current.(map[string]any)
		if !ok {
			return false
		}
		current, ok = m[key]
		if !ok {
			return false
		}
	}
	return true
}

func hookFieldPresence(front []byte, path ...string) HookFieldPresence {
	return HookFieldPresence{
		AfterCreate:    hasNestedKey(front, append(path, "after_create")...),
		BeforeRun:      hasNestedKey(front, append(path, "before_run")...),
		AfterRun:       hasNestedKey(front, append(path, "after_run")...),
		BeforeRemove:   hasNestedKey(front, append(path, "before_remove")...),
		TimeoutMs:      hasNestedKey(front, append(path, "timeout_ms")...),
		EnvPassthrough: hasNestedKey(front, append(path, "env_passthrough")...),
	}
}

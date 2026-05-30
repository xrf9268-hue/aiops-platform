// Package policy implements path and diffstat policy checks used by the
// worker before pushing AI-generated commits. It is intentionally
// dependency-free so it can be unit tested without git or a database.
package policy

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Config mirrors the relevant fields of workflow.PolicyConfig. It is
// duplicated as a small struct so this package does not import workflow
// (which would create a cycle once workflow uses this package).
type Config struct {
	AllowPaths      []string
	DenyPaths       []string
	MaxChangedFiles int
	MaxChangedLines int
}

// ViolationKind enumerates the structured policy failure categories.
type ViolationKind string

const (
	KindDenyPath        ViolationKind = "deny_path"
	KindNotAllowed      ViolationKind = "not_allowed"
	KindMaxChangedFiles ViolationKind = "max_changed_files"
	KindMaxChangedLines ViolationKind = "max_changed_lines"
)

// Violation describes a single policy failure with enough structure for
// task event payloads and human-readable error messages.
type Violation struct {
	Kind    ViolationKind `json:"kind"`
	Path    string        `json:"path,omitempty"`
	Pattern string        `json:"pattern,omitempty"`
	Limit   int           `json:"limit,omitempty"`
	Actual  int           `json:"actual,omitempty"`
	Message string        `json:"message"`
}

func (v Violation) Error() string { return v.Message }

// Diffstat is the per-run measurement used to evaluate policy.
type Diffstat struct {
	Files []string
	Lines int
}

// Evaluate returns every policy violation observed for the given diffstat
// and config. An empty slice means the change is acceptable.
//
// Rules:
//   - If AllowPaths is non-empty, every changed file must match at least one
//     allow pattern, otherwise a not_allowed violation is recorded.
//   - Any file matching DenyPaths produces a deny_path violation, even if it
//     also matches AllowPaths (deny wins).
//   - MaxChangedFiles / MaxChangedLines (when > 0) are enforced as upper
//     bounds on len(Files) and Lines respectively.
func Evaluate(d Diffstat, cfg Config) []Violation { //nolint:gocognit // baseline (#521)
	var out []Violation

	for _, f := range d.Files {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if pat, ok := matchAny(cfg.DenyPaths, f); ok {
			out = append(out, Violation{
				Kind:    KindDenyPath,
				Path:    f,
				Pattern: pat,
				Message: fmt.Sprintf("path %q matches deny pattern %q", f, pat),
			})
			continue
		}
		if len(cfg.AllowPaths) > 0 {
			if _, ok := matchAny(cfg.AllowPaths, f); !ok {
				out = append(out, Violation{
					Kind:    KindNotAllowed,
					Path:    f,
					Message: fmt.Sprintf("path %q does not match any allow pattern", f),
				})
			}
		}
	}

	if cfg.MaxChangedFiles > 0 && len(d.Files) > cfg.MaxChangedFiles {
		out = append(out, Violation{
			Kind:    KindMaxChangedFiles,
			Limit:   cfg.MaxChangedFiles,
			Actual:  len(d.Files),
			Message: fmt.Sprintf("changed files %d exceeds max %d", len(d.Files), cfg.MaxChangedFiles),
		})
	}

	if cfg.MaxChangedLines > 0 && d.Lines > cfg.MaxChangedLines {
		out = append(out, Violation{
			Kind:    KindMaxChangedLines,
			Limit:   cfg.MaxChangedLines,
			Actual:  d.Lines,
			Message: fmt.Sprintf("changed lines %d exceeds max %d", d.Lines, cfg.MaxChangedLines),
		})
	}

	return out
}

// Summarize returns a single-line summary of all violations, suitable for a
// task event message field.
func Summarize(vs []Violation) string {
	if len(vs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(vs))
	for _, v := range vs {
		parts = append(parts, v.Message)
	}
	return strings.Join(parts, "; ")
}

func matchAny(patterns []string, path string) (string, bool) {
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if Match(p, path) {
			return p, true
		}
	}
	return "", false
}

// Match reports whether path matches the glob pattern. Supported syntax:
//
//   - "?"  matches exactly one rune (Unicode code point) except "/"
//   - "*"  matches any sequence of characters except "/"
//   - "**" matches any sequence of characters, including "/"
//   - "/**" as a trailing component matches the empty string or "/<anything>"
//   - "**/" as a leading component matches the empty string or "<anything>/"
//
// Patterns are matched against the full path (no implicit prefix). A pattern
// with no wildcards must equal path exactly. Pattern matching is anchored at
// both ends.
func Match(pattern, path string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return false
	}
	// Convenience: "foo/" is treated as "foo/**" (directory match).
	if strings.HasSuffix(pattern, "/") {
		pattern += "**"
	}
	return globMatch(pattern, path)
}

// globMatch is the recursive glob matcher. It is a thin dispatcher: the
// separator-spanning "**" and single-segment "*" wildcards delegate to
// helpers, "?" consumes exactly one rune via matchSingleRune, and a literal
// position consumes one byte.
func globMatch(pattern, path string) bool {
	for len(pattern) > 0 {
		switch {
		case strings.HasPrefix(pattern, "**"):
			return matchDoubleStar(strings.TrimPrefix(pattern, "**"), path)
		case pattern[0] == '*':
			return matchSingleStar(pattern[1:], path)
		case pattern[0] == '?':
			n, ok := matchSingleRune(path)
			if !ok {
				return false
			}
			pattern = pattern[1:]
			path = path[n:]
		default:
			// A literal pattern byte must equal the next byte of path.
			// Comparing byte-by-byte is rune-correct because both sides are
			// UTF-8: a full rune matches iff all of its bytes match, and "/"
			// (0x2F) never appears inside a multi-byte rune, so a "/" is only
			// ever consumed by a literal "/".
			if len(path) == 0 || pattern[0] != path[0] {
				return false
			}
			pattern = pattern[1:]
			path = path[1:]
		}
	}
	return len(path) == 0
}

// matchSingleRune reports the byte length of path's leading rune and whether a
// "?" wildcard may consume it. "?" matches exactly one rune except "/", so an
// empty path or a leading "/" is rejected. Invalid UTF-8 decodes to a
// single-byte RuneError, so "?" still advances by one byte and never stalls.
func matchSingleRune(path string) (int, bool) {
	if len(path) == 0 || path[0] == '/' {
		return 0, false
	}
	_, n := utf8.DecodeRuneInString(path)
	return n, true
}

// matchDoubleStar matches a "**" wildcard against path, where rest is the
// pattern that follows the "**". A "**" spans path separators: "**/<inner>"
// skips whole leading segments, a trailing "**" matches the remainder, and
// "**" before a non-"/" literal tries every suffix of path.
func matchDoubleStar(rest, path string) bool {
	// A leading "/" after "**" consumes an arbitrary number of path segments,
	// including zero, before matching the inner pattern.
	if strings.HasPrefix(rest, "/") {
		return matchDoubleStarSegment(rest[1:], path)
	}
	// Trailing "**" matches the rest of path.
	if rest == "" {
		return true
	}
	// "**" followed by a non-"/" literal: try every suffix.
	for i := 0; i <= len(path); i++ {
		if globMatch(rest, path[i:]) {
			return true
		}
	}
	return false
}

// matchDoubleStarSegment matches inner (the pattern after a "**/") against
// path, allowing inner to start at the beginning of path (zero skipped
// segments) or immediately after any "/" separator.
func matchDoubleStarSegment(inner, path string) bool {
	// "**/" matches zero segments.
	if globMatch(inner, path) {
		return true
	}
	for i := 0; i < len(path); i++ {
		if path[i] == '/' && globMatch(inner, path[i+1:]) {
			return true
		}
	}
	return false
}

// matchSingleStar matches a "*" wildcard against path, where rest is the
// pattern that follows the "*". A "*" matches a run of non-"/" characters,
// including the empty run, but never crosses a separator.
func matchSingleStar(rest, path string) bool {
	for i := 0; i <= len(path); i++ {
		if i > 0 && path[i-1] == '/' {
			return false
		}
		if globMatch(rest, path[i:]) {
			return true
		}
	}
	return false
}

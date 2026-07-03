package main

import "regexp"

// stateAPILastMessageMaxRunes bounds the `last_message` field returned by
// GET /api/v1/state. SPEC §15.3 forbids leaking tokens/secrets through
// orchestrator surfaces, and the underlying Codex `LastMessage` is a
// passthrough of arbitrary agent / tool output (issue body text, tool
// responses, error strings). Truncating before serialization keeps the
// loopback-only status surface from incidentally echoing screen-fulls of
// agent text into operator dashboards / screenshares.
const stateAPILastMessageMaxRunes = 256

// stateAPIRedactionPatterns is the small set of secret-shaped substrings
// that this redaction layer scrubs before emitting `last_message` on
// /api/v1/state. The list is intentionally narrow — this is only the
// last-line guard against incidental loopback exposure of common token
// shapes that Codex notifications can echo (Authorization headers, raw
// bearer tokens, basic-auth URLs, sk-/ghp_/ghu_-prefixed keys).
var stateAPIRedactionPatterns = []*regexp.Regexp{
	// Authorization header values: match the full header span up to a
	// natural terminator (CR, LF, or ";") so we redact both the scheme
	// (Bearer/Basic) and the credential payload in one substitution.
	regexp.MustCompile(`(?i)\bauthorization:\s*[^\r\n;]+`),
	// Stand-alone bearer tokens that appear outside an Authorization
	// header (Codex notifications sometimes echo the scheme + token
	// without the surrounding header context).
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._\-+/=]+`),
	regexp.MustCompile(`\b(?:sk|ghp|ghu|ghs|gho|github_pat|glpat|xoxb|xoxp)[_-][A-Za-z0-9_\-]{16,}`),
	// Any URL carrying userinfo — with or without a password. The bare
	// `https://token@host` clone URL shape is still a credential, and the
	// userinfo class is greedy up to the last @ before the authority ends so
	// a password containing @ does not leak its tail.
	regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9+.\-]*://[^/?#\s]*@[^\s]+`),
}

// redactStateAPILastMessage applies the SPEC §15.3 / issue #297 redaction
// pass used when projecting orchestrator.RunningView.LastMessage onto the
// /api/v1/state `last_message` field. The function is total: empty input
// returns empty output; short, clean input is returned unchanged.
//
// Behavior:
//  1. Pattern-scrub common token shapes (Authorization headers, bearer
//     tokens, sk-/ghp_-prefixed API keys, embedded basic-auth URLs). Each
//     match is replaced verbatim with "<redacted>" so the surrounding
//     prose remains readable.
//  2. Truncate to stateAPILastMessageMaxRunes runes (not bytes — UTF-8
//     safe) and append a U+2026 horizontal ellipsis so consumers can
//     tell a truncated message from a naturally-short one.
//
// Pattern-scrubbing happens before truncation so a token straddling the
// 256-rune boundary is still redacted.
func redactStateAPILastMessage(s string) string {
	if s == "" {
		return ""
	}
	s = redactStateAPISecretText(s)
	runes := []rune(s)
	if len(runes) > stateAPILastMessageMaxRunes {
		return string(runes[:stateAPILastMessageMaxRunes]) + "…"
	}
	return s
}

func redactStateAPISecretText(s string) string {
	if s == "" {
		return ""
	}
	for _, p := range stateAPIRedactionPatterns {
		s = p.ReplaceAllString(s, "<redacted>")
	}
	return s
}

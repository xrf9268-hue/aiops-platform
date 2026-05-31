package runner

import "strings"

// sandboxStartupSignatures are substrings that mark a codex/bwrap sandbox
// startup denial in the agent's failure reason or captured output. The failure
// originates inside codex's vendored bwrap subprocess, not our code, so there is
// no typed error to match on — text classification of the external tool's output
// is the only signal. This mirrors how the doctor preflight probe classifies the
// same condition (#542) and how the quota-backoff path parses codex's message
// text (codex_app_server_turn.go). The list is deliberately narrow to the
// bubblewrap user-namespace setup phrases so an unrelated "permission denied" in
// ordinary agent output cannot trip the cooldown. Earned by the #542 blast
// radius: ~590k input tokens burned per failed turn, repeated every poll.
var sandboxStartupSignatures = []string{
	"setting up uid map",
	"setting up gid map",
	"creating new user namespace",
}

// textHasSandboxStartupFailure reports whether any part contains a known
// sandbox-startup signature (case-insensitive).
func textHasSandboxStartupFailure(parts ...string) bool {
	for _, p := range parts {
		if p == "" {
			continue
		}
		lower := strings.ToLower(p)
		for _, sig := range sandboxStartupSignatures {
			if strings.Contains(lower, sig) {
				return true
			}
		}
	}
	return false
}

// classifySandboxStartupFailure re-tags an otherwise-generic run failure as a
// *SandboxStartupError when the classified error or captured output shows a
// codex/bwrap sandbox-startup denial that will recur identically until the host
// is reconfigured (#550). The more specific outcomes (timeout, stall, read
// timeout, turn timeout, input-required, quota backoff) keep their own routing:
// they carry distinct worker handling, and none of them can legitimately contain
// the bubblewrap user-namespace phrases. The returned error carries only a
// fixed, output-free detail so raw captured subprocess text never leaks into an
// error string.
func classifySandboxStartupFailure(err error, res Result) error {
	if err == nil || IsSandboxStartup(err) {
		return err
	}
	if IsTimeout(err) || IsStall(err) || IsReadTimeout(err) || IsTurnTimeout(err) || IsInputRequired(err) || IsQuotaBackoff(err) {
		return err
	}
	if !textHasSandboxStartupFailure(err.Error(), res.OutputHead, res.OutputTail) {
		return err
	}
	return &SandboxStartupError{Detail: "host denied the codex bwrap user namespace"}
}

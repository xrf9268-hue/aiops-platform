package main

import (
	"strings"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/orchestrator"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
)

func TestRedactStateAPILastMessageEmptyReturnsEmpty(t *testing.T) {
	if got := redactStateAPILastMessage(""); got != "" {
		t.Errorf("empty input: got %q, want \"\"", got)
	}
}

func TestRedactStateAPILastMessagePreservesShortCleanString(t *testing.T) {
	in := "agent completed turn 3"
	if got := redactStateAPILastMessage(in); got != in {
		t.Errorf("clean short input: got %q, want %q", got, in)
	}
}

func TestRedactStateAPILastMessageScrubsBearerToken(t *testing.T) {
	in := "request failed: Bearer abc123token456 (401)"
	got := redactStateAPILastMessage(in)
	if strings.Contains(got, "abc123token456") {
		t.Errorf("bearer token leaked: %q", got)
	}
	if !strings.Contains(got, "<redacted>") {
		t.Errorf("expected <redacted> marker in: %q", got)
	}
}

func TestRedactStateAPILastMessageScrubsAuthorizationHeader(t *testing.T) {
	in := "outgoing header set: Authorization: Bearer xyz789 ; retrying"
	got := redactStateAPILastMessage(in)
	if strings.Contains(got, "xyz789") {
		t.Errorf("auth header value leaked: %q", got)
	}
	if !strings.Contains(got, "<redacted>") {
		t.Errorf("expected <redacted> marker in: %q", got)
	}
}

func TestRedactStateAPILastMessageScrubsSecretPrefixedKeys(t *testing.T) {
	cases := []string{
		"sk-1234567890abcdef1234567890ABCDEF",
		"ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"github_pat_aaaaaaaaaaaa_aaaaaaaaaaaaaaaaaa",
		"glpat-aaaaaaaaaaaaaaaaaaaa",
	}
	for _, tok := range cases {
		in := "tool output: api_key=" + tok + " more text"
		got := redactStateAPILastMessage(in)
		if strings.Contains(got, tok) {
			t.Errorf("secret-prefixed key leaked for %q: %q", tok, got)
		}
	}
}

func TestRedactStateAPILastMessageScrubsBasicAuthURL(t *testing.T) {
	in := "fetch https://user:p@ssw0rd@example.com/repo failed"
	got := redactStateAPILastMessage(in)
	if strings.Contains(got, "p@ssw0rd") {
		t.Errorf("basic-auth URL leaked: %q", got)
	}
}

func TestRedactStateAPILastMessageTruncatesLongInput(t *testing.T) {
	in := strings.Repeat("x", stateAPILastMessageMaxRunes+50)
	got := redactStateAPILastMessage(in)
	runes := []rune(got)
	if len(runes) != stateAPILastMessageMaxRunes+1 {
		t.Errorf("rune length: got %d, want %d (+ ellipsis)", len(runes), stateAPILastMessageMaxRunes+1)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("missing ellipsis suffix in truncated output: %q", got[len(got)-8:])
	}
}

// TestRedactStateAPILastMessageRedactsBeforeTruncation guards the ordering
// invariant: a token that straddles the 256-rune truncation boundary must
// still be redacted (otherwise truncating first would chop the token and
// leave a partial credential in the output).
func TestRedactStateAPILastMessageRedactsBeforeTruncation(t *testing.T) {
	prefix := strings.Repeat("a", stateAPILastMessageMaxRunes-10)
	in := prefix + "Bearer abcdef1234567890tail"
	got := redactStateAPILastMessage(in)
	if strings.Contains(got, "abcdef1234567890") {
		t.Errorf("bearer token leaked across truncation boundary: %q", got)
	}
}

// TestAPIStateFromViewScrubsErrorFields drives the production projection
// (apiStateFromView) with retry/blocked rows whose error strings embed a
// credentialed clone URL, pinning issue #1032: `Retry.Error`, `Blocked.Error`,
// startup-failure errors, and runtime event messages must pass through the
// same secret scrub as `last_message`. Deleting the redactStateAPIErrorText
// call at any of these seams leaks the token and fails the assertion.
func TestAPIStateFromViewScrubsErrorFields(t *testing.T) {
	const secretErr = "clone failed: fatal: unable to access 'https://bot:hunter2token@git.example.com/org/repo.git/'"
	view := orchestrator.StateView{
		Retrying: []orchestrator.RetryView{{
			IssueID:        "ENG-1",
			Identifier:     "ENG-1",
			Attempt:        2,
			Error:          secretErr,
			StartupFailure: &task.StartupFailure{Phase: "workspace", Error: secretErr},
		}},
		Blocked: []orchestrator.BlockedView{{
			IssueID:    "ENG-2",
			Identifier: "ENG-2",
			Method:     "budget",
			Error:      secretErr,
		}},
		RecentEvents: []orchestrator.RuntimeEvent{{
			Kind:       orchestrator.RuntimeEventFailed,
			IssueID:    "ENG-1",
			Identifier: "ENG-1",
			Message:    secretErr,
		}},
	}
	resp := apiStateFromView(view)
	if len(resp.Retrying) != 1 || len(resp.Blocked) != 1 {
		t.Fatalf("apiStateFromView rows = %d retrying / %d blocked; want 1 / 1", len(resp.Retrying), len(resp.Blocked))
	}
	assertScrubbed := func(field, got string) {
		t.Helper()
		if strings.Contains(got, "hunter2token") {
			t.Errorf("%s leaked credential: %q", field, got)
		}
		if !strings.Contains(got, "<redacted>") {
			t.Errorf("%s missing <redacted> marker: %q", field, got)
		}
	}
	assertScrubbed("Retry.Error", resp.Retrying[0].Error)
	if resp.Retrying[0].StartupFailure == nil {
		t.Fatalf("Retry.StartupFailure = nil; want projected value")
	}
	assertScrubbed("Retry.StartupFailure.Error", resp.Retrying[0].StartupFailure.Error)
	assertScrubbed("Blocked.Error", resp.Blocked[0].Error)
}

// TestAPIIssueFromViewScrubsLastError pins the per-issue endpoint's
// `last_error` projection for a retrying issue (issue #1032).
func TestAPIIssueFromViewScrubsLastError(t *testing.T) {
	const secretErr = "push rejected: https://bot:hunter2token@git.example.com/org/repo.git"
	view := orchestrator.StateView{
		Retrying: []orchestrator.RetryView{{IssueID: "ENG-3", Identifier: "ENG-3", Attempt: 1, Error: secretErr}},
	}
	payload, ok := apiIssueFromView(view, "ENG-3")
	if !ok {
		t.Fatalf("apiIssueFromView(ENG-3) not found")
	}
	if payload.LastError == nil {
		t.Fatalf("LastError = nil; want scrubbed error")
	}
	if strings.Contains(*payload.LastError, "hunter2token") {
		t.Errorf("last_error leaked credential: %q", *payload.LastError)
	}
	if !strings.Contains(*payload.LastError, "<redacted>") {
		t.Errorf("last_error missing <redacted> marker: %q", *payload.LastError)
	}
}

func TestRedactStateAPILastMessageIsUTF8Safe(t *testing.T) {
	// Three-byte runes filled to 200 runes; pure rune slicing must not
	// split a codepoint mid-byte (which []byte truncation would).
	in := strings.Repeat("漢", stateAPILastMessageMaxRunes+5)
	got := redactStateAPILastMessage(in)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("missing ellipsis on truncated multibyte: last bytes=%x", got[len(got)-3:])
	}
	// Output should be valid UTF-8: re-decoding to runes should round-trip.
	for _, r := range got {
		if r == '�' {
			t.Errorf("truncation produced invalid UTF-8 replacement rune in: %q", got)
		}
	}
}

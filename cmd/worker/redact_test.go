package main

import (
	"strings"
	"testing"
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

func TestRedactStateAPISecretTextScrubsBareUserinfoURL(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantLeak string
		wantKept string
	}{
		{
			name:     "bare userinfo token is scrubbed",
			in:       "fatal: unable to access 'https://oauth2supersecret@git.example.com/org/repo.git/'",
			wantLeak: "oauth2supersecret",
		},
		{
			name:     "plain URL without userinfo is preserved",
			in:       "see https://example.com/docs for details",
			wantKept: "https://example.com/docs",
		},
		{
			name:     "email after a URL is not treated as userinfo",
			in:       "docs at https://example.com then mail ops@example.com",
			wantKept: "ops@example.com",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := redactStateAPISecretText(tt.in)
			if tt.wantLeak != "" && strings.Contains(got, tt.wantLeak) {
				t.Errorf("redactStateAPISecretText(%q) = %q; leaked %q", tt.in, got, tt.wantLeak)
			}
			if tt.wantKept != "" && !strings.Contains(got, tt.wantKept) {
				t.Errorf("redactStateAPISecretText(%q) = %q; want %q preserved", tt.in, got, tt.wantKept)
			}
		})
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

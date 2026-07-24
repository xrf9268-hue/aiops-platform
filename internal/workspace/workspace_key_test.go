package workspace

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestIssueWorkspaceKeyAddsStableHashOnlyWhenIdentifierChanges(t *testing.T) {
	t.Parallel()

	const safe = "team_a-1"
	if got := IssueWorkspaceKey(safe); got != safe {
		t.Fatalf("IssueWorkspaceKey(%q) = %q, want unchanged %q", safe, got, safe)
	}
	safeAtLimit := strings.Repeat("A", maxSanitizedLength)
	if got := IssueWorkspaceKey(safeAtLimit); got != safeAtLimit {
		t.Fatalf("IssueWorkspaceKey(safe limit) = %q, want unchanged %q", got, safeAtLimit)
	}

	const unsafe = "team/a-1"
	const want = "team_a-1--7c03d5a56ca4204b"
	if got := IssueWorkspaceKey(unsafe); got != want {
		t.Fatalf("IssueWorkspaceKey(%q) = %q, want %q", unsafe, got, want)
	}

	const spaced = " team/a-1 "
	const wantSpaced = "_team_a-1_--84f3056afc8b3688"
	if got := IssueWorkspaceKey(spaced); got != wantSpaced {
		t.Fatalf("IssueWorkspaceKey(%q) = %q, want exact-original hash %q", spaced, got, wantSpaced)
	}
	if got, tabbed := IssueWorkspaceKey(spaced), IssueWorkspaceKey("\tteam/a-1\t"); got == tabbed {
		t.Fatalf("IssueWorkspaceKey exact-original collision = %q for space- and tab-delimited identifiers", got)
	}
}

func TestIssueWorkspaceKeyDisambiguatesTruncatedIdentifiers(t *testing.T) {
	t.Parallel()

	first := strings.Repeat("A", maxSanitizedLength) + "X"
	second := strings.Repeat("A", maxSanitizedLength) + "Y"
	firstKey := IssueWorkspaceKey(first)
	secondKey := IssueWorkspaceKey(second)

	if firstKey == secondKey {
		t.Fatalf("IssueWorkspaceKey(overlong identifiers) collided at %q", firstKey)
	}
	valid := regexp.MustCompile(`^[A-Za-z0-9._-]+--[0-9a-f]{16}$`)
	unicodeKey := IssueWorkspaceKey(strings.Repeat("界", maxSanitizedLength+1))
	for _, key := range []string{firstKey, secondKey, unicodeKey} {
		if !utf8.ValidString(key) || len([]rune(key)) > maxSanitizedLength {
			t.Fatalf("IssueWorkspaceKey(overlong identifier) = %q (runes=%d valid=%v), want at most %d valid runes", key, len([]rune(key)), utf8.ValidString(key), maxSanitizedLength)
		}
		if !valid.MatchString(key) {
			t.Fatalf("IssueWorkspaceKey(overlong identifier) = %q, want allowed prefix plus 16 lowercase hex characters", key)
		}
	}
}

package tracker

import (
	"reflect"
	"testing"
	"time"
)

func TestIssueRefsFromIDsTrimsAndSkipsEmptyIDs(t *testing.T) {
	got := IssueRefsFromIDs([]string{" 123 ", "", " \t\n", "#7 "})
	want := []IssueRef{{ID: "123"}, {ID: "#7"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("IssueRefsFromIDs = %#v, want %#v", got, want)
	}
}

func TestTimeStringReturnsEmptyForZeroTime(t *testing.T) {
	if got := TimeString(time.Time{}); got != "" {
		t.Fatalf("TimeString(zero) = %q, want empty string", got)
	}
}

func TestTimeStringNormalizesOffsetsToUTC(t *testing.T) {
	input, err := time.Parse(time.RFC3339Nano, "2026-05-08T12:30:00.123456789+02:00")
	if err != nil {
		t.Fatalf("parse test time: %v", err)
	}
	if got := TimeString(input); got != "2026-05-08T10:30:00.123456789Z" {
		t.Fatalf("TimeString(offset time) = %q, want UTC RFC3339Nano", got)
	}
}

// TestBlockedByNonTerminal pins the shared SPEC §8.2 "is this blocker open"
// predicate's three edges (#750): empty/unknown state blocks, a non-terminal
// state blocks, and an all-terminal set does not.
func TestBlockedByNonTerminal(t *testing.T) {
	terminal := map[string]struct{}{"done": {}, "canceled": {}}
	cases := []struct {
		name      string
		blockedBy []BlockerRef
		want      bool
	}{
		{name: "nil blockers do not block", blockedBy: nil, want: false},
		{name: "empty blockers do not block", blockedBy: []BlockerRef{}, want: false},
		{name: "all terminal does not block", blockedBy: []BlockerRef{{State: "Done"}, {State: " canceled "}}, want: false},
		{name: "non-terminal blocks", blockedBy: []BlockerRef{{State: "Done"}, {State: "In Progress"}}, want: true},
		{name: "empty state blocks", blockedBy: []BlockerRef{{State: ""}}, want: true},
		{name: "whitespace state blocks", blockedBy: []BlockerRef{{State: "   "}}, want: true},
	}
	for _, tc := range cases {
		if got := BlockedByNonTerminal(tc.blockedBy, terminal); got != tc.want {
			t.Fatalf("BlockedByNonTerminal(%+v) = %t; want %t (%s)", tc.blockedBy, got, tc.want, tc.name)
		}
	}
}

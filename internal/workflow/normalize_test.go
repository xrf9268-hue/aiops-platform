package workflow

import (
	"reflect"
	"testing"
)

// TestNormalizeStateConcurrencyLimits_DropsEmptyKeyAndNonPositive pins
// the closing-#294 decision recorded in the helper's docstring: empty /
// whitespace-only keys and non-positive limits are dropped, not
// preserved. The earlier loader.go variant preserved them and the
// orchestrator's variant dropped them — that divergence is what #294
// asked us to resolve, and resolving it in favor of "drop" is what the
// orchestrator's runtime lookup path needs (it never produces an empty
// key from a real tracker state, so a preserved entry would be dead).
func TestNormalizeStateConcurrencyLimits_DropsEmptyKeyAndNonPositive(t *testing.T) {
	in := map[string]int{
		"":            5,  // empty key → drop
		"   ":         3,  // whitespace key → drop
		"In Progress": 0,  // zero limit → drop
		"Review":      -2, // negative limit → drop
		"Triage":      2,  // keep, normalized
		"in progress": 4,  // case-folded into in_progress
	}
	got := NormalizeStateConcurrencyLimits(in)
	want := map[string]int{
		"triage":      2,
		"in_progress": 4,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NormalizeStateConcurrencyLimits diverges from #294 decision:\n got  %#v\n want %#v", got, want)
	}
}

func TestNormalizeStateConcurrencyLimits_NilIn_NilOut(t *testing.T) {
	if got := NormalizeStateConcurrencyLimits(nil); got != nil {
		t.Errorf("nil input: got %#v, want nil", got)
	}
	if got := NormalizeStateConcurrencyLimits(map[string]int{}); got != nil {
		t.Errorf("empty input: got %#v, want nil", got)
	}
}

// TestNormalizeStateConcurrencyLimits_LoadReloadParity is the SPEC §6.2
// reload-vs-load shape parity the issue described: the loader's
// initial-load path and the orchestrator's snapshot-build path must
// produce a bit-identical map for any input. Before #294 the two
// diverged on empty / non-positive entries; after consolidation they
// both call this helper, so a fixture containing every edge input
// round-trips identically.
func TestNormalizeStateConcurrencyLimits_LoadReloadParity(t *testing.T) {
	fixture := map[string]int{
		"":            7,
		"In Progress": 0,
		"  rework  ":  3,
		"Review":      5,
		"REVIEW":      9, // duplicates Review after case-fold — last write wins
	}
	first := NormalizeStateConcurrencyLimits(fixture)
	second := NormalizeStateConcurrencyLimits(first)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("load/reload parity violated:\n load   %#v\n reload %#v", first, second)
	}
	// Round-trip stability: applying the normalization to its own
	// output is a no-op (idempotence), guarding against future edits
	// that re-introduce a transform that depends on canonicalization.
	third := NormalizeStateConcurrencyLimits(second)
	if !reflect.DeepEqual(second, third) {
		t.Fatalf("normalize is not idempotent:\n once  %#v\n twice %#v", second, third)
	}
}

func TestNormalizeStateConcurrencyKey_Shape(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"In Progress", "in_progress"},
		{"  in progress ", "in_progress"},
		{"REWORK", "rework"},
		{"", ""},
		{"   ", ""},
		{"AI Ready", "ai_ready"},
	}
	for _, tc := range cases {
		if got := NormalizeStateConcurrencyKey(tc.in); got != tc.want {
			t.Errorf("NormalizeStateConcurrencyKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

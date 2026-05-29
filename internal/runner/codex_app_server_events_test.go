package runner

// codex_app_server_events_test.go pins toSnakeCase at its own function boundary
// before #499 decomposes its 17-cognitive-complexity loop into per-decision
// helpers. toSnakeCase normalizes Codex JSON-RPC payload keys (camelCase /
// acronym) to the snake_case the runtime-event surface emits, so its
// word-boundary rules must hold identically across the refactor.
//
// The subprocess suite only samples specific normalized keys; this exercises
// the boundary rules directly, including the ASCII byte-oriented lookahead the
// implementation relies on (Codex keys are ASCII camelCase).

import "testing"

func TestToSnakeCase(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"thread", "thread"},
		{"user_id", "user_id"}, // already snake: underscore passes through unchanged

		// camelCase boundary: underscore before an uppercase preceded by lower.
		{"threadId", "thread_id"},
		{"callId", "call_id"},
		{"lastAssistantMessage", "last_assistant_message"},
		{"retryAfterSeconds", "retry_after_seconds"},

		// camelCase boundary where the boundary uppercase is the final byte: the
		// underscore comes from the lower-prev branch, independent of the
		// (out-of-range, don't-care) lookahead.
		{"fooB", "foo_b"},

		// digit -> uppercase boundary.
		{"a1B", "a1_b"},
		{"fooBar2Baz", "foo_bar2_baz"},

		// acronym -> word boundary: underscore before the last uppercase of a
		// run when the next rune is lowercase.
		{"HTTPServer", "http_server"},
		{"ABc", "a_bc"},

		// pure acronym / trailing uppercase run: no internal underscore.
		{"HTTP", "http"},
		{"ID", "id"},
		{"AB", "ab"},

		// leading uppercase never gets a prefix underscore.
		{"A", "a"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := toSnakeCase(tt.in); got != tt.want {
				t.Errorf("toSnakeCase(%q) = %q; want %q", tt.in, got, tt.want)
			}
		})
	}
}

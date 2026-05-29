package runner

import "testing"

// The four scanner primitives are the shared lexer steps behind the SPEC
// §15.5 mutation gate (parseLinearGraphQLOperation + countGraphQLOperations).
// These tests pin the exact byte offset each primitive returns on the tricky
// inputs the inline code used to handle — escaped quotes, block strings,
// unterminated literals, and end-of-input — so a future edit to one scanner
// cannot silently change where the lexer resumes.

func TestSkipGraphQLLineComment(t *testing.T) {
	tests := []struct {
		name  string
		query string
		start int
		want  int // offset of the terminating newline, or len(query)
	}{
		{"stops at LF", "#abc\nx", 0, 4},
		{"stops at CR", "#abc\rx", 0, 4},
		{"runs to EOF without newline", "#abc", 0, 4},
		{"bare hash at EOF", "#", 0, 1},
		{"comment after content", "q #c\n", 2, 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := skipGraphQLLineComment(tc.query, tc.start); got != tc.want {
				t.Fatalf("skipGraphQLLineComment(%q, %d) = %d; want %d", tc.query, tc.start, got, tc.want)
			}
		})
	}
}

func TestSkipGraphQLString(t *testing.T) {
	tests := []struct {
		name  string
		query string
		start int
		want  int // offset just past the closing quote, or len(query)
	}{
		{"plain string", `"abc"x`, 0, 5},
		{"escaped quote does not terminate", `"a\"b"`, 0, 6},
		{"unterminated plain string", `"abc`, 0, 4},
		{"block string", `"""a"b"""x`, 0, 9},
		{"unterminated block string", `"""abc`, 0, 6},
		{"empty plain string", `""y`, 0, 2},
		{"empty block string", `""""""z`, 0, 6},
		// A trailing unescaped backslash consumes the (missing) next byte,
		// over-advancing one past EOF. This matches the original inline
		// behavior and is harmless: every caller guards on `i < len(query)`.
		{"trailing backslash over-advances past EOF", "\"ab\\", 0, 5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := skipGraphQLString(tc.query, tc.start); got != tc.want {
				t.Fatalf("skipGraphQLString(%q, %d) = %d; want %d", tc.query, tc.start, got, tc.want)
			}
		})
	}
}

func TestSkipGraphQLVariable(t *testing.T) {
	tests := []struct {
		name  string
		query string
		start int
		want  int // offset just past the variable name
	}{
		{"variable then space", "$foo bar", 0, 4},
		{"variable with digits", "$a1b2)", 0, 5},
		{"bare dollar at EOF", "$", 0, 1},
		{"bare dollar then non-name", "$(x", 0, 1},
		{"variable runs to EOF", "$name", 0, 5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := skipGraphQLVariable(tc.query, tc.start); got != tc.want {
				t.Fatalf("skipGraphQLVariable(%q, %d) = %d; want %d", tc.query, tc.start, got, tc.want)
			}
		})
	}
}

// TestParseLinearGraphQLOperationShorthandQueryAfterFragment pins the
// openBrace default branch: a bare `{ ... }` selection set following a
// fragment definition is a shorthand query, and its first top-level field is
// captured. That branch is otherwise reached only transitively, so this case
// makes the §15.5-gate-relevant behavior explicit and guards the FieldName==""
// condition the branch depends on.
func TestParseLinearGraphQLOperationShorthandQueryAfterFragment(t *testing.T) {
	const query = "fragment F on T { id } { viewer { id } }"
	got := parseLinearGraphQLOperation(query)
	if got.Kind != linearGraphQLOperationQuery || got.FieldName != "viewer" {
		t.Fatalf("parseLinearGraphQLOperation(%q) = {Kind:%q FieldName:%q}; want {Kind:%q FieldName:%q}",
			query, got.Kind, got.FieldName, linearGraphQLOperationQuery, "viewer")
	}
}

func TestScanGraphQLName(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		start    int
		wantName string
		wantNext int
	}{
		{"name then paren", "foo(x)", 0, "foo", 3},
		{"underscore and digits", "_a1 ", 0, "_a1", 3},
		{"name runs to EOF", "issueUpdate", 0, "issueUpdate", 11},
		{"single char name", "q ", 0, "q", 1},
		{"name after offset", "{ field }", 2, "field", 7},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotName, gotNext := scanGraphQLName(tc.query, tc.start)
			if gotName != tc.wantName || gotNext != tc.wantNext {
				t.Fatalf("scanGraphQLName(%q, %d) = (%q, %d); want (%q, %d)", tc.query, tc.start, gotName, gotNext, tc.wantName, tc.wantNext)
			}
		})
	}
}

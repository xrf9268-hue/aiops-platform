package runner

import "testing"

// FuzzParseLinearGraphQLOperation guards the hand-written Linear GraphQL
// operation parser that feeds the SPEC §15.5 mutation gate. A mis-extracted
// operation kind or field name routes a call down the wrong permission
// branch (#411), so the target asserts more than "does not panic": Kind is
// always a known operation kind, and a non-empty FieldName is always a
// structurally valid GraphQL name. countGraphQLOperations shares the same
// lexer state machine on the same gate path, so it is exercised on the same
// corpus and checked for a non-negative count.
func FuzzParseLinearGraphQLOperation(f *testing.F) {
	seeds := []string{
		"",
		"query Q { issue { id } }",
		"mutation M($i: String!) { issueUpdate(id: $i) { issue { id } } }",
		"fragment F on Issue { id }",
		"query { __typename }",
		"{ viewer { id } }",
		"subscription S { issues { id } }",
		"# comment\nquery Q { issue { id } }",
		`query Q { issue(filter: "a{b}c") { id } }`,
		`mutation { commentCreate(input: {body: "}{"}) { success } }`,
		`query Q { a """block { } string""" b }`,
		"fragment F on Issue { id } mutation M { issueUpdate(id: 1) { success } }",
		`query Q($x: String = "\"") { issue { id } }`,
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		op := parseLinearGraphQLOperation(src)
		switch op.Kind {
		case linearGraphQLOperationQuery, linearGraphQLOperationMutation, linearGraphQLOperationSubscription:
		default:
			t.Fatalf("parseLinearGraphQLOperation(%q).Kind = %q; want query, mutation, or subscription", src, op.Kind)
		}
		if op.FieldName != "" {
			if !isGraphQLNameStart(op.FieldName[0]) {
				t.Fatalf("parseLinearGraphQLOperation(%q).FieldName = %q; first byte is not a valid GraphQL name start", src, op.FieldName)
			}
			for i := 0; i < len(op.FieldName); i++ {
				if !isGraphQLNameContinue(op.FieldName[i]) {
					t.Fatalf("parseLinearGraphQLOperation(%q).FieldName = %q; byte %d is not a valid GraphQL name char", src, op.FieldName, i)
				}
			}
		}
		if n := countGraphQLOperations(src); n < 0 {
			t.Fatalf("countGraphQLOperations(%q) = %d; want >= 0", src, n)
		}
	})
}

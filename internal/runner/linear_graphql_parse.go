package runner

import "strings"

// linearGraphQLOperationKind names the GraphQL top-level operation kinds
// the runner discriminates between when applying the SPEC §15.5 mutation
// gate. Subscriptions are recognized so the gate can reject them with a
// typed error instead of letting them slip past the read-only check (the
// runner has no streaming surface for them either).
type linearGraphQLOperationKind string

const (
	linearGraphQLOperationQuery        linearGraphQLOperationKind = "query"
	linearGraphQLOperationMutation     linearGraphQLOperationKind = "mutation"
	linearGraphQLOperationSubscription linearGraphQLOperationKind = "subscription"
)

// linearGraphQLOperation is the minimal parse the gate needs: the operation
// kind plus the first selected top-level field name (e.g. "issueDelete",
// "commentCreate"). FieldName is empty when the parser could not isolate a
// single top-level field; in that case the gate falls back to denying
// mutations with an "unidentified mutation" reason rather than guessing.
type linearGraphQLOperation struct {
	Kind      linearGraphQLOperationKind
	FieldName string
}

// The four scanner primitives below are the shared lexer skip/scan steps that
// both parseLinearGraphQLOperation and countGraphQLOperations run on the same
// SPEC §15.5 gate path. They were hand-inlined identically in both functions
// before #410; extracting them keeps a single source of truth for the
// comment/string/variable/name shapes the gate must tolerate, so a fix to one
// (e.g. a new escape rule) can never drift between the two scanners. Each
// takes the byte offset at the construct's opening byte and returns the offset
// just past it; none validates semantics — that is Linear's job on dispatch.

// skipGraphQLLineComment advances past a `# ... ` comment. The caller has
// already confirmed query[i] == '#'. It stops at the terminating newline (or
// end of input) and leaves the newline for the main loop to consume as
// whitespace, matching the original inline behavior.
func skipGraphQLLineComment(query string, i int) int {
	for i < len(query) && query[i] != '\n' && query[i] != '\r' {
		i++
	}
	return i
}

// skipGraphQLString advances past a string literal. The caller has already
// confirmed query[i] == '"'. It handles both `"""block"""` strings and
// ordinary `"..."` strings, honoring `\`-escaped bytes in the latter so an
// escaped quote does not falsely terminate the string. An unterminated string
// consumes to end of input.
func skipGraphQLString(query string, i int) int { //nolint:gocognit // baseline (#521)
	if strings.HasPrefix(query[i:], `"""`) {
		i += 3
		for i < len(query) && !strings.HasPrefix(query[i:], `"""`) {
			i++
		}
		if i < len(query) {
			i += 3
		}
		return i
	}
	i++
	for i < len(query) {
		if query[i] == '\\' {
			i += 2
			continue
		}
		if query[i] == '"' {
			i++
			break
		}
		i++
	}
	return i
}

// skipGraphQLVariable advances past a `$name` variable reference. The caller
// has already confirmed query[i] == '$'. A bare `$` with no following name
// bytes simply consumes the `$`.
func skipGraphQLVariable(query string, i int) int {
	i++ // consume '$'
	for i < len(query) && isGraphQLNameContinue(query[i]) {
		i++
	}
	return i
}

// scanGraphQLName reads a GraphQL name token. The caller has already confirmed
// isGraphQLNameStart(query[i]). It returns the name and the offset just past
// its last byte.
func scanGraphQLName(query string, i int) (name string, next int) {
	start := i
	i++ // consume the validated name-start byte
	for i < len(query) && isGraphQLNameContinue(query[i]) {
		i++
	}
	return query[start:i], i
}

// linearGraphQLParser is the small state machine behind
// parseLinearGraphQLOperation. Splitting the per-token transitions into
// methods keeps the driver loop short and lets each transition be reasoned
// about — and exercised — in isolation (#410).
type linearGraphQLParser struct {
	op         linearGraphQLOperation
	depth      int
	parenDepth int
	// expecting describes what the next `{` at depth 0 opens:
	//   "operation" — the body of the operation we just identified
	//   "fragment"  — the body of a fragment definition (skip it)
	//   ""          — neither yet; an unannounced `{` becomes a
	//                  shorthand query
	expecting           string
	headerSelectionRoot bool
}

// openBrace transitions on a `{`. At the document root it decides whether the
// body being opened is the selection set whose first top-level field we
// capture, then descends one level.
func (p *linearGraphQLParser) openBrace() {
	if p.depth == 0 && p.parenDepth == 0 {
		switch p.expecting {
		case "operation":
			p.headerSelectionRoot = true
		case "fragment":
			// Entering a fragment body; not the operation we are looking
			// for. Leave headerSelectionRoot false so its inner field name
			// doesn't get captured.
			p.headerSelectionRoot = false
		default:
			// Shorthand `{ ... }` query — no operation keyword was ever
			// encountered. Kind stays query (the zero value).
			if p.op.FieldName == "" {
				p.headerSelectionRoot = true
			}
		}
		p.expecting = ""
	}
	p.depth++
}

// closeBrace transitions on a `}`. Returning to the document root resets the
// selection-root flag so a trailing `mutation { ... }` after a leading
// `fragment F on T { id }` is still parsed.
func (p *linearGraphQLParser) closeBrace() {
	if p.depth > 0 {
		p.depth--
	}
	if p.depth == 0 {
		p.headerSelectionRoot = false
	}
}

// consumeName transitions on a scanned name token. At the document root it
// classifies operation/fragment headers; inside an operation's selection set
// it captures the first top-level field. Once a field is captured op.Kind is
// no longer overridden, so a later operation cannot shadow the one whose body
// we already entered.
func (p *linearGraphQLParser) consumeName(name string) { //nolint:gocognit // baseline (#521)
	switch {
	case p.depth == 0 && p.parenDepth == 0 && p.expecting == "":
		switch name {
		case "query":
			if p.op.FieldName == "" {
				p.op.Kind = linearGraphQLOperationQuery
			}
			p.expecting = "operation"
		case "mutation":
			if p.op.FieldName == "" {
				p.op.Kind = linearGraphQLOperationMutation
			}
			p.expecting = "operation"
		case "subscription":
			if p.op.FieldName == "" {
				p.op.Kind = linearGraphQLOperationSubscription
			}
			p.expecting = "operation"
		case "fragment":
			p.expecting = "fragment"
		}
	case p.headerSelectionRoot && p.depth == 1 && p.parenDepth == 0:
		if p.op.FieldName == "" {
			p.op.FieldName = name
		}
	}
}

// parseLinearGraphQLOperation inspects the agent-supplied query string and
// returns the operation kind plus the first selected top-level field name.
// The parser is structural — it shares its lexer skip/scan steps with
// countGraphQLOperations and stops at the first non-fragment operation
// header — so it is robust against the same comment/string/parameter shapes
// the count check already handles. It does NOT validate that the query is
// semantically well-formed Linear GraphQL; that is Linear's job once the
// request is dispatched.
func parseLinearGraphQLOperation(query string) linearGraphQLOperation {
	p := linearGraphQLParser{op: linearGraphQLOperation{Kind: linearGraphQLOperationQuery}}
	for i := 0; i < len(query); {
		ch := query[i]
		switch ch {
		case '#':
			i = skipGraphQLLineComment(query, i)
			continue
		case '"':
			i = skipGraphQLString(query, i)
			continue
		case '{':
			p.openBrace()
			i++
			continue
		case '}':
			p.closeBrace()
			i++
			continue
		case '(':
			p.parenDepth++
			i++
			continue
		case ')':
			if p.parenDepth > 0 {
				p.parenDepth--
			}
			i++
			continue
		case '\n', '\r', ' ', '\t', ',':
			i++
			continue
		case '$':
			i = skipGraphQLVariable(query, i)
			continue
		}

		if isGraphQLNameStart(ch) {
			var name string
			name, i = scanGraphQLName(query, i)
			p.consumeName(name)
			continue
		}

		i++
	}
	return p.op
}

// countGraphQLOperations counts top-level GraphQL operations (queries,
// mutations, subscriptions; fragment definitions excluded) in query. It
// shares the lexer skip/scan steps with parseLinearGraphQLOperation so the
// gate's "exactly one operation" check tolerates the same comment, string,
// and parameter shapes the operation parse does.
func countGraphQLOperations(query string) int { //nolint:gocognit,funlen // baseline (#521)
	count := 0
	depth := 0
	parenDepth := 0
	operationHeader := false
	for i := 0; i < len(query); {
		ch := query[i]
		switch ch {
		case '#':
			i = skipGraphQLLineComment(query, i)
			continue
		case '"':
			i = skipGraphQLString(query, i)
			continue
		case '{':
			if depth == 0 && parenDepth == 0 {
				if !operationHeader {
					count++
				}
				operationHeader = false
			}
			depth++
			i++
			continue
		case '}':
			if depth > 0 {
				depth--
			}
			i++
			continue
		case '(':
			parenDepth++
			i++
			continue
		case ')':
			if parenDepth > 0 {
				parenDepth--
			}
			i++
			continue
		case '\n', '\r', ' ', '\t', ',':
			i++
			continue
		case '$':
			i = skipGraphQLVariable(query, i)
			continue
		}

		if depth == 0 && parenDepth == 0 && isGraphQLNameStart(ch) {
			var name string
			name, i = scanGraphQLName(query, i)
			if !operationHeader {
				switch name {
				case "query", "mutation", "subscription":
					count++
					operationHeader = true
				case "fragment":
					operationHeader = true
				}
			}
			continue
		}

		i++
	}
	return count
}

func isGraphQLNameStart(ch byte) bool {
	return ch == '_' || (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z')
}

func isGraphQLNameContinue(ch byte) bool {
	return isGraphQLNameStart(ch) || (ch >= '0' && ch <= '9')
}

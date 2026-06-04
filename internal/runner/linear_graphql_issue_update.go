package runner

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var errIssueUpdateNotFound = errors.New("issueUpdate selection not found")

type issueUpdateStateChange struct {
	IssueID string
	StateID string
}

func issueUpdateArgumentTexts(query string) ([]string, error) {
	scanner := issueUpdateScanner{query: query}
	var foundArgs []string
	for scanner.more() {
		args, found, err := scanner.step()
		if err != nil {
			return nil, err
		}
		if found {
			foundArgs = append(foundArgs, args)
		}
	}
	if len(foundArgs) > 0 {
		return foundArgs, nil
	}
	return nil, errIssueUpdateNotFound
}

type issueUpdateScanner struct {
	query      string
	i          int
	depth      int
	parenDepth int
}

func (s *issueUpdateScanner) more() bool {
	return s.i < len(s.query)
}

func (s *issueUpdateScanner) step() (string, bool, error) {
	if next, ok := skipIssueUpdateScanToken(s.query, s.i); ok {
		s.i = next
		return "", false, nil
	}
	if updateIssueUpdateScanDepth(s.query[s.i], &s.depth, &s.parenDepth) {
		s.i++
		return "", false, nil
	}
	args, next, found, err := scanIssueUpdateSelection(s.query, s.i, s.depth, s.parenDepth)
	if next == s.i {
		s.i++
	} else {
		s.i = next
	}
	return args, found, err
}

func skipIssueUpdateScanToken(query string, i int) (int, bool) {
	switch query[i] {
	case '#':
		return skipGraphQLLineComment(query, i), true
	case '"':
		return skipGraphQLString(query, i), true
	default:
		return i, false
	}
}

func updateIssueUpdateScanDepth(ch byte, depth, parenDepth *int) bool {
	switch ch {
	case '{':
		*depth++
	case '}':
		if *depth > 0 {
			*depth--
		}
	case '(':
		*parenDepth++
	case ')':
		if *parenDepth > 0 {
			*parenDepth--
		}
	default:
		return false
	}
	return true
}

func scanIssueUpdateSelection(query string, i, depth, parenDepth int) (string, int, bool, error) {
	if depth != 1 || parenDepth != 0 || !isGraphQLNameStart(query[i]) {
		return "", i, false, nil
	}
	name, next := scanGraphQLName(query, i)
	if name != "issueUpdate" {
		return "", next, false, nil
	}
	args, err := argumentTextAfterName(query, next)
	return args, next, true, err
}

func argumentTextAfterName(query string, i int) (string, error) {
	i = skipGraphQLWhitespace(query, i)
	if i >= len(query) || query[i] != '(' {
		return "", fmt.Errorf("issueUpdate arguments are required")
	}
	end, err := scanGraphQLEnclosedValue(query, i, '(', ')')
	if err != nil {
		return "", err
	}
	return query[i+1 : end-1], nil
}

func parseIssueUpdateArguments(args string, variables map[string]any) (issueUpdateStateChange, error) {
	issueID, err := parseIssueUpdateIssueID(args, variables)
	if err != nil {
		return issueUpdateStateChange{}, err
	}
	stateID, err := parseIssueUpdateStateID(args, variables)
	if err != nil {
		return issueUpdateStateChange{}, err
	}
	return issueUpdateStateChange{IssueID: issueID, StateID: stateID}, nil
}

func parseIssueUpdateIssueID(args string, variables map[string]any) (string, error) {
	raw, found, err := graphQLObjectLikeFieldValueText(args, "id")
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("issueUpdate id is required")
	}
	return parseGraphQLStringLikeValue(raw, variables, "issueUpdate id")
}

func parseIssueUpdateStateID(args string, variables map[string]any) (string, error) {
	raw, found, err := graphQLObjectLikeFieldValueText(args, "input")
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("issueUpdate input.stateId is required")
	}
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "$") {
		return parseIssueUpdateVariableStateID(raw, variables)
	}
	return parseIssueUpdateLiteralStateID(raw, variables)
}

func parseIssueUpdateVariableStateID(raw string, variables map[string]any) (string, error) {
	name, end := scanGraphQLVariableName(raw, 0)
	if end != len(raw) {
		return "", fmt.Errorf("unsupported issueUpdate input value")
	}
	switch input := variables[name].(type) {
	case map[string]any:
		return requiredGraphQLString(input["stateId"], "issueUpdate input.stateId")
	case map[string]string:
		return requiredGraphQLString(input["stateId"], "issueUpdate input.stateId")
	default:
		return "", fmt.Errorf("issueUpdate input.stateId is required")
	}
}

func parseIssueUpdateLiteralStateID(raw string, variables map[string]any) (string, error) {
	if !strings.HasPrefix(raw, "{") {
		return "", fmt.Errorf("issueUpdate input.stateId is required")
	}
	stateRaw, found, err := graphQLObjectLikeFieldValueText(strings.TrimSpace(raw[1:len(raw)-1]), "stateId")
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("issueUpdate input.stateId is required")
	}
	return parseGraphQLStringLikeValue(stateRaw, variables, "issueUpdate input.stateId")
}

func graphQLObjectLikeFieldValueText(input, target string) (string, bool, error) {
	for i := 0; i < len(input); {
		field, ok, err := scanGraphQLObjectLikeField(input, i)
		if err != nil {
			return "", false, err
		}
		if !ok {
			break
		}
		if field.name == target {
			return field.rawValue, true, nil
		}
		i = field.next
	}
	return "", false, nil
}

type graphQLObjectLikeField struct {
	name     string
	rawValue string
	next     int
}

func scanGraphQLObjectLikeField(input string, i int) (graphQLObjectLikeField, bool, error) {
	i = skipGraphQLWhitespace(input, i)
	if i >= len(input) {
		return graphQLObjectLikeField{}, false, nil
	}
	if !isGraphQLNameStart(input[i]) {
		return graphQLObjectLikeField{}, false, fmt.Errorf("expected GraphQL argument name")
	}
	name, next := scanGraphQLName(input, i)
	next = skipGraphQLWhitespace(input, next)
	if next >= len(input) || input[next] != ':' {
		return graphQLObjectLikeField{}, false, fmt.Errorf("expected ':' after GraphQL argument %q", name)
	}
	valueStart := skipGraphQLWhitespace(input, next+1)
	valueEnd, err := scanGraphQLInputValueEnd(input, valueStart)
	if err != nil {
		return graphQLObjectLikeField{}, false, err
	}
	return graphQLObjectLikeField{
		name:     name,
		rawValue: input[valueStart:valueEnd],
		next:     nextGraphQLObjectLikeField(input, valueEnd),
	}, true, nil
}

func nextGraphQLObjectLikeField(input string, i int) int {
	i = skipGraphQLWhitespace(input, i)
	if i < len(input) && input[i] == ',' {
		return i + 1
	}
	return i
}

func scanGraphQLInputValueEnd(input string, i int) (int, error) {
	i = skipGraphQLWhitespace(input, i)
	if i >= len(input) {
		return i, fmt.Errorf("missing GraphQL value")
	}
	switch input[i] {
	case '"':
		return skipGraphQLString(input, i), nil
	case '$':
		return skipGraphQLVariable(input, i), nil
	case '{':
		return scanGraphQLEnclosedValue(input, i, '{', '}')
	case '[':
		return scanGraphQLEnclosedValue(input, i, '[', ']')
	default:
		return scanGraphQLBareValueEnd(input, i), nil
	}
}

func parseGraphQLStringLikeValue(raw string, variables map[string]any, field string) (string, error) {
	raw = strings.TrimSpace(raw)
	switch {
	case strings.HasPrefix(raw, `"""`):
		return "", fmt.Errorf("GraphQL block strings are unsupported in issueUpdate guard")
	case strings.HasPrefix(raw, `"`):
		return parseGraphQLStringLiteral(raw, field)
	case strings.HasPrefix(raw, "$"):
		return parseGraphQLStringVariable(raw, variables, field)
	default:
		return "", fmt.Errorf("unsupported %s value", field)
	}
}

func parseGraphQLStringLiteral(raw, field string) (string, error) {
	end := skipGraphQLString(raw, 0)
	if end != len(raw) {
		return "", fmt.Errorf("unsupported %s value", field)
	}
	value, err := strconv.Unquote(raw)
	if err != nil {
		return "", fmt.Errorf("decode GraphQL string literal: %w", err)
	}
	return requiredGraphQLString(value, field)
}

func parseGraphQLStringVariable(raw string, variables map[string]any, field string) (string, error) {
	name, end := scanGraphQLVariableName(raw, 0)
	if end != len(raw) {
		return "", fmt.Errorf("unsupported %s value", field)
	}
	return requiredGraphQLString(variables[name], field)
}

func requiredGraphQLString(value any, field string) (string, error) {
	text, _ := value.(string)
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	return strings.TrimSpace(text), nil
}

func scanGraphQLBareValueEnd(input string, i int) int {
	for i < len(input) {
		switch input[i] {
		case ',', '}', ']', ')', ' ', '\t', '\n', '\r':
			return i
		default:
			i++
		}
	}
	return i
}

func scanGraphQLVariableName(input string, i int) (string, int) {
	start := i + 1
	end := skipGraphQLVariable(input, i)
	return input[start:end], end
}

func skipGraphQLWhitespace(input string, i int) int {
	for i < len(input) {
		switch input[i] {
		case ' ', '\t', '\n', '\r', ',':
			i++
		default:
			return i
		}
	}
	return i
}

func scanGraphQLEnclosedValue(input string, i int, open, close byte) (int, error) {
	depth := 0
	for ; i < len(input); i++ {
		switch input[i] {
		case '#':
			i = skipGraphQLLineComment(input, i) - 1
		case '"':
			i = skipGraphQLString(input, i) - 1
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return i + 1, nil
			}
		}
	}
	return len(input), fmt.Errorf("unterminated GraphQL value")
}

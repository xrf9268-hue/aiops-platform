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
	doc, err := parseIssueUpdateDocument(query)
	if err != nil {
		return nil, err
	}
	var foundArgs []string
	for _, body := range doc.mutationBodies {
		args, err := scanIssueUpdateSelectionSet(body, doc.fragments, map[string]bool{})
		if err != nil {
			return nil, err
		}
		foundArgs = append(foundArgs, args...)
	}
	if len(foundArgs) > 0 {
		return foundArgs, nil
	}
	return nil, errIssueUpdateNotFound
}

type issueUpdateDocument struct {
	mutationBodies []string
	fragments      map[string]string
}

func parseIssueUpdateDocument(query string) (issueUpdateDocument, error) {
	scanner := issueUpdateDocumentScanner{
		query: query,
		doc:   issueUpdateDocument{fragments: map[string]string{}},
	}
	if err := scanner.run(); err != nil {
		return issueUpdateDocument{}, err
	}
	return scanner.doc, nil
}

type issueUpdateDocumentScanner struct {
	query string
	i     int
	doc   issueUpdateDocument
}

func (s *issueUpdateDocumentScanner) run() error {
	for s.i < len(s.query) {
		if err := s.step(); err != nil {
			return err
		}
	}
	return nil
}

func (s *issueUpdateDocumentScanner) step() error {
	if next, ok := skipIssueUpdateScanToken(s.query, s.i); ok {
		s.i = next
		return nil
	}
	if s.query[s.i] == '{' {
		return s.skipSelectionSet()
	}
	if !isGraphQLNameStart(s.query[s.i]) {
		s.i++
		return nil
	}
	return s.scanTopLevelName()
}

func (s *issueUpdateDocumentScanner) skipSelectionSet() error {
	next, err := scanGraphQLEnclosedValue(s.query, s.i, '{', '}')
	if err != nil {
		return err
	}
	s.i = next
	return nil
}

func (s *issueUpdateDocumentScanner) scanTopLevelName() error {
	name, next := scanGraphQLName(s.query, s.i)
	switch name {
	case "mutation":
		return s.scanMutationHeader(next)
	case "fragment":
		return s.scanFragmentHeader(next)
	default:
		s.i = next
		return nil
	}
}

func (s *issueUpdateDocumentScanner) scanMutationHeader(i int) error {
	body, end, found, err := scanIssueUpdateBodyAfterHeader(s.query, i)
	if err != nil {
		return err
	}
	if found {
		s.doc.mutationBodies = append(s.doc.mutationBodies, body)
		s.i = end
		return nil
	}
	s.i = i
	return nil
}

func (s *issueUpdateDocumentScanner) scanFragmentHeader(i int) error {
	fragment, found, err := scanIssueUpdateFragmentDefinition(s.query, i)
	if err != nil {
		return err
	}
	if found {
		s.doc.fragments[fragment.name] = fragment.body
		s.i = fragment.next
		return nil
	}
	s.i = i
	return nil
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

func scanIssueUpdateBodyAfterHeader(query string, i int) (string, int, bool, error) {
	open, found, err := findNextGraphQLSelectionOpen(query, i)
	if err != nil || !found {
		return "", i, false, err
	}
	end, err := scanGraphQLEnclosedValue(query, open, '{', '}')
	if err != nil {
		return "", i, false, err
	}
	return query[open+1 : end-1], end, true, nil
}

type issueUpdateFragmentDefinition struct {
	name string
	body string
	next int
}

func scanIssueUpdateFragmentDefinition(query string, i int) (issueUpdateFragmentDefinition, bool, error) {
	i = skipGraphQLWhitespace(query, i)
	if i >= len(query) || !isGraphQLNameStart(query[i]) {
		return issueUpdateFragmentDefinition{}, false, nil
	}
	name, next := scanGraphQLName(query, i)
	body, end, found, err := scanIssueUpdateBodyAfterHeader(query, next)
	if err != nil || !found {
		return issueUpdateFragmentDefinition{}, false, err
	}
	return issueUpdateFragmentDefinition{name: name, body: body, next: end}, true, nil
}

func findNextGraphQLSelectionOpen(query string, i int) (int, bool, error) {
	for i < len(query) {
		if next, ok := skipIssueUpdateScanToken(query, i); ok {
			i = next
			continue
		}
		switch query[i] {
		case '(':
			next, err := scanGraphQLEnclosedValue(query, i, '(', ')')
			if err != nil {
				return 0, false, err
			}
			i = next
		case '{':
			return i, true, nil
		default:
			i++
		}
	}
	return 0, false, nil
}

func scanIssueUpdateSelectionSet(body string, fragments map[string]string, visited map[string]bool) ([]string, error) {
	scanner := issueUpdateSelectionSetScanner{body: body, fragments: fragments, visited: visited}
	if err := scanner.run(); err != nil {
		return nil, err
	}
	return scanner.args, nil
}

type issueUpdateSelectionSetScanner struct {
	body       string
	fragments  map[string]string
	visited    map[string]bool
	i          int
	depth      int
	parenDepth int
	args       []string
}

func (s *issueUpdateSelectionSetScanner) run() error {
	for s.i < len(s.body) {
		if err := s.step(); err != nil {
			return err
		}
	}
	return nil
}

func (s *issueUpdateSelectionSetScanner) step() error {
	if next, ok := skipIssueUpdateScanToken(s.body, s.i); ok {
		s.i = next
		return nil
	}
	if s.atRootSpread() {
		return s.scanSpread()
	}
	if updateIssueUpdateSelectionDepth(s.body[s.i], &s.depth, &s.parenDepth) {
		s.i++
		return nil
	}
	if s.atRootName() {
		return s.scanRootName()
	}
	s.i++
	return nil
}

func (s *issueUpdateSelectionSetScanner) atRootSpread() bool {
	return s.depth == 0 && s.parenDepth == 0 && strings.HasPrefix(s.body[s.i:], "...")
}

func (s *issueUpdateSelectionSetScanner) atRootName() bool {
	return s.depth == 0 && s.parenDepth == 0 && isGraphQLNameStart(s.body[s.i])
}

func (s *issueUpdateSelectionSetScanner) scanRootName() error {
	name, next := scanGraphQLName(s.body, s.i)
	if name == "issueUpdate" {
		args, err := argumentTextAfterName(s.body, next)
		if err != nil {
			return err
		}
		s.args = append(s.args, args)
	}
	s.i = next
	return nil
}

func updateIssueUpdateSelectionDepth(ch byte, depth, parenDepth *int) bool {
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

func (s *issueUpdateSelectionSetScanner) scanSpread() error {
	next := skipGraphQLWhitespace(s.body, s.i+3)
	if next >= len(s.body) || !isGraphQLNameStart(s.body[next]) {
		s.i += 3
		return nil
	}
	name, afterName := scanGraphQLName(s.body, next)
	if name == "on" {
		return s.scanInlineFragment(afterName)
	}
	if err := s.scanNamedFragment(name); err != nil {
		return err
	}
	s.i = afterName
	return nil
}

func (s *issueUpdateSelectionSetScanner) scanInlineFragment(i int) error {
	body, end, found, err := scanIssueUpdateBodyAfterHeader(s.body, i)
	if err != nil || !found {
		s.i = i
		return err
	}
	args, err := scanIssueUpdateSelectionSet(body, s.fragments, s.visited)
	if err != nil {
		return err
	}
	s.args = append(s.args, args...)
	s.i = end
	return nil
}

func (s *issueUpdateSelectionSetScanner) scanNamedFragment(name string) error {
	body, ok := s.fragments[name]
	if !ok || s.visited[name] {
		return nil
	}
	s.visited[name] = true
	args, err := scanIssueUpdateSelectionSet(body, s.fragments, s.visited)
	delete(s.visited, name)
	if err != nil {
		return err
	}
	s.args = append(s.args, args...)
	return nil
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

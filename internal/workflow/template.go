package workflow

import (
	"errors"
	"fmt"
	"html"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

var (
	liquidInterpolationRE = regexp.MustCompile(`{{\s*([^{}]+?)\s*}}`)
	liquidTagRE           = regexp.MustCompile(`{%\s*([^{}%]+?)\s*%}`)
)

func DefaultPrompt() string {
	return `You are working on an AI coding task.

Read the task context, inspect the repository before editing, make the smallest safe change, run verification commands, and produce a clear summary.

Handoff:
- Push branches, open pull requests, and write tracker updates yourself using the tools available in the runtime environment.
- If a linear_graphql tool is available, use it for Linear state transitions, comments, and PR-link handoff updates; the orchestrator keeps the Linear token isolated from your process.
- The orchestrator is a scheduler/runner and tracker reader. Do not expect new workflow designs to rely on orchestrator-side ticket moves, comments, pushes, or pull-request handoffs after you exit.

Rules:
- Do not touch secrets, credentials, production deployment files, or database migrations unless explicitly requested.
- Prefer a small change over a broad refactor.
- If blocked, explain the blocker and stop.`
}

type TemplateRenderError struct {
	Name string
	Err  error
}

func (e *TemplateRenderError) Error() string {
	if e == nil {
		return "template_render_error"
	}
	if e.Name == "" {
		return "template_render_error: " + e.Err.Error()
	}
	return fmt.Sprintf("template_render_error: %s: %v", e.Name, e.Err)
}

func (e *TemplateRenderError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func Render(template string, vars map[string]any) (string, error) {
	if strings.TrimSpace(template) == "" {
		template = DefaultPrompt()
	}
	var err error
	template, err = renderLiquidIfTags(template, vars)
	if err != nil {
		return "", &TemplateRenderError{Err: err}
	}

	var firstErr error
	out := liquidInterpolationRE.ReplaceAllStringFunc(template, func(match string) string {
		if firstErr != nil {
			return match
		}
		expr := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(match, "{{"), "}}"))
		parts := strings.Split(expr, "|")
		name := strings.TrimSpace(parts[0])
		value, ok := lookupTemplateVar(vars, name)
		if !ok && !hasDefaultFilter(parts[1:]) {
			firstErr = fmt.Errorf("missing variable %q", name)
			return match
		}
		for _, rawFilter := range parts[1:] {
			var err error
			value, err = applyLiquidFilter(value, rawFilter)
			if err != nil {
				firstErr = err
				return match
			}
		}
		return stringifyLiquidValue(value)
	})
	if firstErr != nil {
		return "", &TemplateRenderError{Err: firstErr}
	}
	return out, nil
}

func renderLiquidIfTags(template string, vars map[string]any) (string, error) {
	for {
		match := liquidTagRE.FindStringSubmatchIndex(template)
		if match == nil {
			return template, nil
		}
		tag := strings.TrimSpace(template[match[2]:match[3]])
		if tag == "endif" {
			return "", fmt.Errorf("unexpected endif tag")
		}
		if !strings.HasPrefix(tag, "if ") {
			return "", fmt.Errorf("unsupported tag %q", tag)
		}
		closeStart, closeEnd, err := findLiquidEndif(template[match[1]:])
		if err != nil {
			return "", err
		}
		bodyStart := match[1]
		bodyEnd := match[1] + closeStart
		afterEnd := match[1] + closeEnd
		replacement, err := renderLiquidIfBody(tag, template[bodyStart:bodyEnd], vars)
		if err != nil {
			return "", err
		}
		template = template[:match[0]] + replacement + template[afterEnd:]
	}
}

type liquidBranch struct {
	cond string
	body string
}

func renderLiquidIfBody(openTag, body string, vars map[string]any) (string, error) {
	branches := splitLiquidBranches(openTag, body)
	for _, branch := range branches {
		if branch.cond == "" {
			return branch.body, nil
		}
		matched, err := evalLiquidCondition(branch.cond, vars)
		if err != nil {
			return "", err
		}
		if matched {
			return branch.body, nil
		}
	}
	return "", nil
}

func splitLiquidBranches(openTag, body string) []liquidBranch {
	branches := []liquidBranch{{cond: strings.TrimSpace(strings.TrimPrefix(openTag, "if "))}}
	segmentStart := 0
	depth := 0
	matches := liquidTagRE.FindAllStringSubmatchIndex(body, -1)
	for _, match := range matches {
		tag := strings.TrimSpace(body[match[2]:match[3]])
		switch {
		case strings.HasPrefix(tag, "if "):
			depth++
		case tag == "endif" && depth > 0:
			depth--
		case depth == 0 && strings.HasPrefix(tag, "elsif "):
			branches[len(branches)-1].body = body[segmentStart:match[0]]
			branches = append(branches, liquidBranch{cond: strings.TrimSpace(strings.TrimPrefix(tag, "elsif "))})
			segmentStart = match[1]
		case depth == 0 && tag == "else":
			branches[len(branches)-1].body = body[segmentStart:match[0]]
			branches = append(branches, liquidBranch{})
			segmentStart = match[1]
		}
	}
	branches[len(branches)-1].body = body[segmentStart:]
	return branches
}

func findLiquidEndif(template string) (start int, end int, err error) {
	matches := liquidTagRE.FindAllStringSubmatchIndex(template, -1)
	depth := 1
	for _, match := range matches {
		tag := strings.TrimSpace(template[match[2]:match[3]])
		switch {
		case strings.HasPrefix(tag, "if "):
			depth++
		case tag == "endif":
			depth--
			if depth == 0 {
				return match[0], match[1], nil
			}
		}
	}
	return 0, 0, fmt.Errorf("unterminated if tag")
}

func evalLiquidCondition(cond string, vars map[string]any) (bool, error) {
	for _, op := range []string{"==", "!=", ">=", "<=", ">", "<"} {
		if left, right, ok := strings.Cut(cond, op); ok {
			lv, err := evalLiquidOperand(strings.TrimSpace(left), vars)
			if err != nil {
				return false, err
			}
			rv, err := evalLiquidOperand(strings.TrimSpace(right), vars)
			if err != nil {
				return false, err
			}
			return compareLiquidValues(lv, rv, op)
		}
	}
	value, ok := lookupTemplateVar(vars, cond)
	if !ok {
		return false, fmt.Errorf("missing variable %q", cond)
	}
	return isLiquidTruthy(value), nil
}

func evalLiquidOperand(raw string, vars map[string]any) (any, error) {
	if raw == "nil" || raw == "null" {
		return nil, nil
	}
	if raw == "true" {
		return true, nil
	}
	if raw == "false" {
		return false, nil
	}
	if i, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return i, nil
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		return f, nil
	}
	if isQuotedLiquidLiteral(raw) {
		return parseLiquidLiteral(raw), nil
	}
	value, ok := lookupTemplateVar(vars, raw)
	if !ok {
		return nil, fmt.Errorf("missing variable %q", raw)
	}
	return value, nil
}

func compareLiquidValues(left, right any, op string) (bool, error) {
	switch op {
	case "==":
		return fmt.Sprint(left) == fmt.Sprint(right), nil
	case "!=":
		return fmt.Sprint(left) != fmt.Sprint(right), nil
	}
	lf, lok := liquidNumber(left)
	rf, rok := liquidNumber(right)
	if !lok || !rok {
		return false, errors.New("ordered Liquid comparisons require numeric operands")
	}
	switch op {
	case ">":
		return lf > rf, nil
	case "<":
		return lf < rf, nil
	case ">=":
		return lf >= rf, nil
	case "<=":
		return lf <= rf, nil
	default:
		return false, fmt.Errorf("unsupported comparison operator %q", op)
	}
}

func liquidNumber(value any) (float64, bool) {
	switch v := value.(type) {
	case int:
		return float64(v), true
	case int8:
		return float64(v), true
	case int16:
		return float64(v), true
	case int32:
		return float64(v), true
	case int64:
		return float64(v), true
	case uint:
		return float64(v), true
	case uint8:
		return float64(v), true
	case uint16:
		return float64(v), true
	case uint32:
		return float64(v), true
	case uint64:
		return float64(v), true
	case float32:
		return float64(v), true
	case float64:
		return v, true
	case string:
		f, err := strconv.ParseFloat(v, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func hasDefaultFilter(filters []string) bool {
	for _, rawFilter := range filters {
		name, _, _ := strings.Cut(strings.TrimSpace(rawFilter), ":")
		if strings.TrimSpace(name) == "default" {
			return true
		}
	}
	return false
}

func applyLiquidFilter(value any, rawFilter string) (any, error) {
	name, arg, hasArg := strings.Cut(strings.TrimSpace(rawFilter), ":")
	name = strings.TrimSpace(name)
	switch name {
	case "escape":
		return html.EscapeString(stringifyLiquidValue(value)), nil
	case "default":
		if !hasArg {
			return nil, fmt.Errorf("filter %q requires an argument", name)
		}
		if isLiquidBlank(value) {
			return parseLiquidLiteral(arg), nil
		}
		return value, nil
	default:
		return nil, fmt.Errorf("unknown filter %q", strings.TrimSpace(rawFilter))
	}
}

func isLiquidTruthy(value any) bool {
	if value == nil {
		return false
	}
	if b, ok := value.(bool); ok {
		return b
	}
	return true
}

func stringifyLiquidValue(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func isLiquidBlank(value any) bool {
	if value == nil {
		return true
	}
	if s, ok := value.(string); ok {
		return s == ""
	}
	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return true
	}
	switch rv.Kind() {
	case reflect.Array, reflect.Chan, reflect.Map, reflect.Slice:
		return rv.Len() == 0
	case reflect.Pointer, reflect.Interface:
		return rv.IsNil()
	}
	return false
}

func parseLiquidLiteral(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) >= 2 {
		quote := trimmed[0]
		if (quote == '\'' || quote == '"') && trimmed[len(trimmed)-1] == quote {
			return trimmed[1 : len(trimmed)-1]
		}
	}
	return trimmed
}

func isQuotedLiquidLiteral(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) < 2 {
		return false
	}
	quote := trimmed[0]
	return (quote == '\'' || quote == '"') && trimmed[len(trimmed)-1] == quote
}

func lookupTemplateVar(vars map[string]any, name string) (any, bool) {
	if vars == nil || name == "" {
		return nil, false
	}
	if value, ok := vars[name]; ok {
		return value, true
	}
	parts := strings.Split(name, ".")
	var cur any = vars
	for _, part := range parts {
		if part == "" {
			return nil, false
		}
		next, ok := lookupTemplatePart(cur, part)
		if !ok {
			return nil, false
		}
		cur = next
	}
	return cur, true
}

func lookupTemplatePart(value any, name string) (any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		got, ok := typed[name]
		return got, ok
	case map[string]string:
		got, ok := typed[name]
		return got, ok
	}
	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return nil, false
	}
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil, false
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil, false
	}
	fieldName := snakeOrDotToFieldName(name)
	field := rv.FieldByName(fieldName)
	if !field.IsValid() || !field.CanInterface() {
		return nil, false
	}
	return field.Interface(), true
}

func snakeOrDotToFieldName(name string) string {
	parts := strings.FieldsFunc(name, func(r rune) bool { return r == '_' || r == '-' })
	for i, part := range parts {
		if part == "" {
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, "")
}

package workflow

import (
	"fmt"
	"html"
	"reflect"
	"regexp"
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
	if tag := liquidTagRE.FindStringSubmatch(template); len(tag) > 0 {
		return "", &TemplateRenderError{Err: fmt.Errorf("unsupported tag %q", strings.TrimSpace(tag[1]))}
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
		return fmt.Sprint(value)
	})
	if firstErr != nil {
		return "", &TemplateRenderError{Err: firstErr}
	}
	return out, nil
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
		return html.EscapeString(fmt.Sprint(value)), nil
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

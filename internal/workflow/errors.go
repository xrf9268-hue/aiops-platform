package workflow

import (
	"errors"
	"fmt"
)

type Category string

const (
	CategoryMissingWorkflowFile       Category = "missing_workflow_file"
	CategoryWorkflowParseError        Category = "workflow_parse_error"
	CategoryWorkflowFrontMatterNotMap Category = "workflow_front_matter_not_a_map"
	CategoryTemplateParseError        Category = "template_parse_error"
	CategoryTemplateRenderError       Category = "template_render_error"
)

var (
	ErrMissingWorkflowFile       = &Error{Category: CategoryMissingWorkflowFile}
	ErrWorkflowParse             = &Error{Category: CategoryWorkflowParseError}
	ErrWorkflowFrontMatterNotMap = &Error{Category: CategoryWorkflowFrontMatterNotMap}
	ErrTemplateParse             = &TemplateParseError{}
	ErrTemplateRender            = &TemplateRenderError{}
)

type Error struct {
	Category Category
	Path     string
	Message  string
	Err      error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	msg := e.Message
	if msg == "" {
		msg = string(e.Category)
	}
	if e.Path != "" {
		msg = e.Path + ": " + msg
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *Error) Is(target error) bool {
	return categoryMatches(e.category(), target)
}

func (e *Error) category() Category {
	if e == nil {
		return ""
	}
	return e.Category
}

type TemplateParseError struct {
	Err error
}

func (e *TemplateParseError) Error() string {
	if e == nil || e.Err == nil {
		return string(CategoryTemplateParseError)
	}
	return fmt.Sprintf("%s: %v", CategoryTemplateParseError, e.Err)
}

func (e *TemplateParseError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *TemplateParseError) Is(target error) bool {
	return categoryMatches(e.category(), target)
}

func (e *TemplateParseError) category() Category {
	return CategoryTemplateParseError
}

type categorized interface {
	category() Category
}

func ErrorCategory(err error) (Category, bool) {
	var categorizedErr categorized
	if errors.As(err, &categorizedErr) {
		category := categorizedErr.category()
		return category, category != ""
	}
	return "", false
}

func categoryMatches(category Category, target error) bool {
	if category == "" || target == nil {
		return false
	}
	var targetCategory categorized
	return errors.As(target, &targetCategory) && targetCategory.category() == category
}

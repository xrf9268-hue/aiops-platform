package runner

import (
	"context"
	"fmt"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

type RunInput struct {
	Task     task.Task
	Workflow workflow.Workflow
	Workdir  string
	Prompt   string
}

type Result struct {
	Summary string
}

type Runner interface {
	Run(ctx context.Context, in RunInput) (Result, error)
}

func New(name string) (Runner, error) {
	switch name {
	case "", "mock":
		return MockRunner{}, nil
	case "codex":
		return ShellRunner{Name: "codex"}, nil
	case "claude":
		return ShellRunner{Name: "claude"}, nil
	default:
		return nil, fmt.Errorf("unknown runner: %s", name)
	}
}

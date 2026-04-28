package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

type ShellRunner struct {
	Name string
}

func (r ShellRunner) Run(ctx context.Context, in RunInput) (Result, error) {
	command := ""
	switch r.Name {
	case "codex":
		command = in.Workflow.Config.Codex.Command
	case "claude":
		command = in.Workflow.Config.Claude.Command
	}
	if command == "" {
		return Result{}, fmt.Errorf("empty command for runner %s", r.Name)
	}
	cmd := exec.CommandContext(ctx, "sh", "-lc", command+" < .aiops/PROMPT.md")
	cmd.Dir = in.Workdir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return Result{}, err
	}
	return Result{Summary: r.Name + " completed"}, nil
}

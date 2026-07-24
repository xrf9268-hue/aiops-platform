package worker

import (
	"context"

	"github.com/xrf9268-hue/aiops-platform/internal/runner"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
	"github.com/xrf9268-hue/aiops-platform/internal/workspace"
)

func emitHookResults(ctx context.Context, ev EventEmitter, taskID, identifier string, results []workspace.HookResult) {
	for _, res := range results {
		Emit(ctx, ev, taskID, identifier, task.EventWorkspaceHookEnd, string(res.Name)+" hook completed", map[string]any{
			"hook":        string(res.Name),
			"command":     res.Command,
			"exit_code":   res.ExitCode,
			"output":      res.Output,
			"truncated":   res.Truncated,
			"duration_ms": res.Duration.Milliseconds(),
			"error":       ErrSummary(res.Err),
		})
	}
}

func runWorkspaceHook(ctx context.Context, ev EventEmitter, taskID, identifier, workdir string, name workspace.HookName, hook workflow.WorkspaceHook, timeoutMs int, envPassthrough []string, cfg workflow.Config) error {
	if len(hook.Commands) == 0 {
		return nil
	}
	Emit(ctx, ev, taskID, identifier, task.EventWorkspaceHookStart, string(name)+" hook started", map[string]any{
		"hook":       string(name),
		"commands":   len(hook.Commands),
		"timeout_ms": timeoutMs,
	})
	results, err := workspace.RunWorkspaceHook(ctx, workdir, name, hook, timeoutMs, envPassthrough, cfg)
	emitHookResults(ctx, ev, taskID, identifier, results)
	return err
}

func removeWorkdirAfterHookFailure(ctx context.Context, ev EventEmitter, taskID, identifier, workspaceRoot, workdir string, beforeRemove workflow.WorkspaceHook, timeoutMs int, envPassthrough []string, cfg workflow.Config, reason string) {
	if err := workspace.ValidateRemove(workspaceRoot, workdir); err != nil {
		LogTaskIDEventf(taskID, identifier, "workspace_remove_failed", "reason=%s workdir=%q error=%q", reason, workdir, err)
		return
	}
	if err := runWorkspaceHook(ctx, ev, taskID, identifier, workdir, workspace.HookBeforeRemove, beforeRemove, timeoutMs, envPassthrough, cfg); err != nil {
		LogTaskIDEventf(taskID, identifier, "before_remove_hook_failed", "reason=%s error=%q", reason, err)
	}
	if err := workspace.SafeRemove(workspaceRoot, workdir); err != nil {
		LogTaskIDEventf(taskID, identifier, "workspace_remove_failed", "reason=%s workdir=%q error=%q", reason, workdir, err)
		return
	}
	if err := runner.RemoveSandboxGoBuildCache(workdir); err != nil {
		LogTaskIDEventf(taskID, identifier, "go_build_cache_remove_failed", "reason=%s workdir=%q error=%q", reason, workdir, err)
	}
}

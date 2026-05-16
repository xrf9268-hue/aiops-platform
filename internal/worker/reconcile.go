package worker

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/workspace"
)

const defaultReconcileTaskID = "reconcile-startup"

// ReconcileTracker is the tracker reader the worker needs for startup
// workspace reconciliation. Implementations fetch issues by explicit workflow
// state names so terminal-state cleanup is driven by tracker state, not queue
// rows.
type ReconcileTracker interface {
	ListIssuesByStates(ctx context.Context, states []string) ([]tracker.Issue, error)
}

// ReconcileConfig contains the dependencies for the SPEC startup reconciliation
// pass. The pass is idempotent: active issue workspaces are preserved, while
// terminal and unknown/deleted issue workspaces are removed before dispatch.
type ReconcileConfig struct {
	WorkspaceRoot   string
	ActiveStates    []string
	TerminalStates  []string
	Tracker         ReconcileTracker
	Emitter         EventEmitter
	ReconcileTaskID string
}

// ReconcileStartup reconciles existing per-issue workspaces with tracker state.
// It removes terminal and unknown/deleted workspaces and leaves active issue
// workspaces intact. It emits reconcile_start, reconcile_workspace, and
// reconcile_end task events so startup recovery is visible in the same event
// stream as normal task lifecycle activity.
func ReconcileStartup(ctx context.Context, cfg ReconcileConfig) error {
	if strings.TrimSpace(cfg.WorkspaceRoot) == "" {
		return fmt.Errorf("workspace root is required")
	}
	if cfg.Tracker == nil {
		return fmt.Errorf("reconcile tracker is required")
	}
	taskID := cfg.ReconcileTaskID
	if taskID == "" {
		taskID = defaultReconcileTaskID
	}
	Emit(ctx, cfg.Emitter, taskID, task.EventReconcileStart, "startup reconciliation started", map[string]any{
		"workspace_root":  cfg.WorkspaceRoot,
		"active_states":   cfg.ActiveStates,
		"terminal_states": cfg.TerminalStates,
	})

	activeIssues, err := cfg.Tracker.ListIssuesByStates(ctx, cfg.ActiveStates)
	if err != nil {
		return fmt.Errorf("fetch active issues: %w", err)
	}
	terminalIssues, err := cfg.Tracker.ListIssuesByStates(ctx, cfg.TerminalStates)
	if err != nil {
		return fmt.Errorf("fetch terminal issues: %w", err)
	}
	activeKeys := make(map[string]tracker.Issue, len(activeIssues))
	for _, issue := range activeIssues {
		for _, key := range issueWorkspaceKeys(issue) {
			activeKeys[key] = issue
		}
	}
	terminalKeys := make(map[string]tracker.Issue, len(terminalIssues))
	for _, issue := range terminalIssues {
		for _, key := range issueWorkspaceKeys(issue) {
			terminalKeys[key] = issue
		}
	}

	workspaces, err := listIssueWorkspaces(cfg.WorkspaceRoot)
	if err != nil {
		return err
	}
	var removed, kept int
	for _, workspace := range workspaces {
		if _, ok := activeKeys[workspace.Key]; ok {
			kept++
			Emit(ctx, cfg.Emitter, taskID, task.EventReconcileWorkspace, "kept active workspace", map[string]any{
				"path":   workspace.Path,
				"key":    workspace.Key,
				"action": "keep",
				"reason": "active",
			})
			continue
		}
		if issue, ok := terminalKeys[workspace.Key]; ok {
			removedOne, err := removeWorkspace(ctx, cfg, taskID, workspace.Path, issue, "terminal")
			if err != nil {
				return err
			}
			if removedOne {
				removed++
			}
			continue
		}
		removedOne, err := removeWorkspace(ctx, cfg, taskID, workspace.Path, tracker.Issue{}, "unknown")
		if err != nil {
			return err
		}
		if removedOne {
			removed++
		}
	}

	Emit(ctx, cfg.Emitter, taskID, task.EventReconcileEnd, "startup reconciliation finished", map[string]any{
		"active_issues":   len(activeIssues),
		"terminal_issues": len(terminalIssues),
		"kept":            kept,
		"removed":         removed,
	})
	return nil
}

type issueWorkspace struct {
	Path string
	Key  string
}

func listIssueWorkspaces(root string) ([]issueWorkspace, error) {
	patterns := []string{
		filepath.Join(root, "*", "*", "linear_issue", "*"),
		filepath.Join(root, "*", "*", "linear-issue", "*"),
	}
	seen := map[string]struct{}{}
	var workspaces []issueWorkspace
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, fmt.Errorf("glob issue workspaces: %w", err)
		}
		for _, path := range matches {
			if _, ok := seen[path]; ok {
				continue
			}
			seen[path] = struct{}{}
			info, err := os.Stat(path)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, fmt.Errorf("stat workspace %s: %w", path, err)
			}
			if !info.IsDir() {
				continue
			}
			workspaces = append(workspaces, issueWorkspace{Path: path, Key: filepath.Base(path)})
		}
	}
	return workspaces, nil
}

func removeWorkspace(ctx context.Context, cfg ReconcileConfig, taskID, path string, issue tracker.Issue, reason string) (bool, error) {
	if err := os.RemoveAll(path); err != nil {
		return false, fmt.Errorf("remove %s workspace %s: %w", reason, path, err)
	}
	Emit(ctx, cfg.Emitter, taskID, task.EventReconcileWorkspace, "removed workspace", map[string]any{
		"path":       path,
		"issue_id":   issue.ID,
		"identifier": issue.Identifier,
		"state":      issue.State,
		"action":     "remove",
		"reason":     reason,
	})
	return true, nil
}

func issueWorkspaceKeys(issue tracker.Issue) []string {
	seen := map[string]struct{}{}
	var keys []string
	rawKeys := []string{issue.Identifier, issue.ID}
	if strings.EqualFold(issue.State, "Rework") && issue.ID != "" && issue.UpdatedAt != "" {
		rawKeys = append(rawKeys, issue.ID+"|rework|"+issue.UpdatedAt)
	}
	for _, raw := range rawKeys {
		for _, key := range []string{workspace.SanitizeComponent(raw), sanitizeLegacyWorkspaceKey(raw)} {
			if key == "" {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
	}
	return keys
}

func sanitizeLegacyWorkspaceKey(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
		if b.Len() >= 120 {
			break
		}
	}
	out := strings.Trim(b.String(), "._-")
	if out == "" {
		return "workspace"
	}
	return out
}

// LogEventEmitter records reconciliation events to the process log. Startup
// reconciliation is tracker/filesystem state, not queue task state, so the
// worker does not write synthetic rows to task_events (which intentionally FK
// to real tasks).
type LogEventEmitter struct{}

func (LogEventEmitter) AddEvent(_ context.Context, taskID, kind, msg string) error {
	log.Printf("task %s: %s: %s", taskID, kind, msg)
	return nil
}

func (LogEventEmitter) AddEventWithPayload(ctx context.Context, taskID, kind, msg string, payload any) error {
	if payload == nil {
		return LogEventEmitter{}.AddEvent(ctx, taskID, kind, msg)
	}
	log.Printf("task %s: %s: %s payload=%v", taskID, kind, msg, payload)
	return nil
}

// LogReconcileError records reconciliation failure before the worker exits.
func LogReconcileError(err error) {
	if err != nil {
		log.Printf("startup reconciliation failed: %v", err)
	}
}

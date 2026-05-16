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
	if len(nonEmptyStates(cfg.ActiveStates)) == 0 {
		return fmt.Errorf("active states are required for startup reconciliation")
	}
	if len(nonEmptyStates(cfg.TerminalStates)) == 0 {
		return fmt.Errorf("terminal states are required for startup reconciliation")
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

	activeStates := nonEmptyStates(cfg.ActiveStates)
	terminalStates := nonEmptyStates(cfg.TerminalStates)
	activeIssues, err := cfg.Tracker.ListIssuesByStates(ctx, activeStates)
	if err != nil {
		return fmt.Errorf("fetch active issues: %w", err)
	}
	terminalIssues, err := cfg.Tracker.ListIssuesByStates(ctx, terminalStates)
	if err != nil {
		return fmt.Errorf("fetch terminal issues: %w", err)
	}
	activeKeys := make(map[string]tracker.Issue, len(activeIssues))
	for _, issue := range activeIssues {
		for _, key := range issueWorkspaceKeys(issue) {
			activeKeys[key] = issue
		}
	}
	canRemoveUnknown := len(terminalIssues) > 0
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
	if len(workspaces) > 0 && len(activeIssues)+len(terminalIssues) == 0 {
		return fmt.Errorf("tracker returned no active or terminal issues; refusing to remove %d existing workspaces", len(workspaces))
	}
	var removed, kept int
	for _, workspace := range workspaces {
		if err := ctx.Err(); err != nil {
			return err
		}
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
		if activeIssue, ok := activeReworkIssueForWorkspace(workspace.Key, activeIssues); ok {
			kept++
			Emit(ctx, cfg.Emitter, taskID, task.EventReconcileWorkspace, "kept active workspace", map[string]any{
				"path":       workspace.Path,
				"key":        workspace.Key,
				"issue_id":   activeIssue.ID,
				"identifier": activeIssue.Identifier,
				"action":     "keep",
				"reason":     "active_rework",
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
		if !canRemoveUnknown {
			kept++
			Emit(ctx, cfg.Emitter, taskID, task.EventReconcileWorkspace, "kept unknown workspace", map[string]any{
				"path":   workspace.Path,
				"key":    workspace.Key,
				"action": "keep",
				"reason": "unknown_terminal_state_unconfirmed",
			})
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
	ownerEntries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read workspace root %s: %w", root, err)
	}
	var workspaces []issueWorkspace
	for _, ownerEntry := range ownerEntries {
		if !ownerEntry.IsDir() {
			continue
		}
		ownerPath := filepath.Join(root, ownerEntry.Name())
		repoEntries, err := os.ReadDir(ownerPath)
		if err != nil {
			return nil, fmt.Errorf("read workspace owner %s: %w", ownerPath, err)
		}
		for _, repoEntry := range repoEntries {
			if !repoEntry.IsDir() {
				continue
			}
			repoPath := filepath.Join(ownerPath, repoEntry.Name())
			for _, sourceDir := range []string{"linear_issue", "linear-issue"} {
				sourcePath := filepath.Join(repoPath, sourceDir)
				workspaceEntries, err := os.ReadDir(sourcePath)
				if err != nil {
					if os.IsNotExist(err) {
						continue
					}
					return nil, fmt.Errorf("read issue workspace source %s: %w", sourcePath, err)
				}
				for _, workspaceEntry := range workspaceEntries {
					if !workspaceEntry.IsDir() {
						continue
					}
					path := filepath.Join(sourcePath, workspaceEntry.Name())
					workspaces = append(workspaces, issueWorkspace{Path: path, Key: workspaceEntry.Name()})
				}
			}
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

func activeReworkIssueForWorkspace(workspaceKey string, issues []tracker.Issue) (tracker.Issue, bool) {
	for _, issue := range issues {
		if !strings.EqualFold(issue.State, "Rework") || strings.TrimSpace(issue.ID) == "" {
			continue
		}
		for _, prefix := range reworkWorkspaceKeyPrefixes(issue.ID) {
			if strings.HasPrefix(workspaceKey, prefix) {
				return issue, true
			}
		}
	}
	return tracker.Issue{}, false
}

func reworkWorkspaceKeyPrefixes(issueID string) []string {
	seen := map[string]struct{}{}
	var prefixes []string
	for _, key := range []string{workspace.SanitizeComponent(issueID), sanitizeLegacyWorkspaceKey(issueID)} {
		if key == "" {
			continue
		}
		prefix := key + "-rework-"
		if _, ok := seen[prefix]; ok {
			continue
		}
		seen[prefix] = struct{}{}
		prefixes = append(prefixes, prefix)
	}
	return prefixes
}

func nonEmptyStates(states []string) []string {
	out := make([]string, 0, len(states))
	for _, state := range states {
		state = strings.TrimSpace(state)
		if state != "" {
			out = append(out, state)
		}
	}
	return out
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

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
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
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
	WorkspaceRoot       string
	ActiveStates        []string
	TerminalStates      []string
	TrackerKind         string
	Tracker             ReconcileTracker
	Emitter             EventEmitter
	ReconcileTaskID     string
	BeforeRemoveHook    workflow.WorkspaceHook
	HookTimeoutMillis   int
	HookEnvPassthrough  []string
	ActiveWorkspaceKeys func(tracker.Issue) []string
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
	Emit(ctx, cfg.Emitter, taskID, "", task.EventReconcileStart, "startup reconciliation started", map[string]any{
		"workspace_root":  cfg.WorkspaceRoot,
		"active_states":   cfg.ActiveStates,
		"terminal_states": cfg.TerminalStates,
	})

	activeStates := nonEmptyStates(cfg.ActiveStates)
	terminalStates := nonEmptyStates(cfg.TerminalStates)
	// SPEC §8.6 / §11.4: transient tracker outages during boot must log a
	// warning and continue startup, not abort the worker. An active-fetch
	// failure is the worst case — without the active list we cannot confirm
	// any workspace is safe to delete — so we emit reconcile_end with
	// `status: skipped` and return nil, leaving every workspace intact. A
	// terminal-fetch failure is non-fatal: active workspaces are still kept,
	// terminal/unknown removal is skipped because `canRemoveUnknown` stays
	// false when `terminalIssues` is empty.
	activeIssues, err := cfg.Tracker.ListIssuesByStates(ctx, activeStates)
	if err != nil {
		LogReconcileEventf("startup_reconcile_active_fetch_failed", "error=%q note=%q", err, "SPEC §8.6: log and continue; no cleanup performed")
		Emit(ctx, cfg.Emitter, taskID, "", task.EventReconcileEnd, "startup reconciliation skipped", map[string]any{
			"status": "skipped",
			"reason": "active_fetch_failed",
			"error":  err.Error(),
		})
		return nil
	}
	terminalIssues, err := cfg.Tracker.ListIssuesByStates(ctx, terminalStates)
	terminalFetchOK := true
	var terminalFetchErr error
	if err != nil {
		LogReconcileEventf("startup_reconcile_terminal_fetch_failed", "error=%q note=%q", err, "SPEC §8.6: log and continue; terminal cleanup skipped")
		terminalFetchOK = false
		terminalFetchErr = err
		terminalIssues = nil
	}
	activeKeysForIssue := cfg.ActiveWorkspaceKeys
	if activeKeysForIssue == nil {
		activeKeysForIssue = issueWorkspaceKeys
	}
	activeKeys := make(map[string]tracker.Issue, len(activeIssues))
	for _, issue := range activeIssues {
		for _, key := range activeKeysForIssue(issue) {
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

	workspaces, err := listIssueWorkspaces(cfg.WorkspaceRoot, cfg.TrackerKind)
	if err != nil {
		return err
	}
	if terminalFetchOK && len(workspaces) > 0 && len(activeIssues)+len(terminalIssues) == 0 {
		return fmt.Errorf("tracker returned no active or terminal issues; refusing to remove %d existing workspaces", len(workspaces))
	}
	var removed, kept int
	for _, workspace := range workspaces {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, ok := activeKeys[workspace.Key]; ok {
			kept++
			Emit(ctx, cfg.Emitter, taskID, "", task.EventReconcileWorkspace, "kept active workspace", map[string]any{
				"path":   workspace.Path,
				"key":    workspace.Key,
				"action": "keep",
				"reason": "active",
			})
			continue
		}
		if activeIssue, ok := activeReworkIssueForWorkspace(workspace.Key, activeIssues, activeKeysForIssue); ok {
			kept++
			Emit(ctx, cfg.Emitter, taskID, "", task.EventReconcileWorkspace, "kept active workspace", map[string]any{
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
			Emit(ctx, cfg.Emitter, taskID, "", task.EventReconcileWorkspace, "kept unknown workspace", map[string]any{
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

	endPayload := map[string]any{
		"active_issues":   len(activeIssues),
		"terminal_issues": len(terminalIssues),
		"kept":            kept,
		"removed":         removed,
	}
	if !terminalFetchOK {
		endPayload["status"] = "partial"
		endPayload["reason"] = "terminal_fetch_failed"
		if terminalFetchErr != nil {
			endPayload["error"] = terminalFetchErr.Error()
		}
	} else {
		endPayload["status"] = "ok"
	}
	Emit(ctx, cfg.Emitter, taskID, "", task.EventReconcileEnd, "startup reconciliation finished", endPayload)
	return nil
}

type issueWorkspace struct {
	Path string
	Key  string
}

func listIssueWorkspaces(root, trackerKind string) ([]issueWorkspace, error) {
	ownerEntries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read workspace root %s: %w", root, err)
	}
	sourceDirs := issueWorkspaceSourceDirs(trackerKind)
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
			for _, sourceDir := range sourceDirs {
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

func issueWorkspaceSourceDirs(trackerKind string) []string {
	switch strings.ToLower(strings.TrimSpace(trackerKind)) {
	case "github":
		return []string{"github_issue", "github-issue"}
	case "gitea":
		return []string{"gitea_issue", "gitea-issue"}
	case "", "linear":
		return []string{"linear_issue", "linear-issue"}
	default:
		return nil
	}
}

func removeWorkspace(ctx context.Context, cfg ReconcileConfig, taskID, path string, issue tracker.Issue, reason string) (bool, error) {
	if err := runWorkspaceHook(ctx, cfg.Emitter, taskID, issue.Identifier, path, workspace.HookBeforeRemove, cfg.BeforeRemoveHook, cfg.HookTimeoutMillis, cfg.HookEnvPassthrough); err != nil {
		log.Printf("event=before_remove_hook_failed task_id=%s issue_id=%s issue_identifier=%s reason=%s workspace=%q error=%q", taskID, issue.ID, issue.Identifier, reason, path, err)
	}
	if err := workspace.SafeRemove(cfg.WorkspaceRoot, path); err != nil {
		return false, fmt.Errorf("remove %s workspace %s: %w", reason, path, err)
	}
	Emit(ctx, cfg.Emitter, taskID, "", task.EventReconcileWorkspace, "removed workspace", map[string]any{
		"path":       path,
		"issue_id":   issue.ID,
		"identifier": issue.Identifier,
		"state":      issue.State,
		"action":     "remove",
		"reason":     reason,
	})
	return true, nil
}

func activeReworkIssueForWorkspace(workspaceKey string, issues []tracker.Issue, activeKeysForIssue func(tracker.Issue) []string) (tracker.Issue, bool) {
	for _, issue := range issues {
		if !strings.EqualFold(issue.State, "Rework") || strings.TrimSpace(issue.ID) == "" {
			continue
		}
		for _, prefix := range reworkWorkspaceKeyPrefixes(issue, activeKeysForIssue) {
			if strings.HasPrefix(workspaceKey, prefix) {
				return issue, true
			}
		}
	}
	return tracker.Issue{}, false
}

func reworkWorkspaceKeyPrefixes(issue tracker.Issue, activeKeysForIssue func(tracker.Issue) []string) []string {
	seen := map[string]struct{}{}
	var prefixes []string
	baseKeys := []string{workspace.SanitizeComponent(issue.ID), sanitizeLegacyWorkspaceKey(issue.ID)}
	for _, key := range activeKeysForIssue(issue) {
		if base, ok := strings.CutSuffix(key, "-rework-"+workspace.SanitizeComponent(tracker.TimeString(issue.UpdatedAt))); ok {
			baseKeys = append(baseKeys, base)
		}
		if base, ok := strings.CutSuffix(key, "_rework_"+sanitizeLegacyWorkspaceKey(tracker.TimeString(issue.UpdatedAt))); ok {
			baseKeys = append(baseKeys, base)
		}
	}
	for _, key := range baseKeys {
		if key == "" {
			continue
		}
		// The current SPEC §4.2 sanitizer maps `|` to `_`, so a rework
		// workspace key looks like `<base>_rework_<sanitized-updatedAt>`.
		// Pre-SPEC layouts produced `<base>-rework-<sanitized-updatedAt>`
		// when `|` collapsed into a `-` separator. Emit both prefix
		// forms so reconciliation matches workspaces from either vintage.
		for _, prefix := range []string{key + "_rework_", key + "-rework-"} {
			if _, ok := seen[prefix]; ok {
				continue
			}
			seen[prefix] = struct{}{}
			prefixes = append(prefixes, prefix)
		}
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
	return workspaceKeysForRawIssueKeys(issue, []string{issue.Identifier, issue.ID})
}

// ActiveWorkspaceKeysForWorkflow returns the active workspace key matcher used
// by startup reconciliation. Service-routed Linear workflows enqueue tasks with
// a service-specific source_event_id, so reconciliation must preserve those
// active workspaces in addition to the legacy issue ID / identifier keys.
func ActiveWorkspaceKeysForWorkflow(cfg workflow.Config) func(tracker.Issue) []string {
	if len(cfg.Services) == 0 {
		return nil
	}
	return func(issue tracker.Issue) []string {
		rawKeys := []string{issue.Identifier, issue.ID}
		for _, service := range cfg.Services {
			if serviceMatchesIssueForReconcile(service, cfg.Tracker, issue) && strings.TrimSpace(service.Name) != "" {
				rawKeys = append(rawKeys, issue.ID+"|service|"+service.Name)
			}
		}
		return workspaceKeysForRawIssueKeys(issue, rawKeys)
	}
}

func workspaceKeysForRawIssueKeys(issue tracker.Issue, rawKeys []string) []string {
	seen := map[string]struct{}{}
	var keys []string
	if strings.EqualFold(issue.State, "Rework") && issue.ID != "" && !issue.UpdatedAt.IsZero() {
		baseKeys := append([]string(nil), rawKeys...)
		for _, raw := range baseKeys {
			if strings.TrimSpace(raw) != "" {
				rawKeys = append(rawKeys, raw+"|rework|"+tracker.TimeString(issue.UpdatedAt))
			}
		}
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

func serviceMatchesIssueForReconcile(service workflow.ServiceConfig, defaults workflow.TrackerConfig, issue tracker.Issue) bool {
	route := service.Tracker
	if !hasExplicitServiceRouteForReconcile(route) {
		return false
	}
	projectSlug := strings.TrimSpace(route.ProjectSlug)
	if projectSlug == "" {
		projectSlug = strings.TrimSpace(defaults.ProjectSlug)
	}
	if projectSlug != "" && !strings.EqualFold(projectSlug, strings.TrimSpace(issue.ProjectSlug)) {
		return false
	}
	if route.TeamKey != "" && !strings.EqualFold(strings.TrimSpace(route.TeamKey), strings.TrimSpace(issue.TeamKey)) {
		return false
	}
	issueLabels := make(map[string]struct{}, len(issue.Labels))
	for _, label := range issue.Labels {
		if label = strings.ToLower(strings.TrimSpace(label)); label != "" {
			issueLabels[label] = struct{}{}
		}
	}
	for _, label := range route.Labels {
		if _, ok := issueLabels[strings.ToLower(strings.TrimSpace(label))]; !ok {
			return false
		}
	}
	for key, want := range route.CustomFields {
		got, ok := issue.CustomFields[key]
		if !ok || strings.TrimSpace(got) != strings.TrimSpace(want) {
			return false
		}
	}
	return true
}

func hasExplicitServiceRouteForReconcile(route workflow.ServiceTrackerRouteConfig) bool {
	return strings.TrimSpace(route.ProjectSlug) != "" ||
		strings.TrimSpace(route.TeamKey) != "" ||
		len(route.Labels) > 0 ||
		len(route.CustomFields) > 0
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
	LogTaskIDEventf(taskID, "", kind, "msg=%q", msg)
	return nil
}

func (LogEventEmitter) AddEventWithPayload(ctx context.Context, taskID, kind, msg string, payload any) error {
	if payload == nil {
		return LogEventEmitter{}.AddEvent(ctx, taskID, kind, msg)
	}
	LogTaskIDEventf(taskID, "", kind, "msg=%q payload=%v", msg, payload)
	return nil
}

// LogReconcileError records reconciliation failure before the worker exits.
func LogReconcileError(err error) {
	if err != nil {
		LogReconcileEventf("startup_reconciliation_failed", "error=%q", err)
	}
}

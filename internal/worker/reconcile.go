package worker

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/xrf9268-hue/aiops-platform/internal/runner"
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
// pass. The pass is idempotent: active and unmatched issue workspaces are
// preserved, while tracker-confirmed terminal issue workspaces are removed.
type ReconcileConfig struct {
	WorkspaceRoot      string
	ActiveStates       []string
	TerminalStates     []string
	TrackerKind        string
	Tracker            ReconcileTracker
	Emitter            EventEmitter
	ReconcileTaskID    string
	WorkflowConfig     workflow.Config
	BeforeRemoveHook   workflow.WorkspaceHook
	HookTimeoutMillis  int
	HookEnvPassthrough []string
}

// ReconcileStartup reconciles existing per-issue workspaces with tracker state.
// It removes only tracker-confirmed terminal workspaces and leaves active or
// unmatched workspaces intact. It emits reconcile_start, reconcile_workspace,
// and reconcile_end task events so startup recovery is visible in the same
// event stream as normal task lifecycle activity.
func ReconcileStartup(ctx context.Context, cfg ReconcileConfig) error {
	if err := validateReconcileConfig(cfg); err != nil {
		return err
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

	fetch, skip := fetchReconcileIssues(ctx, cfg, taskID, nonEmptyStates(cfg.ActiveStates), nonEmptyStates(cfg.TerminalStates))
	if skip {
		return nil
	}
	idx := newReconcileIndex(fetch)

	workspaces, err := listIssueWorkspaces(cfg.WorkspaceRoot, cfg.TrackerKind)
	if err != nil {
		return err
	}

	removed, kept, err := reconcileWorkspaces(ctx, cfg, taskID, workspaces, idx)
	if err != nil {
		return err
	}

	Emit(ctx, cfg.Emitter, taskID, "", task.EventReconcileEnd, "startup reconciliation finished", reconcileEndPayload(fetch, kept, removed))
	return nil
}

// validateReconcileConfig enforces the inputs ReconcileStartup cannot proceed
// without: a workspace root, a tracker, and at least one active and terminal
// state to classify workspaces against.
func validateReconcileConfig(cfg ReconcileConfig) error {
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
	return nil
}

// reconcileFetch holds the tracker issue lists gathered at startup and whether
// the terminal fetch succeeded.
type reconcileFetch struct {
	activeIssues     []tracker.Issue
	terminalIssues   []tracker.Issue
	terminalFetchOK  bool
	terminalFetchErr error
}

// fetchReconcileIssues fetches the active then terminal issue lists.
//
// SPEC §8.6 / §11.4: transient tracker outages during boot must log a warning
// and continue startup, not abort the worker. An active-fetch failure is the
// worst case — without the active list we cannot confirm any workspace is safe
// to delete — so it emits reconcile_end with `status: skipped` and returns
// skip=true, leaving every workspace intact. A terminal-fetch failure is
// non-fatal: active and unmatched workspaces are still kept, while terminal
// cleanup is skipped because no terminal issue identifiers were returned.
func fetchReconcileIssues(ctx context.Context, cfg ReconcileConfig, taskID string, activeStates, terminalStates []string) (reconcileFetch, bool) {
	activeIssues, err := cfg.Tracker.ListIssuesByStates(ctx, activeStates)
	if err != nil {
		LogReconcileEventf("startup_reconcile_active_fetch_failed", "error=%q note=%q", err, "SPEC §8.6: log and continue; no cleanup performed")
		Emit(ctx, cfg.Emitter, taskID, "", task.EventReconcileEnd, "startup reconciliation skipped", map[string]any{
			"status": "skipped",
			"reason": "active_fetch_failed",
			"error":  err.Error(),
		})
		return reconcileFetch{}, true
	}
	fetch := reconcileFetch{activeIssues: activeIssues, terminalFetchOK: true}
	terminalIssues, err := cfg.Tracker.ListIssuesByStates(ctx, terminalStates)
	if err != nil {
		LogReconcileEventf("startup_reconcile_terminal_fetch_failed", "error=%q note=%q", err, "SPEC §8.6: log and continue; terminal cleanup skipped")
		fetch.terminalFetchOK = false
		fetch.terminalFetchErr = err
		return fetch, false
	}
	fetch.terminalIssues = terminalIssues
	return fetch, false
}

// reconcileIndex is the per-workspace lookup state derived from the fetched
// issues: active/terminal key maps and the issues used to match historical
// rework workspace keys.
type reconcileIndex struct {
	activeKeys     map[string]tracker.Issue
	terminalKeys   map[string]tracker.Issue
	activeIssues   []tracker.Issue
	terminalIssues []tracker.Issue
}

func newReconcileIndex(fetch reconcileFetch) reconcileIndex {
	activeKeys := make(map[string]tracker.Issue, len(fetch.activeIssues))
	for _, issue := range fetch.activeIssues {
		for _, key := range issueWorkspaceKeys(issue) {
			activeKeys[key] = issue
		}
	}
	terminalKeys := make(map[string]tracker.Issue, len(fetch.terminalIssues))
	for _, issue := range fetch.terminalIssues {
		for _, key := range issueWorkspaceKeys(issue) {
			terminalKeys[key] = issue
		}
	}
	return reconcileIndex{
		activeKeys:     activeKeys,
		terminalKeys:   terminalKeys,
		activeIssues:   fetch.activeIssues,
		terminalIssues: fetch.terminalIssues,
	}
}

// reconcileWorkspaces applies the keep/remove decision to each workspace and
// returns the removed/kept tallies. A workspace whose removal is declined (e.g.
// a failing before-remove hook) is counted as neither removed nor kept.
func reconcileWorkspaces(ctx context.Context, cfg ReconcileConfig, taskID string, workspaces []issueWorkspace, idx reconcileIndex) (removed, kept int, err error) {
	for _, workspace := range workspaces {
		if err := ctx.Err(); err != nil {
			return removed, kept, err
		}
		removedOne, keptOne, err := reconcileWorkspace(ctx, cfg, taskID, workspace, idx)
		if err != nil {
			return removed, kept, err
		}
		if removedOne {
			removed++
		}
		if keptOne {
			kept++
		}
	}
	return removed, kept, nil
}

// reconcileWorkspace classifies a single workspace and keeps or removes it,
// returning whether it was removed and/or kept (both false when removal was
// declined). Mirrors SPEC §8.6: keep active (exact key or rework), remove a
// tracker-confirmed terminal workspace, and keep unmatched workspaces.
func reconcileWorkspace(ctx context.Context, cfg ReconcileConfig, taskID string, workspace issueWorkspace, idx reconcileIndex) (removedOne, keptOne bool, err error) {
	if _, ok := idx.activeKeys[workspace.Key]; ok {
		Emit(ctx, cfg.Emitter, taskID, "", task.EventReconcileWorkspace, "kept active workspace", map[string]any{
			"path":   workspace.Path,
			"key":    workspace.Key,
			"action": "keep",
			"reason": "active",
		})
		return false, true, nil
	}
	if activeIssue, ok := activeReworkIssueForWorkspace(workspace.Key, idx.activeIssues); ok {
		Emit(ctx, cfg.Emitter, taskID, "", task.EventReconcileWorkspace, "kept active workspace", map[string]any{
			"path":       workspace.Path,
			"key":        workspace.Key,
			"issue_id":   activeIssue.ID,
			"identifier": activeIssue.Identifier,
			"action":     "keep",
			"reason":     "active_rework",
		})
		return false, true, nil
	}
	if issue, ok := idx.terminalKeys[workspace.Key]; ok {
		removedOne, err = removeWorkspace(ctx, cfg, taskID, workspace.Path, issue, "terminal")
		return removedOne, false, err
	}
	if issue, ok := terminalReworkIssueForWorkspace(workspace.Key, idx.terminalIssues); ok {
		removedOne, err = removeWorkspace(ctx, cfg, taskID, workspace.Path, issue, "terminal")
		return removedOne, false, err
	}
	Emit(ctx, cfg.Emitter, taskID, "", task.EventReconcileWorkspace, "kept unknown workspace", map[string]any{
		"path":   workspace.Path,
		"key":    workspace.Key,
		"action": "keep",
		"reason": "unknown_terminal_state_unconfirmed",
	})
	return false, true, nil
}

// reconcileEndPayload assembles the reconcile_end event payload, recording a
// partial status (with the fetch error) when the terminal fetch failed.
func reconcileEndPayload(fetch reconcileFetch, kept, removed int) map[string]any {
	endPayload := map[string]any{
		"active_issues":   len(fetch.activeIssues),
		"terminal_issues": len(fetch.terminalIssues),
		"kept":            kept,
		"removed":         removed,
	}
	if !fetch.terminalFetchOK {
		endPayload["status"] = "partial"
		endPayload["reason"] = "terminal_fetch_failed"
		if fetch.terminalFetchErr != nil {
			endPayload["error"] = fetch.terminalFetchErr.Error()
		}
	} else {
		endPayload["status"] = "ok"
	}
	return endPayload
}

type issueWorkspace struct {
	Path string
	Key  string
}

// listIssueWorkspaces walks the workspace root and returns one issueWorkspace
// per per-issue directory, in owner->repo->sourceDir->entry traversal order.
// It returns (nil, nil) when the root does not exist and short-circuits on the
// first owner/source read error in traversal order.
func listIssueWorkspaces(root, trackerKind string) ([]issueWorkspace, error) {
	ownerEntries, err := readWorkspaceRootEntries(root)
	if err != nil {
		return nil, err
	}
	if ownerEntries == nil {
		return nil, nil
	}
	sourceDirs := issueWorkspaceSourceDirs(trackerKind)
	var workspaces []issueWorkspace
	for _, ownerEntry := range ownerEntries {
		if !ownerEntry.IsDir() {
			continue
		}
		ownerWorkspaces, err := collectOwnerIssueWorkspaces(filepath.Join(root, ownerEntry.Name()), sourceDirs)
		if err != nil {
			return nil, err
		}
		workspaces = append(workspaces, ownerWorkspaces...)
	}
	return workspaces, nil
}

// readWorkspaceRootEntries reads the workspace root. A non-existent root yields
// (nil, nil) so reconciliation no-ops before any workspaces are created; any
// other read error is wrapped.
func readWorkspaceRootEntries(root string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read workspace root %s: %w", root, err)
	}
	return entries, nil
}

// collectOwnerIssueWorkspaces reads one owner directory and gathers the issue
// workspaces under each of its repo directories. Unlike the source-dir level,
// an owner read error is always fatal (no os.IsNotExist special-case).
func collectOwnerIssueWorkspaces(ownerPath string, sourceDirs []string) ([]issueWorkspace, error) {
	repoEntries, err := os.ReadDir(ownerPath)
	if err != nil {
		return nil, fmt.Errorf("read workspace owner %s: %w", ownerPath, err)
	}
	var workspaces []issueWorkspace
	for _, repoEntry := range repoEntries {
		if !repoEntry.IsDir() {
			continue
		}
		repoWorkspaces, err := collectRepoIssueWorkspaces(filepath.Join(ownerPath, repoEntry.Name()), sourceDirs)
		if err != nil {
			return nil, err
		}
		workspaces = append(workspaces, repoWorkspaces...)
	}
	return workspaces, nil
}

// collectRepoIssueWorkspaces gathers the issue workspaces under one repo
// directory across all candidate source dirs, in source-dir declaration order.
func collectRepoIssueWorkspaces(repoPath string, sourceDirs []string) ([]issueWorkspace, error) {
	var workspaces []issueWorkspace
	for _, sourceDir := range sourceDirs {
		sourceWorkspaces, err := collectSourceDirIssueWorkspaces(filepath.Join(repoPath, sourceDir))
		if err != nil {
			return nil, err
		}
		workspaces = append(workspaces, sourceWorkspaces...)
	}
	return workspaces, nil
}

// collectSourceDirIssueWorkspaces reads one candidate source dir and returns
// its per-issue workspace directories. A missing source dir yields (nil, nil)
// so the caller skips it (os.IsNotExist); any other read error is wrapped.
func collectSourceDirIssueWorkspaces(sourcePath string) ([]issueWorkspace, error) {
	workspaceEntries, err := os.ReadDir(sourcePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read issue workspace source %s: %w", sourcePath, err)
	}
	var workspaces []issueWorkspace
	for _, workspaceEntry := range workspaceEntries {
		if !workspaceEntry.IsDir() {
			continue
		}
		path := filepath.Join(sourcePath, workspaceEntry.Name())
		workspaces = append(workspaces, issueWorkspace{Path: path, Key: workspaceEntry.Name()})
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
	return RemoveIssueWorkspace(ctx, cfg.Emitter, RemoveWorkspaceRequest{
		WorkspaceRoot:      cfg.WorkspaceRoot,
		TaskID:             taskID,
		Path:               path,
		IssueID:            issue.ID,
		Identifier:         issue.Identifier,
		State:              issue.State,
		Reason:             reason,
		WorkflowConfig:     cfg.WorkflowConfig,
		BeforeRemoveHook:   cfg.BeforeRemoveHook,
		HookTimeoutMillis:  cfg.HookTimeoutMillis,
		HookEnvPassthrough: cfg.HookEnvPassthrough,
	})
}

// RemoveWorkspaceRequest carries the inputs for a single per-issue workspace
// removal through the shared before_remove → SafeRemove → reconcile_workspace
// event sequence.
type RemoveWorkspaceRequest struct {
	WorkspaceRoot      string
	TaskID             string
	Path               string
	IssueID            string
	Identifier         string
	State              string
	Reason             string
	WorkflowConfig     workflow.Config
	BeforeRemoveHook   workflow.WorkspaceHook
	HookTimeoutMillis  int
	HookEnvPassthrough []string
}

// RemoveIssueWorkspace runs the before_remove hook (best effort: a hook
// failure is logged but does not abort removal), removes the workspace
// directory via SafeRemove, then emits a reconcile_workspace remove event.
// It is the single removal routine shared by the startup sweep
// (ReconcileStartup) and the SPEC §18.1 active-transition cleanup the
// orchestrator triggers when a running issue moves to a terminal state
// mid-run, so both honor the same hook and event contract — mirroring
// upstream Workspace.remove_issue_workspaces, which both paths also share.
// It returns true when the directory was removed.
func RemoveIssueWorkspace(ctx context.Context, ev EventEmitter, req RemoveWorkspaceRequest) (bool, error) {
	if err := runWorkspaceHook(ctx, ev, req.TaskID, req.Identifier, req.Path, workspace.HookBeforeRemove, req.BeforeRemoveHook, req.HookTimeoutMillis, req.HookEnvPassthrough, req.WorkflowConfig); err != nil {
		log.Printf("event=before_remove_hook_failed task_id=%s issue_id=%s issue_identifier=%s reason=%s workspace=%q error=%q", req.TaskID, req.IssueID, req.Identifier, req.Reason, req.Path, err)
	}
	if err := workspace.SafeRemove(req.WorkspaceRoot, req.Path); err != nil {
		return false, fmt.Errorf("remove %s workspace %s: %w", req.Reason, req.Path, err)
	}
	if err := runner.RemoveSandboxGoBuildCache(req.Path); err != nil {
		log.Printf("event=go_build_cache_remove_failed task_id=%s issue_id=%s issue_identifier=%s reason=%s workspace=%q error=%q", req.TaskID, req.IssueID, req.Identifier, req.Reason, req.Path, err)
	}
	Emit(ctx, ev, req.TaskID, "", task.EventReconcileWorkspace, "removed workspace", map[string]any{
		"path":       req.Path,
		"issue_id":   req.IssueID,
		"identifier": req.Identifier,
		"state":      req.State,
		"action":     "remove",
		"reason":     req.Reason,
	})
	return true, nil
}

func activeReworkIssueForWorkspace(workspaceKey string, issues []tracker.Issue) (tracker.Issue, bool) {
	for _, issue := range issues {
		if !strings.EqualFold(issue.State, "Rework") || strings.TrimSpace(issue.ID) == "" {
			continue
		}
		if reworkWorkspaceMatchesIssue(workspaceKey, issue) {
			return issue, true
		}
	}
	return tracker.Issue{}, false
}

func terminalReworkIssueForWorkspace(workspaceKey string, issues []tracker.Issue) (tracker.Issue, bool) {
	for _, issue := range issues {
		if strings.TrimSpace(issue.ID) == "" {
			continue
		}
		if reworkWorkspaceMatchesIssue(workspaceKey, issue) {
			return issue, true
		}
	}
	return tracker.Issue{}, false
}

func reworkWorkspaceMatchesIssue(workspaceKey string, issue tracker.Issue) bool {
	for _, prefix := range reworkWorkspaceKeyPrefixes(issue) {
		if strings.HasPrefix(workspaceKey, prefix) {
			return true
		}
	}
	return false
}

func reworkWorkspaceKeyPrefixes(issue tracker.Issue) []string { //nolint:gocognit // baseline (#521)
	seen := map[string]struct{}{}
	var prefixes []string
	baseKeys := []string{workspace.SanitizeComponent(issue.ID), sanitizeLegacyWorkspaceKey(issue.ID)}
	for _, key := range issueWorkspaceKeys(issue) {
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
		// Emit two prefix forms so reconciliation recognizes Rework
		// workspaces from every sanitizer vintage that aiops-platform
		// has shipped:
		//   1. `<base>_rework_…` — current SPEC §4.2 sanitizer, which
		//      maps `|` and `:` to `_` and preserves case.
		//   2. `<base>-rework-…` — interim/pre-#229 layout where the base
		//      was already case-preserved (or already lowercase) and the
		//      rework separator was the dash form left over from an
		//      earlier `_rework_`/`-rework-` split.
		// #679 removed a speculative third `<lowercased-base>-rework-…`
		// form (the pre-#229 sanitizer lowercased the key): for the Linear,
		// Gitea, and GitHub trackers shipped today the Rework key is
		// composed from `issue.ID` — an all-lowercase UUID or numeric value
		// — so form 2 already covers every directory shape any released
		// worker actually wrote to disk, and form 3 never matched a real
		// directory. Re-add it only when a tracker actually emits an
		// `issue.ID` with uppercase or `[^a-zA-Z0-9._-]` characters (an
		// earned rule with a real failure behind it).
		for _, prefix := range []string{
			key + "_rework_",
			key + "-rework-",
		} {
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

func workspaceKeysForRawIssueKeys(issue tracker.Issue, rawKeys []string) []string { //nolint:gocognit // baseline (#521)
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

func sanitizeLegacyWorkspaceKey(s string) string { //nolint:gocognit // baseline (#521)
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

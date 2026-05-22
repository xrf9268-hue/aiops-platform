package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

type fakeReconcileTracker struct {
	issues []tracker.Issue
	err    error
}

func (f fakeReconcileTracker) ListIssuesByStates(_ context.Context, states []string) ([]tracker.Issue, error) {
	if f.err != nil {
		return nil, f.err
	}
	want := map[string]struct{}{}
	for _, state := range states {
		want[state] = struct{}{}
	}
	var out []tracker.Issue
	for _, issue := range f.issues {
		if _, ok := want[issue.State]; ok {
			out = append(out, issue)
		}
	}
	return out, nil
}

type fakeReconcileTrackerByCall struct {
	issuesByCall [][]tracker.Issue
	errByCall    []error
	calls        int
}

func (f *fakeReconcileTrackerByCall) ListIssuesByStates(_ context.Context, _ []string) ([]tracker.Issue, error) {
	call := f.calls
	f.calls++
	if call < len(f.errByCall) && f.errByCall[call] != nil {
		return nil, f.errByCall[call]
	}
	if call < len(f.issuesByCall) {
		return f.issuesByCall[call], nil
	}
	return nil, nil
}

func TestReconcileStartupRemovesTerminalWorkspaces(t *testing.T) {
	root := t.TempDir()
	activePath := filepath.Join(root, "acme", "repo", "linear_issue", "LIN-1")
	terminalPath := filepath.Join(root, "acme", "repo", "linear_issue", "LIN-2")
	for _, path := range []string{activePath, terminalPath} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
	}

	emitter := &fakeEmitter{}
	err := ReconcileStartup(context.Background(), ReconcileConfig{
		WorkspaceRoot:   root,
		ActiveStates:    []string{"AI Ready", "In Progress", "Rework"},
		TerminalStates:  []string{"Done", "Canceled"},
		Tracker:         fakeReconcileTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"}, {ID: "issue-2", Identifier: "LIN-2", State: "Done"}}},
		Emitter:         emitter,
		ReconcileTaskID: "reconcile-startup",
	})
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if _, err := os.Stat(activePath); err != nil {
		t.Fatalf("active workspace should remain: %v", err)
	}
	if _, err := os.Stat(terminalPath); !os.IsNotExist(err) {
		t.Fatalf("terminal workspace should be removed, stat err=%v", err)
	}
	if got := len(emitter.byKind(task.EventReconcileStart)); got != 1 {
		t.Fatalf("reconcile_start events = %d, want 1", got)
	}
	if got := len(emitter.byKind(task.EventReconcileWorkspace)); got != 2 {
		t.Fatalf("reconcile_workspace events = %d, want 2", got)
	}
	if got := len(emitter.byKind(task.EventReconcileEnd)); got != 1 {
		t.Fatalf("reconcile_end events = %d, want 1", got)
	}
}

func TestReconcileStartupIsIdempotent(t *testing.T) {
	root := t.TempDir()
	terminalPath := filepath.Join(root, "acme", "repo", "linear_issue", "LIN-2")
	if err := os.MkdirAll(terminalPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := ReconcileConfig{
		WorkspaceRoot:   root,
		ActiveStates:    []string{"AI Ready"},
		TerminalStates:  []string{"Done"},
		Tracker:         fakeReconcileTracker{issues: []tracker.Issue{{Identifier: "LIN-2", State: "Done"}}},
		Emitter:         &fakeEmitter{},
		ReconcileTaskID: "reconcile-startup",
	}
	if err := ReconcileStartup(context.Background(), cfg); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if err := ReconcileStartup(context.Background(), cfg); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if _, err := os.Stat(terminalPath); !os.IsNotExist(err) {
		t.Fatalf("terminal workspace should stay removed, stat err=%v", err)
	}
}

func TestReconcileStartupRemovesUnknownWorkspacesWhenTerminalIssuesObserved(t *testing.T) {
	root := t.TempDir()
	unknownPath := filepath.Join(root, "acme", "repo", "linear_issue", "LIN-404")
	if err := os.MkdirAll(unknownPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	fake := &fakeReconcileTrackerByCall{issuesByCall: [][]tracker.Issue{
		{{ID: "issue-1", Identifier: "LIN-1", State: "AI Ready"}},
		{{ID: "issue-2", Identifier: "LIN-2", State: "Done"}},
	}}
	err := ReconcileStartup(context.Background(), ReconcileConfig{
		WorkspaceRoot:   root,
		ActiveStates:    []string{"AI Ready"},
		TerminalStates:  []string{"Done"},
		Tracker:         fake,
		Emitter:         &fakeEmitter{},
		ReconcileTaskID: "reconcile-startup",
	})
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if _, err := os.Stat(unknownPath); !os.IsNotExist(err) {
		t.Fatalf("unknown workspace should be removed after terminal state is observed, stat err=%v", err)
	}
}

func TestReconcileStartupRefusesToRemoveWhenTrackerReturnsNoIssues(t *testing.T) {
	root := t.TempDir()
	unknownPath := filepath.Join(root, "acme", "repo", "linear-issue", "lin-404")
	if err := os.MkdirAll(unknownPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	err := ReconcileStartup(context.Background(), ReconcileConfig{
		WorkspaceRoot:   root,
		ActiveStates:    []string{"AI Ready"},
		TerminalStates:  []string{"Done"},
		Tracker:         fakeReconcileTracker{},
		Emitter:         &fakeEmitter{},
		ReconcileTaskID: "reconcile-startup",
	})
	if err == nil || !strings.Contains(err.Error(), "tracker returned no active or terminal issues") {
		t.Fatalf("ReconcileStartup error = %v, want empty tracker safety error", err)
	}
	if _, err := os.Stat(unknownPath); err != nil {
		t.Fatalf("workspace should remain when tracker returns no issues: %v", err)
	}
}

// TestReconcileStartupTolerantOfActiveFetchFailure pins SPEC §8.6 / §11.4:
// a transient tracker outage during boot must NOT abort worker startup. The
// active-fetch failure is the worst case — without it we cannot confirm any
// workspace is safe to delete — so the helper logs, emits reconcile_end with
// skipped status, and returns nil, leaving every workspace intact.
func TestReconcileStartupTolerantOfActiveFetchFailure(t *testing.T) {
	root := t.TempDir()
	activePath := filepath.Join(root, "acme", "repo", "linear_issue", "LIN-1")
	terminalPath := filepath.Join(root, "acme", "repo", "linear_issue", "LIN-2")
	for _, path := range []string{activePath, terminalPath} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	emitter := &fakeEmitter{}
	err := ReconcileStartup(context.Background(), ReconcileConfig{
		WorkspaceRoot:   root,
		ActiveStates:    []string{"AI Ready"},
		TerminalStates:  []string{"Done"},
		Tracker:         &fakeReconcileTrackerByCall{errByCall: []error{errors.New("tracker outage"), nil}},
		Emitter:         emitter,
		ReconcileTaskID: "reconcile-startup",
	})
	if err != nil {
		t.Fatalf("ReconcileStartup must not fail on tracker outage: %v", err)
	}
	if _, err := os.Stat(activePath); err != nil {
		t.Fatalf("active workspace must remain when active fetch fails: %v", err)
	}
	if _, err := os.Stat(terminalPath); err != nil {
		t.Fatalf("any workspace must remain when active fetch fails (safety): %v", err)
	}
	endEvents := emitter.byKind(task.EventReconcileEnd)
	if len(endEvents) != 1 {
		t.Fatalf("reconcile_end events = %d, want 1", len(endEvents))
	}
	payload, ok := endEvents[0].Payload.(map[string]any)
	if !ok {
		t.Fatalf("reconcile_end payload = %T, want map", endEvents[0].Payload)
	}
	if status, _ := payload["status"].(string); status != "skipped" {
		t.Fatalf("reconcile_end status = %q, want \"skipped\"", status)
	}
	if reason, _ := payload["reason"].(string); reason != "active_fetch_failed" {
		t.Fatalf("reconcile_end reason = %q, want \"active_fetch_failed\"", reason)
	}
}

// TestReconcileStartupTolerantOfTerminalFetchFailure: terminal-fetch failure
// is non-fatal. Active workspaces are still kept; terminal/unknown removal is
// skipped because terminalKeys is empty and canRemoveUnknown stays false.
func TestReconcileStartupTolerantOfTerminalFetchFailure(t *testing.T) {
	root := t.TempDir()
	activePath := filepath.Join(root, "acme", "repo", "linear_issue", "LIN-1")
	maybeTerminalPath := filepath.Join(root, "acme", "repo", "linear_issue", "LIN-2")
	for _, path := range []string{activePath, maybeTerminalPath} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	emitter := &fakeEmitter{}
	err := ReconcileStartup(context.Background(), ReconcileConfig{
		WorkspaceRoot:  root,
		ActiveStates:   []string{"In Progress"},
		TerminalStates: []string{"Done"},
		Tracker: &fakeReconcileTrackerByCall{
			issuesByCall: [][]tracker.Issue{{{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"}}, nil},
			errByCall:    []error{nil, errors.New("tracker outage")},
		},
		Emitter:         emitter,
		ReconcileTaskID: "reconcile-startup",
	})
	if err != nil {
		t.Fatalf("ReconcileStartup must not fail on terminal-fetch outage: %v", err)
	}
	if _, err := os.Stat(activePath); err != nil {
		t.Fatalf("active workspace must remain: %v", err)
	}
	if _, err := os.Stat(maybeTerminalPath); err != nil {
		t.Fatalf("workspace must remain when terminal fetch failed (no confirmation it is safe to delete): %v", err)
	}
	endEvents := emitter.byKind(task.EventReconcileEnd)
	if len(endEvents) != 1 {
		t.Fatalf("reconcile_end events = %d, want 1", len(endEvents))
	}
	payload, _ := endEvents[0].Payload.(map[string]any)
	if status, _ := payload["status"].(string); status != "partial" {
		t.Fatalf("reconcile_end status = %q, want \"partial\"", status)
	}
	if reason, _ := payload["reason"].(string); reason != "terminal_fetch_failed" {
		t.Fatalf("reconcile_end reason = %q, want \"terminal_fetch_failed\"", reason)
	}
}

// TestReconcileStartupTolerantOfBothFetchFailures: both fail. Treated as the
// active-fetch failure path (most conservative), nothing deleted.
func TestReconcileStartupTolerantOfBothFetchFailures(t *testing.T) {
	root := t.TempDir()
	wsPath := filepath.Join(root, "acme", "repo", "linear_issue", "LIN-X")
	if err := os.MkdirAll(wsPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	err := ReconcileStartup(context.Background(), ReconcileConfig{
		WorkspaceRoot:   root,
		ActiveStates:    []string{"In Progress"},
		TerminalStates:  []string{"Done"},
		Tracker:         &fakeReconcileTrackerByCall{errByCall: []error{errors.New("active outage"), errors.New("terminal outage")}},
		Emitter:         &fakeEmitter{},
		ReconcileTaskID: "reconcile-startup",
	})
	if err != nil {
		t.Fatalf("ReconcileStartup must not fail on dual-fetch outage: %v", err)
	}
	if _, err := os.Stat(wsPath); err != nil {
		t.Fatalf("workspace must remain when both fetches fail: %v", err)
	}
}

func TestReconcileStartupMatchesCurrentSanitizedWorkspaceLayout(t *testing.T) {
	root := t.TempDir()
	// Workspace dir names follow SPEC §4.2 sanitization: case preserved,
	// `_` substituted for any character outside [A-Za-z0-9._-]. The rework
	// key adds a `|rework|<updatedAt>` segment before sanitization, which
	// turns into `_rework_<sanitized timestamp>`.
	activePath := filepath.Join(root, "acme", "repo", "linear-issue", "LIN_1_Needs_Fix")
	reworkPath := filepath.Join(root, "acme", "repo", "linear-issue", "issue-3_rework_2026-05-16T10_00_00Z")
	terminalPath := filepath.Join(root, "acme", "repo", "linear-issue", "LIN_2_Done")
	for _, path := range []string{activePath, reworkPath, terminalPath} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
	}

	err := ReconcileStartup(context.Background(), ReconcileConfig{
		WorkspaceRoot:  root,
		ActiveStates:   []string{"In Progress", "Rework"},
		TerminalStates: []string{"Done"},
		Tracker: fakeReconcileTracker{issues: []tracker.Issue{
			{ID: "issue-1", Identifier: "LIN 1 Needs/Fix", State: "In Progress"},
			{ID: "issue-3", Identifier: "LIN-3", State: "Rework", UpdatedAt: mustTime("2026-05-16T10:00:00Z")},
			{ID: "issue-2", Identifier: "LIN 2 Done", State: "Done"},
		}},
		Emitter:         &fakeEmitter{},
		ReconcileTaskID: "reconcile-startup",
	})
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if _, err := os.Stat(activePath); err != nil {
		t.Fatalf("active workspace in current layout should remain: %v", err)
	}
	if _, err := os.Stat(reworkPath); err != nil {
		t.Fatalf("active Rework workspace in current source_event_id layout should remain: %v", err)
	}
	if _, err := os.Stat(terminalPath); !os.IsNotExist(err) {
		t.Fatalf("terminal workspace in current layout should be removed, stat err=%v", err)
	}
}

func TestReconcileStartupMatchesGiteaWorkspaceLayout(t *testing.T) {
	root := t.TempDir()
	activePath := filepath.Join(root, "acme", "repo", "gitea_issue", "issue-1")
	terminalPath := filepath.Join(root, "acme", "repo", "gitea_issue", "issue-2")
	for _, path := range []string{activePath, terminalPath} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
	}

	err := ReconcileStartup(context.Background(), ReconcileConfig{
		WorkspaceRoot:  root,
		ActiveStates:   []string{"AI Ready"},
		TerminalStates: []string{"Done"},
		TrackerKind:    "gitea",
		Tracker: fakeReconcileTracker{issues: []tracker.Issue{
			{ID: "issue-1", Identifier: "GIT-1", State: "AI Ready"},
			{ID: "issue-2", Identifier: "GIT-2", State: "Done"},
		}},
		Emitter:         &fakeEmitter{},
		ReconcileTaskID: "reconcile-startup",
	})
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if _, err := os.Stat(activePath); err != nil {
		t.Fatalf("active gitea workspace should remain: %v", err)
	}
	if _, err := os.Stat(terminalPath); !os.IsNotExist(err) {
		t.Fatalf("terminal gitea workspace should be removed, stat err=%v", err)
	}
}

func TestReconcileStartupMatchesGitHubWorkspaceLayout(t *testing.T) {
	root := t.TempDir()
	activePath := filepath.Join(root, "acme", "repo", "github_issue", "issue-1")
	terminalPath := filepath.Join(root, "acme", "repo", "github_issue", "issue-2")
	for _, path := range []string{activePath, terminalPath} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
	}

	err := ReconcileStartup(context.Background(), ReconcileConfig{
		WorkspaceRoot:  root,
		ActiveStates:   []string{"open"},
		TerminalStates: []string{"closed"},
		TrackerKind:    "github",
		Tracker: fakeReconcileTracker{issues: []tracker.Issue{
			{ID: "issue-1", Identifier: "#1", State: "open"},
			{ID: "issue-2", Identifier: "#2", State: "closed"},
		}},
		Emitter:         &fakeEmitter{},
		ReconcileTaskID: "reconcile-startup",
	})
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if _, err := os.Stat(activePath); err != nil {
		t.Fatalf("active GitHub workspace should remain: %v", err)
	}
	if _, err := os.Stat(terminalPath); !os.IsNotExist(err) {
		t.Fatalf("terminal GitHub workspace should be removed, stat err=%v", err)
	}
}

func TestReconcileStartupSkipsOtherTrackerWorkspaceLayouts(t *testing.T) {
	root := t.TempDir()
	linearTerminalPath := filepath.Join(root, "acme", "repo", "linear-issue", "lin-2-done")
	giteaOtherTrackerPath := filepath.Join(root, "acme", "repo", "gitea_issue", "issue-owned-by-gitea-worker")
	for _, path := range []string{linearTerminalPath, giteaOtherTrackerPath} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
	}

	err := ReconcileStartup(context.Background(), ReconcileConfig{
		WorkspaceRoot:   root,
		ActiveStates:    []string{"In Progress"},
		TerminalStates:  []string{"Done"},
		TrackerKind:     "linear",
		Tracker:         fakeReconcileTracker{issues: []tracker.Issue{{ID: "issue-2", Identifier: "LIN 2 Done", State: "Done"}}},
		Emitter:         &fakeEmitter{},
		ReconcileTaskID: "reconcile-startup",
	})
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if _, err := os.Stat(linearTerminalPath); !os.IsNotExist(err) {
		t.Fatalf("linear terminal workspace should be removed, stat err=%v", err)
	}
	if _, err := os.Stat(giteaOtherTrackerPath); err != nil {
		t.Fatalf("gitea workspace should be ignored by linear reconciliation: %v", err)
	}
}

func TestReconcileStartupKeepsReworkWorkspaceWhenUpdatedAtChanged(t *testing.T) {
	root := t.TempDir()
	reworkPath := filepath.Join(root, "acme", "repo", "linear-issue", "issue-3-rework-2026-05-16t10-00-00z")
	if err := os.MkdirAll(reworkPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	err := ReconcileStartup(context.Background(), ReconcileConfig{
		WorkspaceRoot:  root,
		ActiveStates:   []string{"Rework"},
		TerminalStates: []string{"Done"},
		Tracker: fakeReconcileTracker{issues: []tracker.Issue{
			{ID: "issue-3", Identifier: "LIN-3", State: "Rework", UpdatedAt: mustTime("2026-05-16T11:30:00Z")},
			{ID: "issue-2", Identifier: "LIN-2", State: "Done"},
		}},
		Emitter:         &fakeEmitter{},
		ReconcileTaskID: "reconcile-startup",
	})
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if _, err := os.Stat(reworkPath); err != nil {
		t.Fatalf("active Rework workspace should remain even when issue updatedAt changed: %v", err)
	}
}

func TestReconcileStartupKeepsReworkWorkspaceWithLegacyOffsetTimestampSuffix(t *testing.T) {
	root := t.TempDir()
	legacyOffsetPath := filepath.Join(root, "acme", "repo", "linear-issue", "issue-3-rework-2026-05-08t12-30-00-02-00")
	if err := os.MkdirAll(legacyOffsetPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	err := ReconcileStartup(context.Background(), ReconcileConfig{
		WorkspaceRoot:  root,
		ActiveStates:   []string{"Rework"},
		TerminalStates: []string{"Done"},
		Tracker: fakeReconcileTracker{issues: []tracker.Issue{
			{ID: "issue-3", Identifier: "LIN-3", State: "Rework", UpdatedAt: mustTime("2026-05-08T12:30:00+02:00")},
		}},
		Emitter:         &fakeEmitter{},
		ReconcileTaskID: "reconcile-startup",
	})
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if _, err := os.Stat(legacyOffsetPath); err != nil {
		t.Fatalf("active Rework workspace with legacy offset timestamp suffix should remain: %v", err)
	}
}

func TestReworkWorkspaceKeyPrefixesMatchCanonicalAndLegacyOffsetSuffixes(t *testing.T) {
	issue := tracker.Issue{ID: "issue-3", State: "Rework", UpdatedAt: mustTime("2026-05-08T12:30:00+02:00")}
	keys := func(tracker.Issue) []string {
		return []string{
			"issue-3-rework-2026-05-08t10-30-00z",
			"issue-3-rework-2026-05-08t12-30-00-02-00",
		}
	}

	// reworkWorkspaceKeyPrefixes emits both the canonical SPEC §4.2
	// `_rework_` form (produced by the current sanitizer) and the legacy
	// `-rework-` form, so reconciliation matches workspaces from either
	// vintage on disk.
	got := reworkWorkspaceKeyPrefixes(issue, keys)
	want := []string{"issue-3_rework_", "issue-3-rework-"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reworkWorkspaceKeyPrefixes = %#v, want %#v", got, want)
	}
}

func TestReconcileStartupRunsBeforeRemoveHookAndStillRemovesOnFailure(t *testing.T) {
	root := t.TempDir()
	terminalPath := filepath.Join(root, "acme", "repo", "linear-issue", "LIN-1")
	if err := os.MkdirAll(terminalPath, 0o755); err != nil {
		t.Fatalf("mkdir terminal workspace: %v", err)
	}
	marker := filepath.Join(root, "hook-ran")
	emitter := &fakeEmitter{}
	err := ReconcileStartup(context.Background(), ReconcileConfig{
		WorkspaceRoot:  root,
		ActiveStates:   []string{"AI Ready"},
		TerminalStates: []string{"Done"},
		TrackerKind:    "linear",
		Tracker:        fakeReconcileTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "Done"}}},
		Emitter:        emitter,
		BeforeRemoveHook: workflow.WorkspaceHook{Commands: []string{
			"printf before_remove > " + shellQuote(marker),
			"exit 9",
		}},
	})
	if err != nil {
		t.Fatalf("ReconcileStartup should ignore before_remove hook failure and remove workspace: %v", err)
	}
	if _, err := os.Stat(terminalPath); !os.IsNotExist(err) {
		t.Fatalf("terminal workspace should be removed despite before_remove failure; stat err=%v", err)
	}
	if body, err := os.ReadFile(marker); err != nil || string(body) != "before_remove" {
		t.Fatalf("before_remove hook marker = %q, %v; want hook to run before removal", body, err)
	}
	var hookEnds int
	for _, e := range emitter.events {
		if e.Kind == task.EventWorkspaceHookEnd {
			hookEnds++
		}
	}
	if hookEnds != 2 {
		t.Fatalf("hook_end events = %d, want 2 for both before_remove commands; events=%#v", hookEnds, emitter.events)
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func TestReconcileStartupRejectsEmptyActiveStatesBeforeCleanup(t *testing.T) {
	root := t.TempDir()
	workspacePath := filepath.Join(root, "acme", "repo", "linear-issue", "lin-1")
	if err := os.MkdirAll(workspacePath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	err := ReconcileStartup(context.Background(), ReconcileConfig{
		WorkspaceRoot:   root,
		TerminalStates:  []string{"Done"},
		Tracker:         fakeReconcileTracker{},
		Emitter:         &fakeEmitter{},
		ReconcileTaskID: "reconcile-startup",
	})
	if err == nil || !strings.Contains(err.Error(), "active states are required") {
		t.Fatalf("ReconcileStartup error = %v, want active states config error", err)
	}
	if _, statErr := os.Stat(workspacePath); statErr != nil {
		t.Fatalf("workspace should remain when config is unsafe: %v", statErr)
	}
}

func TestReconcileStartupRejectsEmptyTerminalStatesBeforeCleanup(t *testing.T) {
	root := t.TempDir()
	workspacePath := filepath.Join(root, "acme", "repo", "linear-issue", "lin-1")
	if err := os.MkdirAll(workspacePath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	err := ReconcileStartup(context.Background(), ReconcileConfig{
		WorkspaceRoot:   root,
		ActiveStates:    []string{"AI Ready"},
		Tracker:         fakeReconcileTracker{},
		Emitter:         &fakeEmitter{},
		ReconcileTaskID: "reconcile-startup",
	})
	if err == nil || !strings.Contains(err.Error(), "terminal states are required") {
		t.Fatalf("ReconcileStartup error = %v, want terminal states config error", err)
	}
	if _, statErr := os.Stat(workspacePath); statErr != nil {
		t.Fatalf("workspace should remain when config is unsafe: %v", statErr)
	}
}

func TestReconcileStartupHandlesSourceEventIDAndTaskIDWorkspaceLayouts(t *testing.T) {
	root := t.TempDir()
	linearPath := filepath.Join(root, "acme", "repo", "linear_issue", "issue-uuid")
	legacyTaskPath := filepath.Join(root, "acme", "repo", "tsk_123")
	for _, path := range []string{linearPath, legacyTaskPath} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
	}

	err := ReconcileStartup(context.Background(), ReconcileConfig{
		WorkspaceRoot:   root,
		ActiveStates:    []string{"AI Ready"},
		TerminalStates:  []string{"Done"},
		Tracker:         fakeReconcileTracker{issues: []tracker.Issue{{ID: "issue-uuid", Identifier: "LIN-123", State: "Done"}}},
		Emitter:         &fakeEmitter{},
		ReconcileTaskID: "reconcile-startup",
	})
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if _, err := os.Stat(linearPath); !os.IsNotExist(err) {
		t.Fatalf("linear_issue/source_event_id workspace should be removed, stat err=%v", err)
	}
	if _, err := os.Stat(legacyTaskPath); err != nil {
		t.Fatalf("task-id workspace should be ignored by reconciliation: %v", err)
	}
}

func TestReconcileStartupKeepsWorkspaceWhenActiveAndTerminalKeysConflict(t *testing.T) {
	root := t.TempDir()
	// SPEC §4.2 sanitization of "same key" → "same_key" (space → `_`).
	workspacePath := filepath.Join(root, "acme", "repo", "linear-issue", "same_key")
	if err := os.MkdirAll(workspacePath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	fake := &fakeReconcileTrackerByCall{issuesByCall: [][]tracker.Issue{
		{{ID: "active", Identifier: "same key", State: "In Progress"}},
		{{ID: "terminal", Identifier: "same key", State: "Done"}},
	}}
	err := ReconcileStartup(context.Background(), ReconcileConfig{
		WorkspaceRoot:   root,
		ActiveStates:    []string{"In Progress"},
		TerminalStates:  []string{"Done"},
		Tracker:         fake,
		Emitter:         &fakeEmitter{},
		ReconcileTaskID: "reconcile-startup",
	})
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if _, err := os.Stat(workspacePath); err != nil {
		t.Fatalf("conflicting active workspace should be preserved: %v", err)
	}
}

func TestReconcileStartupKeepsUnknownWorkspaceWhenTrackerHasActiveIssuesOnly(t *testing.T) {
	root := t.TempDir()
	unknownPath := filepath.Join(root, "acme", "repo", "linear-issue", "lin-unknown")
	activePath := filepath.Join(root, "acme", "repo", "linear-issue", "lin-active")
	for _, path := range []string{unknownPath, activePath} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
	}

	err := ReconcileStartup(context.Background(), ReconcileConfig{
		WorkspaceRoot:   root,
		ActiveStates:    []string{"In Progress"},
		TerminalStates:  []string{"Done"},
		Tracker:         fakeReconcileTracker{issues: []tracker.Issue{{ID: "active", Identifier: "LIN Active", State: "In Progress"}}},
		Emitter:         &fakeEmitter{},
		ReconcileTaskID: "reconcile-startup",
	})
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if _, err := os.Stat(activePath); err != nil {
		t.Fatalf("active workspace should remain: %v", err)
	}
	if _, err := os.Stat(unknownPath); err != nil {
		t.Fatalf("unknown workspace should remain when terminal query is empty: %v", err)
	}
}

func mustTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		panic(err)
	}
	return parsed
}

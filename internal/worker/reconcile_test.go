package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/runner"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
	"github.com/xrf9268-hue/aiops-platform/internal/workspace"
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
		ActiveStates:    []string{"Todo", "In Progress", "Rework"},
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

func TestRemoveIssueWorkspaceRemovesGoBuildCache(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	root := t.TempDir()
	workdir := filepath.Join(root, "acme", "repo", "linear_issue", "LIN-2")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatalf("mkdir workspace %s: %v", workdir, err)
	}
	cache := runner.SandboxGoBuildCachePath(workdir)
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatalf("mkdir Go build cache %s: %v", cache, err)
	}

	removed, err := RemoveIssueWorkspace(context.Background(), &fakeEmitter{}, RemoveWorkspaceRequest{
		WorkspaceRoot: root,
		TaskID:        "reconcile-startup",
		Path:          workdir,
		IssueID:       "issue-2",
		Identifier:    "LIN-2",
		State:         "Done",
		Reason:        "terminal",
	})
	if err != nil {
		t.Fatalf("RemoveIssueWorkspace() err = %v; want nil", err)
	}
	if !removed {
		t.Fatalf("RemoveIssueWorkspace() removed = %v; want true", removed)
	}
	if _, err := os.Stat(workdir); !os.IsNotExist(err) {
		t.Fatalf("removed workspace stat err = %v; want not exist", err)
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Fatalf("removed Go build cache stat err = %v; want not exist", err)
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
		ActiveStates:    []string{"Todo"},
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

func TestReconcileStartupRemovesOnlyTrackerConfirmedTerminalWorkspace(t *testing.T) {
	root := t.TempDir()
	terminalPath := filepath.Join(root, "acme", "repo", "linear_issue", "LIN-2")
	unknownPath := filepath.Join(root, "acme", "repo", "linear_issue", "LIN-404")
	for _, path := range []string{terminalPath, unknownPath} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
	}

	fake := &fakeReconcileTrackerByCall{issuesByCall: [][]tracker.Issue{
		nil,
		{{ID: "issue-2", Identifier: "LIN-2", State: "Done"}},
	}}
	err := ReconcileStartup(context.Background(), ReconcileConfig{
		WorkspaceRoot:   root,
		ActiveStates:    []string{"Todo"},
		TerminalStates:  []string{"Done"},
		Tracker:         fake,
		Emitter:         &fakeEmitter{},
		ReconcileTaskID: "reconcile-startup",
	})
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if _, err := os.Stat(terminalPath); !os.IsNotExist(err) {
		t.Fatalf("tracker-confirmed terminal workspace should be removed, stat err=%v", err)
	}
	if _, err := os.Stat(unknownPath); err != nil {
		t.Fatalf("unmatched workspace should remain without terminal-state confirmation: %v", err)
	}
}

func TestReconcileStartupRemovesTrackerConfirmedTerminalReworkWorkspaces(t *testing.T) {
	tests := []struct {
		name         string
		key          string
		unmatchedKey string
	}{
		{
			name:         "current sanitizer",
			key:          "issue-2_rework_2026-05-16T10_00_00Z",
			unmatchedKey: "issue-404_rework_2026-05-16T10_00_00Z",
		},
		{
			name:         "legacy sanitizer",
			key:          "issue-2-rework-2026-05-16t10-00-00z",
			unmatchedKey: "issue-404-rework-2026-05-16t10-00-00z",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			terminalPath := filepath.Join(root, "acme", "repo", "linear_issue", tt.key)
			unmatchedPath := filepath.Join(root, "acme", "repo", "linear_issue", tt.unmatchedKey)
			for _, path := range []string{terminalPath, unmatchedPath} {
				if err := os.MkdirAll(path, 0o755); err != nil {
					t.Fatalf("mkdir %s: %v", path, err)
				}
			}

			emitter := &fakeEmitter{}
			err := ReconcileStartup(context.Background(), ReconcileConfig{
				WorkspaceRoot:  root,
				ActiveStates:   []string{"Rework"},
				TerminalStates: []string{"Done"},
				Tracker: &fakeReconcileTrackerByCall{issuesByCall: [][]tracker.Issue{
					nil,
					{{ID: "issue-2", Identifier: "LIN-2", State: "Done"}},
				}},
				Emitter:         emitter,
				ReconcileTaskID: "reconcile-startup",
			})
			if err != nil {
				t.Fatalf("ReconcileStartup: %v", err)
			}
			if _, err := os.Stat(terminalPath); !os.IsNotExist(err) {
				t.Fatalf("tracker-confirmed terminal Rework workspace should be removed, stat err=%v", err)
			}
			if _, err := os.Stat(unmatchedPath); err != nil {
				t.Fatalf("unmatched Rework workspace should remain: %v", err)
			}
			wantReconcileWorkspaceReasonForPath(t, emitter, terminalPath, "terminal")
			wantReconcileWorkspaceReasonForPath(t, emitter, unmatchedPath, "unknown_terminal_state_unconfirmed")
		})
	}
}

func TestReconcileStartupDoesNotIndexBlankTerminalIssueKeys(t *testing.T) {
	tests := []struct {
		name            string
		issueID         string
		currentFallback string
	}{
		{name: "empty", issueID: "", currentFallback: "unknown"},
		{name: "whitespace", issueID: "   ", currentFallback: "___"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			confirmedPath := filepath.Join(root, "acme", "repo", "linear_issue", "LIN-2")
			currentFallbackPath := filepath.Join(root, "acme", "repo", "linear_issue", tt.currentFallback)
			legacyFallbackPath := filepath.Join(root, "acme", "repo", "linear_issue", "workspace")
			currentReworkFallbackPath := filepath.Join(root, "acme", "repo", "linear_issue", tt.currentFallback+"_rework_2026-05-16T10_00_00Z")
			legacyReworkFallbackPath := filepath.Join(root, "acme", "repo", "linear_issue", "workspace-rework-2026-05-16t10-00-00z")
			fallbackPaths := []string{currentFallbackPath, legacyFallbackPath, currentReworkFallbackPath, legacyReworkFallbackPath}
			for _, path := range append([]string{confirmedPath}, fallbackPaths...) {
				if err := os.MkdirAll(path, 0o755); err != nil {
					t.Fatalf("mkdir %s: %v", path, err)
				}
			}

			emitter := &fakeEmitter{}
			err := ReconcileStartup(context.Background(), ReconcileConfig{
				WorkspaceRoot:  root,
				ActiveStates:   []string{"Todo"},
				TerminalStates: []string{"Done"},
				Tracker: &fakeReconcileTrackerByCall{issuesByCall: [][]tracker.Issue{
					nil,
					{{ID: tt.issueID, Identifier: "LIN-2", State: "Done"}},
				}},
				Emitter:         emitter,
				ReconcileTaskID: "reconcile-startup",
			})
			if err != nil {
				t.Fatalf("ReconcileStartup: %v", err)
			}
			if _, err := os.Stat(confirmedPath); !os.IsNotExist(err) {
				t.Fatalf("identifier-confirmed terminal workspace should be removed, stat err=%v", err)
			}
			for _, path := range fallbackPaths {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("blank-key fallback workspace %s should remain: %v", path, err)
				}
				wantReconcileWorkspaceReasonForPath(t, emitter, path, "unknown_terminal_state_unconfirmed")
			}
		})
	}
}

func TestReconcileStartupKeepsWorkspacesWhenTrackerReturnsNoIssues(t *testing.T) {
	root := t.TempDir()
	unknownPath := filepath.Join(root, "acme", "repo", "linear-issue", "lin-404")
	if err := os.MkdirAll(unknownPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	emitter := &fakeEmitter{}
	err := ReconcileStartup(context.Background(), ReconcileConfig{
		WorkspaceRoot:   root,
		ActiveStates:    []string{"Todo"},
		TerminalStates:  []string{"Done"},
		Tracker:         fakeReconcileTracker{},
		Emitter:         emitter,
		ReconcileTaskID: "reconcile-startup",
	})
	if err != nil {
		t.Fatalf("ReconcileStartup must continue when tracker returns no issues: %v", err)
	}
	if _, err := os.Stat(unknownPath); err != nil {
		t.Fatalf("workspace should remain when tracker returns no issues: %v", err)
	}
	payload := reconcileEndPayloadFor(t, emitter)
	wantPayloadCount(t, payload, "kept", 1)
	wantPayloadCount(t, payload, "removed", 0)
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
		ActiveStates:    []string{"Todo"},
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

// TestReconcileStartupSafeWhenActiveListingCapped pins #401/#402: a partial
// active list (signaled by tracker.ErrIssueListingCapped) MUST be treated as a
// fetch failure so reconcile skips cleanup. This preserves the established
// fail-safe: no terminal workspace is removed from a startup snapshot whose
// active half is incomplete.
func TestReconcileStartupSafeWhenActiveListingCapped(t *testing.T) {
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
		ActiveStates:   []string{"Todo"},
		TerminalStates: []string{"Done"},
		Tracker: &fakeReconcileTrackerByCall{
			// Active fetch returns capped error; terminal fetch would otherwise
			// provide positive evidence for removing LIN-2.
			errByCall:    []error{tracker.ErrIssueListingCapped, nil},
			issuesByCall: [][]tracker.Issue{nil, {{ID: "issue-2", Identifier: "LIN-2", State: "Done"}}},
		},
		Emitter:         emitter,
		ReconcileTaskID: "reconcile-startup",
	})
	if err != nil {
		t.Fatalf("ReconcileStartup must not fail when active listing is capped: %v", err)
	}
	if _, err := os.Stat(activePath); err != nil {
		t.Fatalf("active workspace must remain when active listing is capped: %v", err)
	}
	if _, err := os.Stat(maybeTerminalPath); err != nil {
		t.Fatalf("any workspace must remain when active listing is capped (safety): %v", err)
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
	if errMsg, _ := payload["error"].(string); errMsg != tracker.ErrIssueListingCapped.Error() {
		t.Fatalf("reconcile_end error = %q, want %q", errMsg, tracker.ErrIssueListingCapped.Error())
	}
}

// TestReconcileStartupTolerantOfTerminalFetchFailure: terminal-fetch failure
// is non-fatal. Active workspaces are still kept and no terminal workspace is
// removed because the tracker returned no positive terminal-state evidence.
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

func TestReconcileStartupSafeWhenTerminalListingCapped(t *testing.T) {
	root := t.TempDir()
	activePath := filepath.Join(root, "acme", "repo", "linear_issue", "LIN-1")
	unmatchedPath := filepath.Join(root, "acme", "repo", "linear_issue", "LIN-9")
	for _, path := range []string{activePath, unmatchedPath} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
	}

	emitter := &fakeEmitter{}
	var reconcileErr error
	logs := captureLog(t, func() {
		reconcileErr = ReconcileStartup(context.Background(), ReconcileConfig{
			WorkspaceRoot:  root,
			ActiveStates:   []string{"In Progress"},
			TerminalStates: []string{"Done"},
			Tracker: &fakeReconcileTrackerByCall{
				issuesByCall: [][]tracker.Issue{{{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"}}, nil},
				errByCall:    []error{nil, tracker.ErrIssueListingCapped},
			},
			Emitter:         emitter,
			ReconcileTaskID: "reconcile-startup",
		})
	})
	if reconcileErr != nil {
		t.Fatalf("ReconcileStartup must not fail when terminal listing is capped: %v", reconcileErr)
	}
	if !strings.Contains(logs, "event=startup_reconcile_terminal_fetch_failed") || !strings.Contains(logs, "issue_listing_capped") {
		t.Fatalf("terminal listing warning log = %q, want event and issue_listing_capped category", logs)
	}
	for _, path := range []string{activePath, unmatchedPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("workspace %s must remain when terminal listing is capped: %v", path, err)
		}
	}
	payload := reconcileEndPayloadFor(t, emitter)
	if status, _ := payload["status"].(string); status != "partial" {
		t.Fatalf("reconcile_end status = %q, want \"partial\"", status)
	}
	if reason, _ := payload["reason"].(string); reason != "terminal_fetch_failed" {
		t.Fatalf("reconcile_end reason = %q, want \"terminal_fetch_failed\"", reason)
	}
	if errMsg, _ := payload["error"].(string); errMsg != tracker.ErrIssueListingCapped.Error() {
		t.Fatalf("reconcile_end error = %q, want %q", errMsg, tracker.ErrIssueListingCapped.Error())
	}
	wantPayloadCount(t, payload, "kept", 2)
	wantPayloadCount(t, payload, "removed", 0)
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
		ActiveStates:   []string{"Todo"},
		TerminalStates: []string{"Done"},
		TrackerKind:    "gitea",
		Tracker: fakeReconcileTracker{issues: []tracker.Issue{
			{ID: "issue-1", Identifier: "GIT-1", State: "Todo"},
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
	linearTerminalPath := filepath.Join(root, "acme", "repo", "linear-issue", "issue-2")
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

	// reworkWorkspaceKeyPrefixes emits two prefix forms so reconciliation
	// matches workspaces from every aiops-platform sanitizer vintage on disk:
	// the canonical SPEC §4.2 `_rework_` and the interim/pre-#229 case-preserved
	// `-rework-`. (#679 removed the speculative lowercased-base third form; for
	// an all-lowercase ID like "issue-3" it was always a duplicate of form 2.)
	got := reworkWorkspaceKeyPrefixes(issue)
	want := []string{"issue-3_rework_", "issue-3-rework-"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reworkWorkspaceKeyPrefixes = %#v, want %#v", got, want)
	}
}

// TestReconcileStartupKeepsPreSpecLowercasedReworkWorkspace pins the
// migration promise made in PR #290 / issue #229: a Rework workspace
// created by the pre-#229 sanitizer (lowercased input, `-` separators
// throughout, lowercased timestamp) must still be classified as
// `active_rework` on the first reconcile after the upgrade, instead of
// being misclassified when the terminal fetch also returns issues. The shipped
// trackers' Rework key is the
// all-lowercase `issue.ID`, so the case-preserving form 2 (`<id>-rework-`)
// matches the on-disk directory — this is exactly why #679 could drop the
// speculative lowercased-base third form without regressing the promise.
func TestReconcileStartupKeepsPreSpecLowercasedReworkWorkspace(t *testing.T) {
	root := t.TempDir()
	// Pre-#229 actual on-disk shape for a Linear Rework workspace, mirroring
	// what the pre-#229 sanitizer would have written
	// for `SourceEventID = "<issue.ID>|rework|<updatedAt>"`:
	//   - source-type subdir is `linear_issue` (pre-#229 `SanitizeSourceType`
	//     preserved `_`),
	//   - workspace key is `<issue.ID>-rework-<lowercased-timestamp>`
	//     (pre-#229 `SanitizeComponent` lowercased and collapsed `|` / `:`
	//     into `-`).
	preSpecPath := filepath.Join(root, "acme", "repo", "linear_issue", "issue-3-rework-2026-05-16t10-00-00z")
	if err := os.MkdirAll(preSpecPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	emitter := &fakeEmitter{}
	err := ReconcileStartup(context.Background(), ReconcileConfig{
		WorkspaceRoot:  root,
		ActiveStates:   []string{"Rework"},
		TerminalStates: []string{"Done"},
		Tracker: fakeReconcileTracker{issues: []tracker.Issue{
			{ID: "issue-3", Identifier: "LIN-123", State: "Rework", UpdatedAt: mustTime("2026-05-16T11:30:00Z")},
			{ID: "issue-2", Identifier: "LIN-2", State: "Done"},
		}},
		Emitter:         emitter,
		ReconcileTaskID: "reconcile-startup",
	})
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if _, err := os.Stat(preSpecPath); err != nil {
		t.Fatalf("pre-#229 Rework workspace should remain after the SPEC §4.2 cutover: %v", err)
	}
	wantSingleReconcileWorkspaceReason(t, emitter, "active_rework")
}

// TestReworkWorkspaceKeyPrefixesOmitsPreSpecLowercaseForm pins #679: the
// speculative pre-#229 lowercased-base `-rework-` form is no longer emitted, so
// even an uppercase Identifier yields only the case-preserving SPEC and interim
// forms. Form 2 already covers every shipped tracker, whose Rework key is built
// from an all-lowercase `issue.ID`.
func TestReworkWorkspaceKeyPrefixesOmitsPreSpecLowercaseForm(t *testing.T) {
	issue := tracker.Issue{ID: "issue-3", Identifier: "LIN-123", State: "Rework", UpdatedAt: mustTime("2026-05-16T10:00:00Z")}

	got := reworkWorkspaceKeyPrefixes(issue)
	for _, want := range []string{
		"LIN-123_rework_", // SPEC §4.2 form
		"LIN-123-rework-", // interim dash form
		"issue-3_rework_", // SPEC form for ID
		"issue-3-rework-", // interim form for ID
	} {
		if !containsStringWorker(got, want) {
			t.Fatalf("reworkWorkspaceKeyPrefixes = %#v, missing %q", got, want)
		}
	}
	if containsStringWorker(got, "lin-123-rework-") {
		t.Fatalf("reworkWorkspaceKeyPrefixes = %#v, must not emit the removed pre-#229 lowercased form %q", got, "lin-123-rework-")
	}
}

func containsStringWorker(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
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
		ActiveStates:   []string{"Todo"},
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

func TestReconcileStartupBeforeRemoveRejectsTrackerAPIKeyValuePassthrough(t *testing.T) {
	root := t.TempDir()
	terminalPath := filepath.Join(root, "acme", "repo", "linear-issue", "LIN-1")
	if err := os.MkdirAll(terminalPath, 0o755); err != nil {
		t.Fatalf("mkdir terminal workspace: %v", err)
	}
	t.Setenv("EXTRA_BUILD_VAR", "let-me-in")
	t.Setenv("AIOPS_TRACKER_SECRET", "reconcile-tracker-secret")
	marker := filepath.Join(root, "hook-env")

	err := ReconcileStartup(context.Background(), ReconcileConfig{
		WorkspaceRoot:      root,
		ActiveStates:       []string{"Todo"},
		TerminalStates:     []string{"Done"},
		TrackerKind:        "linear",
		Tracker:            fakeReconcileTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "Done"}}},
		WorkflowConfig:     workflow.Config{Tracker: workflow.TrackerConfig{APIKey: "reconcile-tracker-secret"}},
		HookEnvPassthrough: []string{"EXTRA_BUILD_VAR", "AIOPS_TRACKER_SECRET"},
		BeforeRemoveHook: workflow.WorkspaceHook{Commands: []string{
			`printf '<%s><%s>' "$EXTRA_BUILD_VAR" "$AIOPS_TRACKER_SECRET" > ` + shellQuote(marker),
		}},
	})
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	body, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read before_remove marker: %v", err)
	}
	if got := string(body); got != "<let-me-in><>" {
		t.Fatalf("before_remove env marker = %q, want tracker secret slot empty", got)
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
		ActiveStates:    []string{"Todo"},
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
		ActiveStates:    []string{"Todo"},
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
	emitter := &fakeEmitter{}
	err := ReconcileStartup(context.Background(), ReconcileConfig{
		WorkspaceRoot:   root,
		ActiveStates:    []string{"In Progress"},
		TerminalStates:  []string{"Done"},
		Tracker:         fake,
		Emitter:         emitter,
		ReconcileTaskID: "reconcile-startup",
	})
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if _, err := os.Stat(workspacePath); err != nil {
		t.Fatalf("conflicting active workspace should be preserved: %v", err)
	}
	wantSingleReconcileWorkspaceReason(t, emitter, "active")
}

func TestReconcileStartupKeepsActiveReworkWhenTerminalSnapshotConflicts(t *testing.T) {
	root := t.TempDir()
	workspacePath := filepath.Join(root, "acme", "repo", "linear-issue", "issue-3_rework_2026-05-16T10_00_00Z")
	if err := os.MkdirAll(workspacePath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	fake := &fakeReconcileTrackerByCall{issuesByCall: [][]tracker.Issue{
		{{ID: "issue-3", Identifier: "LIN-3", State: "Rework", UpdatedAt: mustTime("2026-05-16T11:30:00Z")}},
		{{ID: "issue-3", Identifier: "LIN-3", State: "Done"}},
	}}
	emitter := &fakeEmitter{}
	err := ReconcileStartup(context.Background(), ReconcileConfig{
		WorkspaceRoot:   root,
		ActiveStates:    []string{"Rework"},
		TerminalStates:  []string{"Done"},
		Tracker:         fake,
		Emitter:         emitter,
		ReconcileTaskID: "reconcile-startup",
	})
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if _, err := os.Stat(workspacePath); err != nil {
		t.Fatalf("conflicting active Rework workspace should remain: %v", err)
	}
	wantSingleReconcileWorkspaceReason(t, emitter, "active_rework")
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

// reconcileEndPayloadFor returns the single reconcile_end payload, failing if
// there is not exactly one. Existing tests assert the skipped/partial status
// strings but not the kept/removed counts; the tests below pin those counts so
// the #521 decomposition (which moves the per-workspace counting into
// reconcileWorkspaces and payload assembly into reconcileEndPayload) is
// provably behavior-preserving.
func reconcileEndPayloadFor(t *testing.T, em *fakeEmitter) map[string]any {
	t.Helper()
	ends := em.byKind(task.EventReconcileEnd)
	if len(ends) != 1 {
		t.Fatalf("reconcile_end events = %d, want 1", len(ends))
	}
	payload, ok := ends[0].Payload.(map[string]any)
	if !ok {
		t.Fatalf("reconcile_end payload = %T, want map", ends[0].Payload)
	}
	return payload
}

func wantPayloadCount(t *testing.T, payload map[string]any, key string, want int) {
	t.Helper()
	if got, _ := payload[key].(int); got != want {
		t.Errorf("reconcile_end payload[%q] = %v, want %d", key, payload[key], want)
	}
}

func wantSingleReconcileWorkspaceReason(t *testing.T, emitter *fakeEmitter, want string) {
	t.Helper()
	events := emitter.byKind(task.EventReconcileWorkspace)
	if len(events) != 1 {
		t.Fatalf("reconcile_workspace events = %d, want 1", len(events))
	}
	payload, ok := events[0].Payload.(map[string]any)
	if !ok {
		t.Fatalf("reconcile_workspace payload = %T, want map", events[0].Payload)
	}
	if got, _ := payload["reason"].(string); got != want {
		t.Fatalf("reconcile_workspace reason = %q, want %q", got, want)
	}
}

func wantReconcileWorkspaceReasonForPath(t *testing.T, emitter *fakeEmitter, path, want string) {
	t.Helper()
	for _, event := range emitter.byKind(task.EventReconcileWorkspace) {
		payload, ok := event.Payload.(map[string]any)
		if !ok || payload["path"] != path {
			continue
		}
		if got, _ := payload["reason"].(string); got != want {
			t.Fatalf("reconcile_workspace reason for %s = %q, want %q", path, got, want)
		}
		return
	}
	t.Fatalf("reconcile_workspace event for %s not found", path)
}

func TestReconcileStartupEndPayloadReportsKeptRemovedCounts(t *testing.T) {
	root := t.TempDir()
	for _, key := range []string{"LIN-1", "LIN-2", "LIN-3"} {
		if err := os.MkdirAll(filepath.Join(root, "acme", "repo", "linear_issue", key), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", key, err)
		}
	}
	emitter := &fakeEmitter{}
	err := ReconcileStartup(context.Background(), ReconcileConfig{
		WorkspaceRoot:  root,
		ActiveStates:   []string{"In Progress"},
		TerminalStates: []string{"Done"},
		Tracker: fakeReconcileTracker{issues: []tracker.Issue{
			{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"},
			{ID: "issue-2", Identifier: "LIN-2", State: "Done"},
		}},
		Emitter:         emitter,
		ReconcileTaskID: "reconcile-startup",
	})
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	payload := reconcileEndPayloadFor(t, emitter)
	if status, _ := payload["status"].(string); status != "ok" {
		t.Errorf("reconcile_end status = %q, want \"ok\"", status)
	}
	wantPayloadCount(t, payload, "kept", 2)    // LIN-1 active + LIN-3 unmatched
	wantPayloadCount(t, payload, "removed", 1) // LIN-2 terminal
	wantPayloadCount(t, payload, "active_issues", 1)
	wantPayloadCount(t, payload, "terminal_issues", 1)
}

func TestReconcileStartupEndPayloadCountsOnTerminalFetchFailure(t *testing.T) {
	root := t.TempDir()
	for _, key := range []string{"LIN-1", "LIN-9"} {
		if err := os.MkdirAll(filepath.Join(root, "acme", "repo", "linear_issue", key), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", key, err)
		}
	}
	emitter := &fakeEmitter{}
	err := ReconcileStartup(context.Background(), ReconcileConfig{
		WorkspaceRoot:  root,
		ActiveStates:   []string{"In Progress"},
		TerminalStates: []string{"Done"},
		Tracker: &fakeReconcileTrackerByCall{
			issuesByCall: [][]tracker.Issue{{{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"}}, nil},
			errByCall:    []error{nil, errors.New("terminal outage")},
		},
		Emitter:         emitter,
		ReconcileTaskID: "reconcile-startup",
	})
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	payload := reconcileEndPayloadFor(t, emitter)
	if status, _ := payload["status"].(string); status != "partial" {
		t.Errorf("reconcile_end status = %q, want \"partial\"", status)
	}
	wantPayloadCount(t, payload, "removed", 0)
	wantPayloadCount(t, payload, "kept", 2) // LIN-1 active + LIN-9 unknown kept (terminal unconfirmed)
}

// TestReconcileStartupEndPayloadCountsReworkKeep pins the active_rework branch's
// kept count. The on-disk workspace timestamp differs from the issue's current
// updatedAt, so it matches via reworkWorkspaceKeyPrefixes (not an exact active
// key). The terminal result keeps the test representative of a mixed board.
func TestReconcileStartupEndPayloadCountsReworkKeep(t *testing.T) {
	root := t.TempDir()
	reworkPath := filepath.Join(root, "acme", "repo", "linear-issue", "issue-3-rework-2026-05-16t10-00-00z")
	if err := os.MkdirAll(reworkPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	emitter := &fakeEmitter{}
	err := ReconcileStartup(context.Background(), ReconcileConfig{
		WorkspaceRoot:  root,
		ActiveStates:   []string{"Rework"},
		TerminalStates: []string{"Done"},
		Tracker: fakeReconcileTracker{issues: []tracker.Issue{
			{ID: "issue-3", Identifier: "LIN-3", State: "Rework", UpdatedAt: mustTime("2026-05-16T11:30:00Z")},
			{ID: "issue-2", Identifier: "LIN-2", State: "Done"},
		}},
		Emitter:         emitter,
		ReconcileTaskID: "reconcile-startup",
	})
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if _, err := os.Stat(reworkPath); err != nil {
		t.Fatalf("active_rework workspace should remain: %v", err)
	}
	payload := reconcileEndPayloadFor(t, emitter)
	wantPayloadCount(t, payload, "kept", 1) // matched via active_rework prefix branch
	wantPayloadCount(t, payload, "removed", 0)
	wantSingleReconcileWorkspaceReason(t, emitter, "active_rework")
}

func TestRemoveIssueWorkspaceRejectsUnsafePathWithoutRemoveEvent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	emitter := &fakeEmitter{}
	removed, err := RemoveIssueWorkspace(context.Background(), emitter, RemoveWorkspaceRequest{
		WorkspaceRoot: root,
		TaskID:        "reconcile-startup",
		Path:          outside,
		Reason:        "terminal",
	})
	if !errors.Is(err, workspace.ErrSafeRemoveEscapesRoot) {
		t.Fatalf("RemoveIssueWorkspace error = %v, want ErrSafeRemoveEscapesRoot", err)
	}
	if removed {
		t.Fatal("RemoveIssueWorkspace removed = true, want false")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("unsafe outside path must remain: %v", err)
	}
	if got := len(emitter.byKind(task.EventReconcileWorkspace)); got != 0 {
		t.Fatalf("reconcile_workspace events = %d, want 0 after failed removal", got)
	}
}

// listIssueWorkspaceKeys returns just the sanitized keys discovered by
// listIssueWorkspaces, sorted, so order-independent membership assertions read
// cleanly. Traversal-order assertions use the raw slice instead.
func listIssueWorkspaceKeys(t *testing.T, root, trackerKind string) []string {
	t.Helper()
	workspaces, err := listIssueWorkspaces(root, trackerKind)
	if err != nil {
		t.Fatalf("listIssueWorkspaces(%q) = _, %v; want nil error", root, err)
	}
	keys := make([]string, 0, len(workspaces))
	for _, ws := range workspaces {
		keys = append(keys, ws.Key)
	}
	sort.Strings(keys)
	return keys
}

// TestListIssueWorkspacesMissingRootReturnsNil pins the root os.IsNotExist
// branch: a non-existent workspace root yields (nil, nil), never an error.
func TestListIssueWorkspacesMissingRootReturnsNil(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	workspaces, err := listIssueWorkspaces(missing, "linear")
	if err != nil {
		t.Fatalf("listIssueWorkspaces(%q) = _, %v; want nil error", missing, err)
	}
	if workspaces != nil {
		t.Errorf("listIssueWorkspaces(%q) = %v; want nil", missing, workspaces)
	}
}

// TestListIssueWorkspacesUnknownTrackerYieldsNoWorkspaces pins the nil
// sourceDirs branch: an unknown tracker kind ranges over nil source dirs, so a
// real workspace directory laid out on disk is never discovered.
func TestListIssueWorkspacesUnknownTrackerYieldsNoWorkspaces(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "acme", "repo", "linear_issue", "LIN-1"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	workspaces, err := listIssueWorkspaces(root, "bitbucket")
	if err != nil {
		t.Fatalf("listIssueWorkspaces(%q) = _, %v; want nil error", root, err)
	}
	if len(workspaces) != 0 {
		t.Errorf("listIssueWorkspaces(%q, unknown tracker) = %v; want no workspaces", root, workspaces)
	}
}

// TestListIssueWorkspacesExcludesNonDirectoryEntries pins the three !IsDir
// guards (owner, repo, workspace level). Regular files placed beside real
// workspace dirs at every level must be excluded from the result; only the
// genuine workspace directory is reported.
func TestListIssueWorkspacesExcludesNonDirectoryEntries(t *testing.T) {
	root := t.TempDir()
	workspaceDir := filepath.Join(root, "acme", "repo", "linear_issue", "LIN-1")
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A file at the owner level (beside the "acme" owner dir).
	mustWriteFile(t, filepath.Join(root, "owner-file.txt"))
	// A file at the repo level (beside the "repo" repo dir).
	mustWriteFile(t, filepath.Join(root, "acme", "repo-file.txt"))
	// A file at the workspace level (beside the "LIN-1" workspace dir).
	mustWriteFile(t, filepath.Join(root, "acme", "repo", "linear_issue", "workspace-file.txt"))

	got := listIssueWorkspaceKeys(t, root, "linear")
	want := []string{"LIN-1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("listIssueWorkspaces(%q) keys = %v; want %v (non-directory entries must be excluded)", root, got, want)
	}
}

// TestListIssueWorkspacesUnreadableOwnerDirReturnsWrappedError pins the owner
// read-error branch: an unreadable owner directory is fatal (no IsNotExist
// special-case at this level) and surfaces the "read workspace owner %s: %w"
// wrap with the owner path. Skipped under root, which bypasses 0o000.
func TestListIssueWorkspacesUnreadableOwnerDirReturnsWrappedError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses 0o000 permissions")
	}
	root := t.TempDir()
	ownerPath := filepath.Join(root, "acme")
	if err := os.MkdirAll(ownerPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(ownerPath, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(ownerPath, 0o755) })

	_, err := listIssueWorkspaces(root, "linear")
	if err == nil {
		t.Fatalf("listIssueWorkspaces(%q) = _, nil; want owner read error", root)
	}
	wantPrefix := "read workspace owner " + ownerPath + ":"
	if !strings.HasPrefix(err.Error(), wantPrefix) {
		t.Errorf("listIssueWorkspaces(%q) error = %q; want prefix %q", root, err.Error(), wantPrefix)
	}
}

// TestListIssueWorkspacesUnreadableSourceDirReturnsWrappedError pins the
// source-dir read-error branch: an unreadable issue-workspace source directory
// is fatal (distinct from the IsNotExist skip) and surfaces the "read issue
// workspace source %s: %w" wrap with the source path. Skipped under root.
func TestListIssueWorkspacesUnreadableSourceDirReturnsWrappedError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses 0o000 permissions")
	}
	root := t.TempDir()
	sourcePath := filepath.Join(root, "acme", "repo", "linear_issue")
	if err := os.MkdirAll(sourcePath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(sourcePath, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(sourcePath, 0o755) })

	_, err := listIssueWorkspaces(root, "linear")
	if err == nil {
		t.Fatalf("listIssueWorkspaces(%q) = _, nil; want source read error", root)
	}
	wantPrefix := "read issue workspace source " + sourcePath + ":"
	if !strings.HasPrefix(err.Error(), wantPrefix) {
		t.Errorf("listIssueWorkspaces(%q) error = %q; want prefix %q", root, err.Error(), wantPrefix)
	}
}

// TestListIssueWorkspacesMissingSourceDirIsSkipped pins the source-dir
// os.IsNotExist branch: when one of the candidate source dirs does not exist
// the traversal continues to the next candidate rather than erroring, so the
// sibling source dir's workspaces are still discovered.
func TestListIssueWorkspacesMissingSourceDirIsSkipped(t *testing.T) {
	root := t.TempDir()
	// Only the hyphen variant "linear-issue" exists; the underscore variant
	// "linear_issue" is absent and must be skipped, not error.
	if err := os.MkdirAll(filepath.Join(root, "acme", "repo", "linear-issue", "LIN-1"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	got := listIssueWorkspaceKeys(t, root, "linear")
	want := []string{"LIN-1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("listIssueWorkspaces(%q) keys = %v; want %v (missing source dir must be skipped)", root, got, want)
	}
}

// TestListIssueWorkspacesPreservesTraversalOrder pins the owner->repo->
// sourceDir->workspaceEntry concatenation order that feeds reconcile_workspace
// event emission ordering. Entries within a directory come back from os.ReadDir
// sorted by name, so the expected order is deterministic.
func TestListIssueWorkspacesPreservesTraversalOrder(t *testing.T) {
	root := t.TempDir()
	// Two owners (alpha, beta), each with one repo. Within alpha/repo both
	// source dirs exist; the inner loop ranges issueWorkspaceSourceDirs in its
	// declared order ("linear_issue" then "linear-issue"), NOT ReadDir's
	// name-sorted order, so A2 (under linear_issue) must precede A1 (under
	// linear-issue) even though "linear-issue" sorts first on disk. This pins
	// the sourceDir-ordering branch alongside the owner-major sequence.
	dirs := []string{
		filepath.Join(root, "alpha", "repo", "linear-issue", "A1"),
		filepath.Join(root, "alpha", "repo", "linear_issue", "A2"),
		filepath.Join(root, "beta", "repo", "linear-issue", "B1"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	workspaces, err := listIssueWorkspaces(root, "linear")
	if err != nil {
		t.Fatalf("listIssueWorkspaces(%q) = _, %v; want nil error", root, err)
	}
	gotPaths := make([]string, 0, len(workspaces))
	for _, ws := range workspaces {
		gotPaths = append(gotPaths, ws.Path)
	}
	wantPaths := []string{
		filepath.Join(root, "alpha", "repo", "linear_issue", "A2"),
		filepath.Join(root, "alpha", "repo", "linear-issue", "A1"),
		filepath.Join(root, "beta", "repo", "linear-issue", "B1"),
	}
	if !reflect.DeepEqual(gotPaths, wantPaths) {
		t.Errorf("listIssueWorkspaces(%q) paths = %v; want %v (traversal order must be owner->repo->sourceDir->entry)", root, gotPaths, wantPaths)
	}
}

func mustWriteFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file %s: %v", path, err)
	}
}

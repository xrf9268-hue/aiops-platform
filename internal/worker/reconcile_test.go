package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
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

func TestReconcileStartupRemovesUnknownWorkspaces(t *testing.T) {
	root := t.TempDir()
	unknownPath := filepath.Join(root, "acme", "repo", "linear_issue", "LIN-404")
	if err := os.MkdirAll(unknownPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	err := ReconcileStartup(context.Background(), ReconcileConfig{
		WorkspaceRoot:   root,
		ActiveStates:    []string{"AI Ready"},
		TerminalStates:  []string{"Done"},
		Tracker:         fakeReconcileTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "AI Ready"}}},
		Emitter:         &fakeEmitter{},
		ReconcileTaskID: "reconcile-startup",
	})
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if _, err := os.Stat(unknownPath); !os.IsNotExist(err) {
		t.Fatalf("unknown workspace should be removed, stat err=%v", err)
	}
}

func TestReconcileStartupRemovesUnknownWorkspacesWhenNoIssuesReturned(t *testing.T) {
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
	if err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if _, err := os.Stat(unknownPath); !os.IsNotExist(err) {
		t.Fatalf("unknown workspace should be removed even when tracker returns no active issues, stat err=%v", err)
	}
}

func TestReconcileStartupMatchesCurrentSanitizedWorkspaceLayout(t *testing.T) {
	root := t.TempDir()
	activePath := filepath.Join(root, "acme", "repo", "linear-issue", "lin-1-needs-fix")
	reworkPath := filepath.Join(root, "acme", "repo", "linear-issue", "issue-3-rework-2026-05-16t10-00-00z")
	terminalPath := filepath.Join(root, "acme", "repo", "linear-issue", "lin-2-done")
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
			{ID: "issue-3", Identifier: "LIN-3", State: "Rework", UpdatedAt: "2026-05-16T10:00:00Z"},
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

func TestReconcileStartupReturnsTrackerError(t *testing.T) {
	root := t.TempDir()
	if err := ReconcileStartup(context.Background(), ReconcileConfig{
		WorkspaceRoot:   root,
		ActiveStates:    []string{"AI Ready"},
		TerminalStates:  []string{"Done"},
		Tracker:         fakeReconcileTracker{err: errors.New("tracker down")},
		Emitter:         &fakeEmitter{},
		ReconcileTaskID: "reconcile-startup",
	}); err == nil {
		t.Fatal("ReconcileStartup error = nil, want tracker error")
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
	workspacePath := filepath.Join(root, "acme", "repo", "linear-issue", "same-key")
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

func TestReconcileStartupReturnsTerminalFetchError(t *testing.T) {
	root := t.TempDir()
	fake := &fakeReconcileTrackerByCall{errByCall: []error{nil, errors.New("terminal fetch failed")}}
	err := ReconcileStartup(context.Background(), ReconcileConfig{
		WorkspaceRoot:   root,
		ActiveStates:    []string{"AI Ready"},
		TerminalStates:  []string{"Done"},
		Tracker:         fake,
		Emitter:         &fakeEmitter{},
		ReconcileTaskID: "reconcile-startup",
	})
	if err == nil || !strings.Contains(err.Error(), "fetch terminal issues") {
		t.Fatalf("ReconcileStartup error = %v, want terminal fetch context", err)
	}
}

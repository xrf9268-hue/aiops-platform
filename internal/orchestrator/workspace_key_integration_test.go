package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/worker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
	"github.com/xrf9268-hue/aiops-platform/internal/workspace"
)

type workspaceKeyReconcileTracker struct {
	calls    int
	terminal tracker.Issue
}

func (f *workspaceKeyReconcileTracker) ListIssuesByStates(context.Context, []string) ([]tracker.Issue, error) {
	f.calls++
	if f.calls == 1 {
		return nil, nil
	}
	return []tracker.Issue{f.terminal}, nil
}

func TestIssueWorkspaceKeyWiresDispatchPathToTerminalCleanup(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg := workflow.Config{
		Tracker: workflow.TrackerConfig{Kind: "linear"},
		Repo: workflow.RepoConfig{
			Owner:         "acme",
			Name:          "demo",
			CloneURL:      "git@example.com:acme/demo.git",
			DefaultBranch: "main",
		},
	}
	terminalIssue := tracker.Issue{ID: "slash", Identifier: "team/a-1", Title: "terminal", State: "Done"}
	collidingIssue := tracker.Issue{ID: "underscore", Identifier: "team_a-1", Title: "active elsewhere", State: "Todo"}

	terminalTask, err := TaskFromIssue(terminalIssue, cfg)
	if err != nil {
		t.Fatalf("TaskFromIssue(terminal) error = %v", err)
	}
	collidingTask, err := TaskFromIssue(collidingIssue, cfg)
	if err != nil {
		t.Fatalf("TaskFromIssue(colliding) error = %v", err)
	}
	manager := workspace.New(root)
	terminalPath := manager.PathFor(terminalTask)
	collidingPath := manager.PathFor(collidingTask)
	if terminalPath == collidingPath {
		t.Fatalf("Manager.PathFor collision = %q for identifiers %q and %q", terminalPath, terminalIssue.Identifier, collidingIssue.Identifier)
	}
	for _, path := range []string{terminalPath, collidingPath} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v", path, err)
		}
	}

	err = worker.ReconcileStartup(context.Background(), worker.ReconcileConfig{
		WorkspaceRoot:   root,
		ActiveStates:    []string{"Todo"},
		TerminalStates:  []string{"Done"},
		TrackerKind:     cfg.Tracker.Kind,
		Tracker:         &workspaceKeyReconcileTracker{terminal: terminalIssue},
		ReconcileTaskID: "workspace-key-wiring",
	})
	if err != nil {
		t.Fatalf("ReconcileStartup error = %v", err)
	}
	if _, err := os.Stat(terminalPath); !os.IsNotExist(err) {
		t.Fatalf("terminal workspace %q still exists after reconciliation: stat error = %v", terminalPath, err)
	}
	if _, err := os.Stat(collidingPath); err != nil {
		t.Fatalf("unmatched colliding workspace %q was not preserved: %v", collidingPath, err)
	}
	if got := filepath.Base(terminalPath); got == filepath.Base(collidingPath) {
		t.Fatalf("workspace key collision remains at %q", got)
	}
}

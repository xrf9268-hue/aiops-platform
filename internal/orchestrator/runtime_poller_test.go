package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/worker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

type stubRefresher struct {
	calls   [][]string
	states  map[string]string
	fetchEr error
}

func (s *stubRefresher) FetchIssueStatesByIDs(_ context.Context, ids []string) (map[string]string, error) {
	s.calls = append(s.calls, append([]string(nil), ids...))
	if s.fetchEr != nil {
		return nil, s.fetchEr
	}
	out := map[string]string{}
	for _, id := range ids {
		if state, ok := s.states[id]; ok {
			out[id] = state
		}
	}
	return out, nil
}

type stubRefAwareRefresher struct {
	refs   [][]tracker.IssueRef
	states map[string]string
}

func (s *stubRefAwareRefresher) FetchIssueStatesByIDs(context.Context, []string) (map[string]string, error) {
	return nil, errors.New("legacy ID-only refresh should not be used")
}

func (s *stubRefAwareRefresher) FetchIssueStatesByRefs(_ context.Context, refs []tracker.IssueRef) (map[string]string, error) {
	s.refs = append(s.refs, append([]tracker.IssueRef(nil), refs...))
	out := map[string]string{}
	for _, ref := range refs {
		if state, ok := s.states[ref.ID]; ok {
			out[ref.ID] = state
		}
	}
	return out, nil
}

// TestRuntimeDispatcherConfigForSnapshotBuildsRefresherClosure pins the
// SPEC §16.5 wiring: when SetIssueStateRefresher is set, the worker.Config
// returned for a snapshot carries a factory that the worker uses to build
// the runner's per-turn refresher. The factory must consult the tracker
// with the task's stable ID and report (in)active states from the
// workflow's active_states vocabulary.
func TestRuntimeDispatcherConfigForSnapshotBuildsRefresherClosure(t *testing.T) {
	d := &RuntimeDispatcher{baseConfig: worker.Config{}}
	stub := &stubRefresher{states: map[string]string{
		"issue-active":   "In Progress",
		"issue-canceled": "Canceled",
	}}
	d.SetIssueStateRefresher(stub)

	snap := WorkflowSnapshot{Workflow: &workflow.Workflow{Config: workflow.Config{
		Tracker: workflow.TrackerConfig{ActiveStates: []string{"In Progress", "AI Ready"}},
	}}}
	cfg := d.configForSnapshot(snap)
	if cfg.IssueStateRefresher == nil {
		t.Fatal("configForSnapshot did not wire IssueStateRefresher factory")
	}

	wcfg := snap.Workflow.Config

	t.Run("active state keeps run alive", func(t *testing.T) {
		fn := cfg.IssueStateRefresher(task.Task{ID: "issue-active"}, wcfg)
		if fn == nil {
			t.Fatal("factory returned nil for valid task")
		}
		active, err := fn(context.Background())
		if err != nil {
			t.Fatalf("refresher err: %v", err)
		}
		if !active {
			t.Fatalf("active = false, want true (state In Progress is in active set)")
		}
		if got := stub.calls; len(got) != 1 || len(got[0]) != 1 || got[0][0] != "issue-active" {
			t.Fatalf("tracker calls = %#v, want exactly [[issue-active]]", got)
		}
	})

	t.Run("inactive state stops the run", func(t *testing.T) {
		fn := cfg.IssueStateRefresher(task.Task{ID: "issue-canceled"}, wcfg)
		active, err := fn(context.Background())
		if err != nil {
			t.Fatalf("refresher err: %v", err)
		}
		if active {
			t.Fatal("active = true, want false (state Canceled is not in active set)")
		}
	})

	t.Run("missing row keeps run alive per SPEC fallback", func(t *testing.T) {
		fn := cfg.IssueStateRefresher(task.Task{ID: "issue-unknown"}, wcfg)
		active, err := fn(context.Background())
		if err != nil {
			t.Fatalf("refresher err: %v", err)
		}
		if !active {
			t.Fatal("active = false, want true; SPEC §16.5 keeps prior state when refresh returns no row")
		}
	})

	t.Run("fetch error surfaces", func(t *testing.T) {
		boom := errors.New("tracker boom")
		errStub := &stubRefresher{fetchEr: boom}
		d.SetIssueStateRefresher(errStub)
		errCfg := d.configForSnapshot(snap)
		fn := errCfg.IssueStateRefresher(task.Task{ID: "issue-active"}, wcfg)
		active, err := fn(context.Background())
		if active {
			t.Fatal("active = true on fetch error, want false")
		}
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want wrapped tracker boom", err)
		}
	})
}

func TestRuntimeDispatcherIssueStateRefresherPassesIssueIdentifierFallback(t *testing.T) {
	d := &RuntimeDispatcher{baseConfig: worker.Config{}}
	stub := &stubRefAwareRefresher{states: map[string]string{"global-101": "Done"}}
	d.SetIssueStateRefresher(stub)

	snap := WorkflowSnapshot{Workflow: &workflow.Workflow{Config: workflow.Config{
		Tracker: workflow.TrackerConfig{ActiveStates: []string{"In Progress"}},
	}}}
	cfg := d.configForSnapshot(snap)
	fn := cfg.IssueStateRefresher(task.Task{
		ID:            "global-101",
		SourceEventID: "global-101|service|api",
		IssueRender:   map[string]any{"identifier": "#7"},
	}, snap.Workflow.Config)

	active, err := fn(context.Background())
	if err != nil {
		t.Fatalf("refresher err: %v", err)
	}
	if active {
		t.Fatal("active = true, want false after ref-aware Done refresh")
	}
	if len(stub.refs) != 1 || len(stub.refs[0]) != 1 {
		t.Fatalf("ref calls = %#v, want one issue ref", stub.refs)
	}
	if got := stub.refs[0][0]; got.ID != "global-101" || got.Identifier != "#7" {
		t.Fatalf("issue ref = %+v, want ID global-101 with identifier #7", got)
	}
}

// TestRuntimeDispatcherConfigForSnapshotReturnsNilWithoutRefresher pins the
// fallback: without SetIssueStateRefresher the dispatcher leaves
// IssueStateRefresher unset so RunTask keeps the legacy continueRun-only
// path (e.g. tests, mock runners).
func TestRuntimeDispatcherConfigForSnapshotReturnsNilWithoutRefresher(t *testing.T) {
	d := &RuntimeDispatcher{baseConfig: worker.Config{}}
	snap := WorkflowSnapshot{Workflow: &workflow.Workflow{Config: workflow.Config{
		Tracker: workflow.TrackerConfig{ActiveStates: []string{"In Progress"}},
	}}}
	cfg := d.configForSnapshot(snap)
	if cfg.IssueStateRefresher != nil {
		t.Fatal("IssueStateRefresher should be nil when dispatcher has no tracker refresher set")
	}
}

// TestRuntimePollerAttachDispatcherCarriesCurrentRefresher pins the
// cmd/worker/main.go startup sequence: NewRuntimePollerWithTrackerFactory
// builds the initial tracker fan-in (and SetIssueStateRefresher's it
// onto its internal dispatcher) before the caller has a chance to
// AttachDispatcher with the external dispatcher used by the actor. The
// attach call must replay the current refresher so the first PollOnce
// — which sees an unchanged snapshot key and short-circuits the
// SetIssueStateRefresher path — does not leave the actor's dispatcher
// without a refresher.
func TestRuntimePollerAttachDispatcherCarriesCurrentRefresher(t *testing.T) {
	stub := &stubRefresher{states: map[string]string{"issue-1": "In Progress"}}
	internal := &RuntimeDispatcher{}
	rp := &RuntimePoller{dispatcher: internal}
	// Simulate pollerForSnapshot having stored the multiLister.
	rp.mu.Lock()
	rp.currentRefresher = stub
	rp.mu.Unlock()
	internal.SetIssueStateRefresher(stub)

	external := &RuntimeDispatcher{}
	rp.AttachDispatcher(external)
	if external.currentRefresher() == nil {
		t.Fatal("AttachDispatcher did not carry the current refresher onto the external dispatcher")
	}

	snap := WorkflowSnapshot{Workflow: &workflow.Workflow{Config: workflow.Config{
		Tracker: workflow.TrackerConfig{ActiveStates: []string{"In Progress"}},
	}}}
	cfg := external.configForSnapshot(snap)
	if cfg.IssueStateRefresher == nil {
		t.Fatal("external dispatcher has no IssueStateRefresher factory after AttachDispatcher")
	}
	fn := cfg.IssueStateRefresher(task.Task{ID: "issue-1"}, snap.Workflow.Config)
	if fn == nil {
		t.Fatal("factory returned nil for valid task on external dispatcher")
	}
	active, err := fn(context.Background())
	if err != nil {
		t.Fatalf("refresher err: %v", err)
	}
	if !active {
		t.Fatal("active = false, want true; external dispatcher should call through to the stub refresher")
	}
	if len(stub.calls) == 0 {
		t.Fatal("stub refresher never invoked; external dispatcher is wired to a different tracker")
	}
}

// TestRuntimeDispatcherConfigForSnapshotEmptyActiveStatesDisablesFactory
// guards a foot-gun: if active_states is empty the refresher would mark
// every state as inactive and kill workers after the first turn. The
// factory must return nil so the runner falls back to continueRun.
func TestRuntimeDispatcherConfigForSnapshotEmptyActiveStatesDisablesFactory(t *testing.T) {
	d := &RuntimeDispatcher{baseConfig: worker.Config{}}
	d.SetIssueStateRefresher(&stubRefresher{states: map[string]string{"issue-1": "In Progress"}})
	snap := WorkflowSnapshot{Workflow: &workflow.Workflow{Config: workflow.Config{}}}
	cfg := d.configForSnapshot(snap)
	if cfg.IssueStateRefresher == nil {
		t.Fatal("factory should be set on dispatcher; we test its per-task output instead")
	}
	fn := cfg.IssueStateRefresher(task.Task{ID: "issue-1"}, snap.Workflow.Config)
	if fn != nil {
		t.Fatal("per-task refresher should be nil when active_states is empty (avoids the 'kills everyone' foot-gun)")
	}
}

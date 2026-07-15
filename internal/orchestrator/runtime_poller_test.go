package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/worker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

type stubRefresher struct {
	calls       [][]string
	states      map[string]string
	labels      map[string][]string
	issueStates map[string]tracker.IssueState
	fetchEr     error
}

func (s *stubRefresher) FetchIssueStatesByIDs(_ context.Context, ids []string) (map[string]tracker.IssueState, error) {
	s.calls = append(s.calls, append([]string(nil), ids...))
	if s.fetchEr != nil {
		return nil, s.fetchEr
	}
	out := map[string]tracker.IssueState{}
	for _, id := range ids {
		if state, ok := s.issueStates[id]; ok {
			out[id] = state
			continue
		}
		if state, ok := s.states[id]; ok {
			out[id] = tracker.IssueState{Outcome: tracker.IssueStateOutcomeCurrent, State: state, Labels: s.labels[id]}
		}
	}
	return out, nil
}

type stubRefAwareRefresher struct {
	refs   [][]tracker.IssueRef
	states map[string]string
}

func (s *stubRefAwareRefresher) FetchIssueStatesByIDs(context.Context, []string) (map[string]tracker.IssueState, error) {
	return nil, errors.New("legacy ID-only refresh should not be used")
}

func (s *stubRefAwareRefresher) FetchIssueStatesByRefs(_ context.Context, refs []tracker.IssueRef) (map[string]tracker.IssueState, error) {
	s.refs = append(s.refs, append([]tracker.IssueRef(nil), refs...))
	out := map[string]tracker.IssueState{}
	for _, ref := range refs {
		if state, ok := s.states[ref.ID]; ok {
			out[ref.ID] = tracker.IssueState{Outcome: tracker.IssueStateOutcomeCurrent, State: state}
		}
	}
	return out, nil
}

type stubNoBlockerRefAwareRefresher struct {
	blockerRefs   [][]tracker.IssueRef
	noBlockerRefs [][]tracker.IssueRef
	states        map[string]string
}

func (s *stubNoBlockerRefAwareRefresher) FetchIssueStatesByIDs(context.Context, []string) (map[string]tracker.IssueState, error) {
	return nil, errors.New("legacy ID-only refresh should not be used")
}

func (s *stubNoBlockerRefAwareRefresher) FetchIssueStatesByRefs(_ context.Context, refs []tracker.IssueRef) (map[string]tracker.IssueState, error) {
	s.blockerRefs = append(s.blockerRefs, append([]tracker.IssueRef(nil), refs...))
	return nil, errors.New("blocker-aware refresh should not be used for runner current-issue state")
}

func (s *stubNoBlockerRefAwareRefresher) FetchIssueStatesWithoutBlockersByRefs(_ context.Context, refs []tracker.IssueRef) (map[string]tracker.IssueState, error) {
	s.noBlockerRefs = append(s.noBlockerRefs, append([]tracker.IssueRef(nil), refs...))
	out := map[string]tracker.IssueState{}
	for _, ref := range refs {
		if state, ok := s.states[ref.ID]; ok {
			out[ref.ID] = tracker.IssueState{Outcome: tracker.IssueStateOutcomeCurrent, State: state}
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
		Tracker: workflow.TrackerConfig{ActiveStates: []string{"In Progress", "Todo"}},
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
		snapshot, err := fn(context.Background())
		if err != nil {
			t.Fatalf("refresher err: %v", err)
		}
		if !snapshot.Active || !snapshot.Found || snapshot.State != "In Progress" {
			t.Fatalf("snapshot = %+v, want found active In Progress", snapshot)
		}
		if got := stub.calls; len(got) != 1 || len(got[0]) != 1 || got[0][0] != "issue-active" {
			t.Fatalf("tracker calls = %#v, want exactly [[issue-active]]", got)
		}
	})

	t.Run("inactive state stops the run", func(t *testing.T) {
		fn := cfg.IssueStateRefresher(task.Task{ID: "issue-canceled"}, wcfg)
		snapshot, err := fn(context.Background())
		if err != nil {
			t.Fatalf("refresher err: %v", err)
		}
		if snapshot.Active || !snapshot.Found || snapshot.State != "Canceled" {
			t.Fatalf("snapshot = %+v, want found inactive Canceled", snapshot)
		}
	})

	t.Run("missing row keeps run alive per SPEC fallback", func(t *testing.T) {
		fn := cfg.IssueStateRefresher(task.Task{ID: "issue-unknown"}, wcfg)
		snapshot, err := fn(context.Background())
		if err != nil {
			t.Fatalf("refresher err: %v", err)
		}
		if !snapshot.Active || snapshot.Found {
			t.Fatalf("snapshot = %+v, want missing active fallback; SPEC §16.5 keeps prior state when refresh returns no row", snapshot)
		}
	})

	t.Run("fetch error surfaces", func(t *testing.T) {
		boom := errors.New("tracker boom")
		errStub := &stubRefresher{fetchEr: boom}
		d.SetIssueStateRefresher(errStub)
		errCfg := d.configForSnapshot(snap)
		fn := errCfg.IssueStateRefresher(task.Task{ID: "issue-active"}, wcfg)
		snapshot, err := fn(context.Background())
		if snapshot.Active {
			t.Fatalf("snapshot = %+v, want inactive zero snapshot on fetch error", snapshot)
		}
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want wrapped tracker boom", err)
		}
	})
}

func TestRuntimeDispatcherCurrentIssueRefreshRequiresCurrentOutcome(t *testing.T) {
	d := &RuntimeDispatcher{baseConfig: worker.Config{}}
	snap := WorkflowSnapshot{Workflow: &workflow.Workflow{Config: workflow.Config{
		Tracker: workflow.TrackerConfig{ActiveStates: []string{"In Progress"}, TerminalStates: []string{"Done"}},
	}}}
	tests := []struct {
		name   string
		states map[string]tracker.IssueState
	}{
		{
			name: "state-bearing unknown",
			states: map[string]tracker.IssueState{
				"issue-1": {Outcome: tracker.IssueStateOutcomeUnknown, State: "In Progress"},
			},
		},
		{
			name: "state-bearing absent",
			states: map[string]tracker.IssueState{
				"issue-1": {Outcome: tracker.IssueStateOutcomeAbsent, State: "Done"},
			},
		},
		{name: "missing row", states: map[string]tracker.IssueState{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d.SetIssueStateRefresher(&stubRefresher{issueStates: tt.states})
			cfg := d.configForSnapshot(snap)
			fn := cfg.IssueStateRefresher(task.Task{ID: "issue-1"}, snap.Workflow.Config)
			got, err := fn(context.Background())
			if err != nil {
				t.Fatalf("current issue refresh error = %v; want nil", err)
			}
			if got.Found || !got.Active || got.Terminal || got.State != "" {
				t.Fatalf("current issue snapshot = %+v; want Found=false Active=true fallback", got)
			}
		})
	}
}

func TestRuntimeDispatcherIssueStateRefresherUsesNoBlockerRefreshWhenAvailable(t *testing.T) {
	d := &RuntimeDispatcher{baseConfig: worker.Config{}}
	stub := &stubNoBlockerRefAwareRefresher{states: map[string]string{"global-101": "In Progress"}}
	d.SetIssueStateRefresher(stub)

	snap := WorkflowSnapshot{Workflow: &workflow.Workflow{Config: workflow.Config{
		Tracker: workflow.TrackerConfig{ActiveStates: []string{"In Progress"}},
	}}}
	cfg := d.configForSnapshot(snap)
	fn := cfg.IssueStateRefresher(task.Task{
		ID:          "global-101",
		IssueRender: map[string]any{"identifier": "#7"},
	}, snap.Workflow.Config)

	snapshot, err := fn(context.Background())
	if err != nil {
		t.Fatalf("refresher err = %v; want nil from no-blocker current-issue refresh", err)
	}
	if !snapshot.Active || !snapshot.Found || snapshot.State != "In Progress" {
		t.Fatalf("snapshot = %+v, want found active In Progress", snapshot)
	}
	if len(stub.blockerRefs) != 0 {
		t.Fatalf("blocker-aware refresh calls = %#v; want none for runner current-issue state", stub.blockerRefs)
	}
	if len(stub.noBlockerRefs) != 1 || len(stub.noBlockerRefs[0]) != 1 {
		t.Fatalf("no-blocker refresh calls = %#v; want one issue ref", stub.noBlockerRefs)
	}
	if got := stub.noBlockerRefs[0][0]; got.ID != "global-101" || got.Identifier != "#7" {
		t.Fatalf("no-blocker refresh ref = %+v; want ID global-101 identifier #7", got)
	}
}

// TestRuntimeDispatcherContinueGateAppliesRequiredLabels pins the SPEC §6.4
// "continue" gate (P2-a): the per-turn refresher closure must mark an issue
// that is still in an active state but missing a required label as NOT routable
// (Active=false), so the runner self-stops instead of starting another
// continuation turn. A present row with the label stays Active; a missing/absent
// row stays Active regardless of labels (no-information); an empty required_labels
// disables the gate. Mutation: dropping the labelsSatisfyRequired clause from the
// routable computation (or RequiredLabels from the closure) fails the first case.
func TestRuntimeDispatcherContinueGateAppliesRequiredLabels(t *testing.T) {
	d := &RuntimeDispatcher{baseConfig: worker.Config{}}
	stub := &stubRefresher{
		states: map[string]string{"issue-1": "In Progress"},
		labels: map[string][]string{"issue-1": {"backend"}},
	}
	d.SetIssueStateRefresher(stub)
	snap := WorkflowSnapshot{Workflow: &workflow.Workflow{Config: workflow.Config{
		Tracker: workflow.TrackerConfig{ActiveStates: []string{"In Progress"}, RequiredLabels: []string{"aiops-ready"}},
	}}}
	cfg := d.configForSnapshot(snap)
	wcfg := snap.Workflow.Config

	t.Run("active state missing required label is not routable", func(t *testing.T) {
		fn := cfg.IssueStateRefresher(task.Task{ID: "issue-1"}, wcfg)
		snapshot, err := fn(context.Background())
		if err != nil {
			t.Fatalf("refresher err = %v, want nil", err)
		}
		if snapshot.Active {
			t.Fatalf("snapshot.Active = true for active issue missing required label %v; want false (continue gate must self-stop), snapshot=%+v", wcfg.Tracker.RequiredLabels, snapshot)
		}
		if !snapshot.Found || snapshot.State != "In Progress" {
			t.Fatalf("snapshot = %+v, want found In Progress (raw state preserved)", snapshot)
		}
	})

	t.Run("active state retaining required label stays routable", func(t *testing.T) {
		labeled := &stubRefresher{
			states: map[string]string{"issue-1": "In Progress"},
			labels: map[string][]string{"issue-1": {"aiops-ready", "backend"}},
		}
		d.SetIssueStateRefresher(labeled)
		fn := d.configForSnapshot(snap).IssueStateRefresher(task.Task{ID: "issue-1"}, wcfg)
		snapshot, err := fn(context.Background())
		if err != nil {
			t.Fatalf("refresher err = %v, want nil", err)
		}
		if !snapshot.Active {
			t.Fatalf("snapshot.Active = false for active issue with required label; want true, snapshot=%+v", snapshot)
		}
	})

	t.Run("empty required_labels disables the gate", func(t *testing.T) {
		offSnap := WorkflowSnapshot{Workflow: &workflow.Workflow{Config: workflow.Config{
			Tracker: workflow.TrackerConfig{ActiveStates: []string{"In Progress"}},
		}}}
		d.SetIssueStateRefresher(stub) // labels lack aiops-ready, but no required_labels
		fn := d.configForSnapshot(offSnap).IssueStateRefresher(task.Task{ID: "issue-1"}, offSnap.Workflow.Config)
		snapshot, err := fn(context.Background())
		if err != nil {
			t.Fatalf("refresher err = %v, want nil", err)
		}
		if !snapshot.Active {
			t.Fatalf("snapshot.Active = false with empty required_labels; want true (gate off), snapshot=%+v", snapshot)
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

	snapshot, err := fn(context.Background())
	if err != nil {
		t.Fatalf("refresher err: %v", err)
	}
	if snapshot.Active || !snapshot.Found || snapshot.State != "Done" {
		t.Fatalf("snapshot = %+v, want found inactive Done after ref-aware refresh", snapshot)
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

func TestRuntimeDispatcherCleanupRejectsTrackerAPIKeyValuePassthrough(t *testing.T) {
	root := t.TempDir()
	workdir := filepath.Join(root, "acme", "repo", "linear_issue", "LIN-1")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	t.Setenv("EXTRA_BUILD_VAR", "let-me-in")
	t.Setenv("AIOPS_TRACKER_SECRET", "active-tracker-secret")
	marker := filepath.Join(root, "hook-env")
	cfg := workflow.DefaultConfig()
	cfg.Tracker.APIKey = "active-tracker-secret"
	cfg.Hooks = workflow.WorkspaceHooks{
		EnvPassthrough: []string{"EXTRA_BUILD_VAR", "AIOPS_TRACKER_SECRET"},
		BeforeRemove: workflow.WorkspaceHook{Commands: []string{
			`printf '<%s><%s>' "$EXTRA_BUILD_VAR" "$AIOPS_TRACKER_SECRET" > ` + shellQuoteRuntimeTest(marker),
		}},
	}
	runtime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{
		Initial: &workflow.Workflow{Config: cfg, Source: workflow.SourceDefault},
		Source:  workflow.SourceDefault,
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	dispatcher, err := NewRuntimeDispatcher(runtime, worker.Config{}, &recordingWorkerEmitter{})
	if err != nil {
		t.Fatalf("new runtime dispatcher: %v", err)
	}

	dispatcher.CleanupReconciledWorkspace(context.Background(), ReconciledWorkspace{
		IssueID:    IssueID("issue-1"),
		Identifier: "LIN-1",
		Path:       workdir,
		Root:       root,
		State:      "Done",
		Reason:     "terminal",
	})

	body, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read before_remove marker: %v", err)
	}
	if got := string(body); got != "<let-me-in><>" {
		t.Fatalf("before_remove env marker = %q, want tracker secret slot empty", got)
	}
	if _, err := os.Stat(workdir); !os.IsNotExist(err) {
		t.Fatalf("cleanup workdir stat err = %v, want removed", err)
	}
}

func shellQuoteRuntimeTest(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// TestRuntimePollerWiresRefresherOntoProvidedDispatcher pins #791: the
// dispatcher the actor spawns workers through is passed into
// NewRuntimePollerWithTrackerFactory, and the poller points the SPEC §16.5
// per-turn refresh hook at *that* dispatcher (no separate internal instance,
// no post-construction AttachDispatcher). Construction alone runs the initial
// pollerForSnapshot, which must leave the provided dispatcher carrying the
// factory's tracker as its refresher and producing a refresh that calls
// through to it. A regression that built its own dispatcher would leave this
// one's refresher nil.
func TestRuntimePollerWiresRefresherOntoProvidedDispatcher(t *testing.T) {
	ctx := context.Background()
	path := writeWorkflowForReloadTest(t, "linear", 30000)
	initial, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load initial workflow: %v", err)
	}
	runtime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{Initial: initial, Path: path, Source: workflow.SourceFile})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	trackerClient := &fakeIssueStateTracker{}
	trackerClient.setFetchIDStates(map[string]string{"issue-1": "In Progress"})

	dispatcher, err := NewRuntimeDispatcher(runtime, worker.Config{}, nil)
	if err != nil {
		t.Fatalf("new runtime dispatcher: %v", err)
	}
	// Same dispatcher to the actor (Deps.Dispatcher) and the poller, mirroring
	// cmd/worker/main.go.
	orch, cancel := startActor(t, Deps{Dispatcher: dispatcher, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()
	if _, err := NewRuntimePollerWithTrackerFactory(func(workflow.Config) (IssueStateLister, error) {
		return trackerClient, nil
	}, orch, runtime, dispatcher); err != nil {
		t.Fatalf("new runtime poller: %v", err)
	}

	if got := dispatcher.currentRefresher(); got != IssueStateRefresher(trackerClient) {
		t.Fatalf("dispatcher.currentRefresher() = %v, want the factory's tracker %v; constructor must wire the refresher onto the provided dispatcher", got, trackerClient)
	}

	snap := WorkflowSnapshot{Workflow: &workflow.Workflow{Config: workflow.Config{
		Tracker: workflow.TrackerConfig{ActiveStates: []string{"In Progress"}},
	}}}
	cfg := dispatcher.configForSnapshot(snap)
	if cfg.IssueStateRefresher == nil {
		t.Fatal("provided dispatcher has no IssueStateRefresher factory after construction")
	}
	fn := cfg.IssueStateRefresher(task.Task{ID: "issue-1"}, snap.Workflow.Config)
	if fn == nil {
		t.Fatal("factory returned nil for valid task on provided dispatcher")
	}
	snapshot, err := fn(ctx)
	if err != nil {
		t.Fatalf("refresher err: %v", err)
	}
	if !snapshot.Active || !snapshot.Found || snapshot.State != "In Progress" {
		t.Fatalf("snapshot = %+v, want found active In Progress; provided dispatcher should call through to the factory's tracker", snapshot)
	}
	if len(trackerClient.fetchIssueStatesByRefsCalls()) == 0 {
		t.Fatal("tracker refresher never invoked; provided dispatcher is wired to a different tracker")
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

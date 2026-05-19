package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

func TestActorStatusSnapshotRecordsDispatchAndCompletionEvents(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-42", Identifier: "ENG-42", Title: "status"}
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitForSpawn(t, disp, 1)
	disp.finishAt(0, WorkerResult{Elapsed: 25 * time.Millisecond})

	status := waitForStatus(t, o, func(status RuntimeStatus) bool {
		return status.Summary.Completed == 1 && len(status.RecentEvents) >= 2
	})

	if status.Summary.Running != 0 || status.Summary.Completed != 1 {
		t.Fatalf("summary = %+v, want completed runtime and no running", status.Summary)
	}
	if status.Summary.Candidate != 1 {
		t.Fatalf("candidate summary = %d, want observed candidate before dispatch", status.Summary.Candidate)
	}
	kinds := map[RuntimeEventKind]bool{}
	for _, ev := range status.RecentEvents {
		kinds[ev.Kind] = true
		if ev.IssueID == "ENG-42" && ev.Identifier != "ENG-42" {
			t.Fatalf("event identifier = %q, want ENG-42", ev.Identifier)
		}
	}
	if !kinds[RuntimeEventCandidate] || !kinds[RuntimeEventRunning] || !kinds[RuntimeEventCompleted] {
		t.Fatalf("events = %+v, want candidate, running, and completed", status.RecentEvents)
	}
}

func TestActorStatusSnapshotRecordsRetryableFailureEvent(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-43", Identifier: "ENG-43", Title: "status failure"}
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitForSpawn(t, disp, 1)
	disp.finishAt(0, WorkerResult{Err: context.DeadlineExceeded, Elapsed: 25 * time.Millisecond})

	status := waitForStatus(t, o, func(status RuntimeStatus) bool {
		return status.Summary.Failed == 1 && status.Summary.Retrying == 1
	})

	if status.Summary.Failed != 1 || status.Summary.Retrying != 1 {
		t.Fatalf("summary = %+v, want one failed run waiting for retry", status.Summary)
	}
	for _, ev := range status.RecentEvents {
		if ev.Kind == RuntimeEventFailed && ev.IssueID == "ENG-43" && ev.Identifier == "ENG-43" {
			return
		}
	}
	t.Fatalf("events = %+v, want failed event for ENG-43", status.RecentEvents)
}

func waitForStatus(t *testing.T, o *Orchestrator, ready func(RuntimeStatus) bool) RuntimeStatus {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var status RuntimeStatus
	for time.Now().Before(deadline) {
		var err error
		status, err = o.StatusSnapshot(context.Background(), 10)
		if err != nil {
			t.Fatalf("StatusSnapshot: %v", err)
		}
		if ready(status) {
			return status
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("status did not converge before deadline: %+v", status)
	return RuntimeStatus{}
}

func waitForSpawn(t *testing.T, disp *fakeDispatcher, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if disp.count() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("dispatcher spawn count = %d, want at least %d", disp.count(), want)
}

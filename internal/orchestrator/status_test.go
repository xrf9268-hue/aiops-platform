package orchestrator

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

func TestStatusSnapshotIncludesRuntimeStateAndRecentEvents(t *testing.T) {
	st := NewOrchestratorState(30000, 2)

	run := &RunningEntry{
		Issue:      tracker.Issue{ID: "issue-1", Identifier: "ENG-1", Title: "running issue", URL: "https://tracker.example/ENG-1"},
		Identifier: "ENG-1",
		StartedAt:  time.Unix(100, 0).UTC(),
		Workspace:  Workspace{Path: "/tmp/symphony/ENG-1"},
	}
	st.BeginDispatch("issue-1", run)
	st.RecordEvent(RuntimeEvent{
		Kind:       RuntimeEventCandidate,
		IssueID:    "issue-1",
		Identifier: "ENG-1",
		Message:    "candidate fetched from tracker",
		At:         time.Unix(99, 0).UTC(),
	})
	st.RecordEvent(RuntimeEvent{
		Kind:       RuntimeEventRunning,
		IssueID:    "issue-1",
		Identifier: "ENG-1",
		Message:    "dispatched to agent",
		Branch:     "agent/eng-1",
		PRURL:      "https://github.example/pr/1",
		At:         time.Unix(100, 0).UTC(),
	})

	status := st.StatusSnapshot(10)
	if status.Source != "orchestrator_runtime" {
		t.Fatalf("status source = %q, want orchestrator_runtime", status.Source)
	}
	if status.Summary.Candidate != 1 || status.Summary.Running != 1 {
		t.Fatalf("summary = %+v, want one candidate and one running", status.Summary)
	}
	if len(status.Running) != 1 {
		t.Fatalf("running rows = %d, want 1", len(status.Running))
	}
	if got := status.Running[0].Branch; got != "agent/eng-1" {
		t.Fatalf("running branch = %q, want agent/eng-1", got)
	}
	if len(status.RecentEvents) != 2 {
		t.Fatalf("events = %d, want 2", len(status.RecentEvents))
	}
	if status.RecentEvents[1].PRURL == "" {
		t.Fatalf("expected PR link discovered from runtime event: %+v", status.RecentEvents[1])
	}
}

func TestStatusSnapshotRecentEventsAreBoundedAndCopied(t *testing.T) {
	st := NewOrchestratorState(30000, 2)
	for i, kind := range []RuntimeEventKind{RuntimeEventCandidate, RuntimeEventRunning, RuntimeEventCompleted} {
		st.RecordEvent(RuntimeEvent{Kind: kind, IssueID: IssueID("issue"), At: time.Unix(int64(i), 0)})
	}

	status := st.StatusSnapshot(2)
	if len(status.RecentEvents) != 2 {
		t.Fatalf("events = %d, want 2", len(status.RecentEvents))
	}
	if status.RecentEvents[0].Kind != RuntimeEventRunning || status.RecentEvents[1].Kind != RuntimeEventCompleted {
		t.Fatalf("events = %+v, want last two in chronological order", status.RecentEvents)
	}
	status.RecentEvents[0].Kind = RuntimeEventFailed
	again := st.StatusSnapshot(2)
	if again.RecentEvents[0].Kind != RuntimeEventRunning {
		t.Fatal("StatusSnapshot returned events aliased to orchestrator state")
	}
}

func TestStatusSnapshotDeduplicatesEventSummaryByIssueAndKind(t *testing.T) {
	st := NewOrchestratorState(30000, 2)
	st.RecordEvent(RuntimeEvent{Kind: RuntimeEventCandidate, IssueID: "issue-1", At: time.Unix(1, 0)})
	st.RecordEvent(RuntimeEvent{Kind: RuntimeEventCandidate, IssueID: "issue-1", At: time.Unix(2, 0)})
	st.RecordEvent(RuntimeEvent{Kind: RuntimeEventBlocked, IssueID: "issue-1", At: time.Unix(3, 0)})

	status := st.StatusSnapshot(10)
	if status.Summary.Candidate != 1 {
		t.Fatalf("candidate summary = %d, want deduplicated count 1", status.Summary.Candidate)
	}
	if status.Summary.Blocked != 1 {
		t.Fatalf("blocked summary = %d, want 1", status.Summary.Blocked)
	}
}

func TestWriteStatusJSONDocumentsQueueIndependentSource(t *testing.T) {
	st := NewOrchestratorState(30000, 2)
	st.RecordEvent(RuntimeEvent{Kind: RuntimeEventBlocked, IssueID: "issue-1", Identifier: "ENG-1", Message: "blocked by dependency", At: time.Unix(101, 0).UTC()})

	var buf bytes.Buffer
	if err := WriteStatusJSON(&buf, st.StatusSnapshot(20)); err != nil {
		t.Fatalf("WriteStatusJSON: %v", err)
	}

	body := buf.String()
	if strings.Contains(body, "postgres") || strings.Contains(body, "queue") {
		t.Fatalf("status JSON should describe runtime source without legacy queue wording: %s", body)
	}
	var decoded RuntimeStatus
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("status JSON is invalid: %v", err)
	}
	if decoded.RecentEvents[0].Kind != RuntimeEventBlocked {
		t.Fatalf("decoded event kind = %q, want %q", decoded.RecentEvents[0].Kind, RuntimeEventBlocked)
	}
}

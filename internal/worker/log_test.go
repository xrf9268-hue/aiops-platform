package worker

import (
	"log"
	"strings"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
)

// captureLog redirects the stdlib log writer to a buffer for the duration of
// fn and returns whatever was emitted. The default log flags are stripped so
// assertions can match the raw structured payload.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf strings.Builder
	origOut := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(origOut)
		log.SetFlags(origFlags)
	})
	fn()
	return buf.String()
}

func TestLogIssueEventfEmitsSpec13_1Context(t *testing.T) {
	got := captureLog(t, func() {
		LogIssueEventf(task.Task{ID: "lin-42", SourceEventID: "LIN-42"}, "after_run_hook_failed", "error=%q", "exit 1")
	})
	for _, want := range []string{
		"event=after_run_hook_failed",
		"task_id=lin-42",
		"issue_id=lin-42",
		"issue_identifier=LIN-42",
		`error="exit 1"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("LogIssueEventf output missing %q in:\n%s", want, got)
		}
	}
}

// TestLogIssueEventfOmitsIdentifierWhenAbsent covers the case where Task was
// hand-constructed without SourceEventID (e.g. legacy tests). The identifier
// key is omitted rather than emitted as an empty value, so log filters do
// not misinterpret `issue_identifier=` as a real empty identifier.
func TestLogIssueEventfOmitsIdentifierWhenAbsent(t *testing.T) {
	got := captureLog(t, func() {
		LogIssueEventf(task.Task{ID: "lin-7"}, "workspace_remove_failed", "reason=%s", "afterhook")
	})
	if strings.Contains(got, "issue_identifier=") {
		t.Errorf("missing identifier should not emit empty issue_identifier= ; got:\n%s", got)
	}
	if !strings.Contains(got, "task_id=lin-7") || !strings.Contains(got, "issue_id=lin-7") {
		t.Errorf("missing required task/issue ids in:\n%s", got)
	}
}

func TestLogTaskIDEventfMirrorsTaskAndIssueIDs(t *testing.T) {
	got := captureLog(t, func() {
		LogTaskIDEventf("tsk-9", "ENG-9", "verification_write_failed", "error=%q", "disk full")
	})
	for _, want := range []string{
		"event=verification_write_failed",
		"task_id=tsk-9",
		"issue_id=tsk-9",
		"issue_identifier=ENG-9",
		`error="disk full"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("LogTaskIDEventf output missing %q in:\n%s", want, got)
		}
	}
}

// TestLogTaskIDEventfOmitsEmptyIdentifier covers the reconcile path where
// the caller has only a synthetic task id and no tracker identifier: the
// helper must omit the `issue_identifier=` key entirely rather than emit an
// empty value (which would mis-parse in log aggregators).
func TestLogTaskIDEventfOmitsEmptyIdentifier(t *testing.T) {
	got := captureLog(t, func() {
		LogTaskIDEventf("reconcile-startup", "", "kept_active_workspace", "key=%s", "lin-1")
	})
	if strings.Contains(got, "issue_identifier=") {
		t.Errorf("empty identifier should omit the key, got:\n%s", got)
	}
}

// TestLogIssueSessionEventfEmitsSessionID covers SPEC §13.1's REQUIRED
// `session_id` context for coding-agent session logs: helpers fired after
// `session_started` must carry the session id alongside the existing
// task/issue identifiers.
func TestLogIssueSessionEventfEmitsSessionID(t *testing.T) {
	got := captureLog(t, func() {
		LogIssueSessionEventf(task.Task{ID: "lin-42", SourceEventID: "LIN-42"}, "thread-1-turn-1", "after_run_hook_failed", "error=%q", "exit 1")
	})
	for _, want := range []string{
		"event=after_run_hook_failed",
		"task_id=lin-42",
		"issue_id=lin-42",
		"issue_identifier=LIN-42",
		"session_id=thread-1-turn-1",
		`error="exit 1"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("LogIssueSessionEventf output missing %q in:\n%s", want, got)
		}
	}
}

// TestLogIssueSessionEventfOmitsEmptySessionID covers the pre-session path
// where session_started has not fired yet (e.g. the runner failed during
// process spawn). The helper must omit `session_id=` rather than emit an
// empty value so log aggregators do not mis-parse it as a real id.
func TestLogIssueSessionEventfOmitsEmptySessionID(t *testing.T) {
	got := captureLog(t, func() {
		LogIssueSessionEventf(task.Task{ID: "lin-7", SourceEventID: "LIN-7"}, "", "after_run_hook_failed", "error=%q", "spawn")
	})
	if strings.Contains(got, "session_id=") {
		t.Errorf("empty session id should omit the key, got:\n%s", got)
	}
	if !strings.Contains(got, "task_id=lin-7") || !strings.Contains(got, "issue_identifier=LIN-7") {
		t.Errorf("missing required §13.1 context in:\n%s", got)
	}
}

func TestLogTaskIDSessionEventfEmitsSessionID(t *testing.T) {
	got := captureLog(t, func() {
		LogTaskIDSessionEventf("tsk-9", "ENG-9", "thread-2-turn-3", "verification_write_failed", "error=%q", "disk full")
	})
	for _, want := range []string{
		"event=verification_write_failed",
		"task_id=tsk-9",
		"issue_id=tsk-9",
		"issue_identifier=ENG-9",
		"session_id=thread-2-turn-3",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("LogTaskIDSessionEventf output missing %q in:\n%s", want, got)
		}
	}
}

func TestSessionIDFromRuntimeEventsExtractsSessionStartedPayload(t *testing.T) {
	got := sessionIDFromRuntimeEvents([]task.RuntimeEvent{
		{Event: task.EventTurnCompleted, Payload: map[string]any{"thread_id": "t-2"}},
		{Event: task.EventSessionStarted, Payload: map[string]any{"session_id": "thread-1-turn-1", "thread_id": "thread-1"}},
		{Event: task.EventTurnCompleted, Payload: map[string]any{"thread_id": "t-2"}},
	})
	if got != "thread-1-turn-1" {
		t.Errorf("sessionIDFromRuntimeEvents = %q, want thread-1-turn-1", got)
	}
}

func TestSessionIDFromRuntimeEventsReturnsEmptyWhenAbsent(t *testing.T) {
	if got := sessionIDFromRuntimeEvents(nil); got != "" {
		t.Errorf("nil events: got %q, want \"\"", got)
	}
	got := sessionIDFromRuntimeEvents([]task.RuntimeEvent{
		{Event: task.EventTurnCompleted, Payload: map[string]any{"thread_id": "t-2"}},
	})
	if got != "" {
		t.Errorf("no session_started: got %q, want \"\"", got)
	}
}

func TestLogReconcileEventfOmitsIssueContext(t *testing.T) {
	got := captureLog(t, func() {
		LogReconcileEventf("startup_reconciliation_failed", "error=%q", "boom")
	})
	if !strings.Contains(got, "event=startup_reconciliation_failed") {
		t.Errorf("missing event= in:\n%s", got)
	}
	if strings.Contains(got, "task_id=") || strings.Contains(got, "issue_id=") {
		t.Errorf("reconciliation log must not carry per-issue context, got:\n%s", got)
	}
}

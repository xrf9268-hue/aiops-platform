// SPEC §13.1 mandates `key=value` structured logging carrying `issue_id`,
// `issue_identifier` (and `session_id` for coding-agent session logs) on
// every issue-related log line. The helpers in this file produce that
// shape without dragging in a structured-logger dependency; ad-hoc
// `log.Printf` lines in the worker / orchestrator route through them so
// operators tailing stderr can correlate with `/api/v1/state`
// (`issue_id`/`issue_identifier`) and grep / Loki / journald see real
// fields instead of prose.

package worker

import (
	"fmt"
	"log"
	"strings"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
)

// LogIssueEventf formats a SPEC §13.1 issue-lifecycle log line for a known
// task: `event=<kind> task_id=<id> issue_id=<id> issue_identifier=<src> <msg>`.
// Pair with Emit() for runtime-event surfaces; this helper covers the
// log-only failure paths (workspace hooks, retry-queue plumbing, artifact
// writes) where the orchestrator does not also fold a runtime event.
func LogIssueEventf(t task.Task, event, format string, args ...any) {
	log.Printf("%s %s", issueLogPrefix(event, t), fmt.Sprintf(format, args...))
}

// LogTaskIDEventf is the narrow form used by call sites that only have the
// task id (and optionally the issue identifier) in scope rather than the
// full Task struct. Pass identifier="" when the caller does not know it;
// the issue_identifier= key is then omitted (rather than emitted empty) so
// log filters do not misparse it as a real empty identifier. `issue_id` is
// set equal to `task_id` because TaskFromIssue keys Task.ID by the tracker
// issue id.
func LogTaskIDEventf(taskID, identifier, event, format string, args ...any) {
	log.Printf("%s %s", taskIDLogPrefix(event, taskID, identifier), fmt.Sprintf(format, args...))
}

// LogReconcileEventf is the helper for reconciliation / orchestrator-wide
// log lines that have no per-issue context yet (startup reconcile fetches,
// poll-loop errors). It still emits `event=<kind>` so log filters can group
// by event class instead of regex-matching prose.
func LogReconcileEventf(event, format string, args ...any) {
	log.Printf("event=%s %s", event, fmt.Sprintf(format, args...))
}

func issueLogPrefix(event string, t task.Task) string {
	var b strings.Builder
	b.Grow(64)
	b.WriteString("event=")
	b.WriteString(event)
	b.WriteString(" task_id=")
	b.WriteString(t.ID)
	b.WriteString(" issue_id=")
	b.WriteString(t.ID)
	if t.SourceEventID != "" {
		b.WriteString(" issue_identifier=")
		b.WriteString(t.SourceEventID)
	}
	return b.String()
}

func taskIDLogPrefix(event, taskID, identifier string) string {
	prefix := "event=" + event + " task_id=" + taskID + " issue_id=" + taskID
	if identifier != "" {
		prefix += " issue_identifier=" + identifier
	}
	return prefix
}

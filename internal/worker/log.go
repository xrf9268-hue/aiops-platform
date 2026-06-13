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

// sessionIDFromRuntimeEvents scans runner-produced runtime events for the
// `session_started` event (SPEC §10.4) and returns the `session_id` payload
// field it carries (e.g. `<thread_id>-<turn_id>`). Returns "" when no
// session_started event is present, so callers can pass the result through
// the session-aware log helpers without branching on whether the runner
// reached its session-handshake phase.
func sessionIDFromRuntimeEvents(events []task.RuntimeEvent) string {
	for _, ev := range events {
		if ev.Event != task.EventSessionStarted {
			continue
		}
		payload, ok := ev.Payload.(map[string]any)
		if !ok {
			continue
		}
		if id, ok := payload["session_id"].(string); ok && id != "" {
			return id
		}
	}
	return ""
}

// LogIssueEventf formats a SPEC §13.1 issue-lifecycle log line for a known
// task: `event=<kind> task_id=<id> issue_id=<id> issue_identifier=<src> <msg>`.
// It is the log-only counterpart to Emit(): use it on failure paths that
// record an issue-lifecycle line without also folding a runtime event.
func LogIssueEventf(t task.Task, event, format string, args ...any) {
	log.Printf("%s %s", issueLogPrefix(event, t, ""), fmt.Sprintf(format, args...))
}

// LogIssueSessionEventf is the session-aware variant for log lines that
// fire after `session_started`: it threads SPEC §13.1's REQUIRED
// `session_id` field through alongside the standard issue context. Pass
// sessionID="" to fall back to the pre-session shape (the key is omitted
// rather than emitted empty so aggregators do not mis-parse it).
func LogIssueSessionEventf(t task.Task, sessionID, event, format string, args ...any) {
	log.Printf("%s %s", issueLogPrefix(event, t, sessionID), fmt.Sprintf(format, args...))
}

// LogTaskIDEventf is the narrow form used by call sites that only have the
// task id (and optionally the issue identifier) in scope rather than the
// full Task struct. Pass identifier="" when the caller does not know it;
// the issue_identifier= key is then omitted (rather than emitted empty) so
// log filters do not misparse it as a real empty identifier. `issue_id` is
// set equal to `task_id` because TaskFromIssue keys Task.ID by the tracker
// issue id.
func LogTaskIDEventf(taskID, identifier, event, format string, args ...any) {
	log.Printf("%s %s", taskIDLogPrefix(event, taskID, identifier, ""), fmt.Sprintf(format, args...))
}

// LogTaskIDSessionEventf is the session-aware variant of LogTaskIDEventf
// for callers that have only the task id in scope but already know the
// session id (e.g. the post-runner event_emit failure path).
func LogTaskIDSessionEventf(taskID, identifier, sessionID, event, format string, args ...any) {
	log.Printf("%s %s", taskIDLogPrefix(event, taskID, identifier, sessionID), fmt.Sprintf(format, args...))
}

// LogReconcileEventf is the helper for reconciliation / orchestrator-wide
// log lines that have no per-issue context yet (startup reconcile fetches,
// poll-loop errors). It still emits `event=<kind>` so log filters can group
// by event class instead of regex-matching prose.
func LogReconcileEventf(event, format string, args ...any) {
	log.Printf("event=%s %s", event, fmt.Sprintf(format, args...))
}

func issueLogPrefix(event string, t task.Task, sessionID string) string {
	var b strings.Builder
	b.Grow(96)
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
	if sessionID != "" {
		b.WriteString(" session_id=")
		b.WriteString(sessionID)
	}
	return b.String()
}

func taskIDLogPrefix(event, taskID, identifier, sessionID string) string {
	prefix := "event=" + event + " task_id=" + taskID + " issue_id=" + taskID
	if identifier != "" {
		prefix += " issue_identifier=" + identifier
	}
	if sessionID != "" {
		prefix += " session_id=" + sessionID
	}
	return prefix
}

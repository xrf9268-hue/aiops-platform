package runner

// codex_app_server_turn_test.go holds characterization tests that drive
// (*appServerClient).awaitTurnCompletion directly over an in-memory JSON-RPC
// stream, without spawning a real `codex app-server` subprocess. They pin the
// turn loop's input→(returned error, recorded runtime events, continuation
// signal) behavior at the exact function boundary before #499 decomposes the
// 78-cognitive-complexity loop into per-concern handlers; the assertions must
// hold identically before and after that refactor.
//
// The end-to-end subprocess tests in codex_app_server_test.go remain the
// authority for the transport, sandbox, and multi-turn paths; these focus on
// the single-turn message demux that the decomposition reshapes, especially
// branches the subprocess suite does not exercise (input-required
// notifications, non-protocol decode errors, the auto-approve vs. decline
// server-request fork).

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
)

// newTurnLoopClient builds an appServerClient whose stdout stream is the given
// newline-delimited JSON-RPC lines. Server-request replies and tool-call
// outputs land in the returned stdin buffer. opts mutate the client before the
// loop runs (e.g. to set stallTimeoutMs or override the approval policy).
func newTurnLoopClient(t *testing.T, lines []string, opts ...func(*appServerClient)) (*appServerClient, *bytes.Buffer) {
	t.Helper()
	var stdout bytes.Buffer
	for _, line := range lines {
		stdout.WriteString(line)
		stdout.WriteByte('\n')
	}
	sc := bufio.NewScanner(&stdout)
	sc.Buffer(make([]byte, 0, appServerScannerInitialBuf), maxAppServerLineBytes+1)
	stdin := &bytes.Buffer{}
	c := &appServerClient{
		scanner:        sc,
		stdin:          stdin,
		out:            io.Discard,
		approvalPolicy: "never",
	}
	for _, opt := range opts {
		opt(c)
	}
	startReaderForTest(t, c)
	return c, stdin
}

func runtimeEventNames(c *appServerClient) []string {
	names := make([]string, len(c.runtimeEvents))
	for i, ev := range c.runtimeEvents {
		names[i] = ev.Event
	}
	return names
}

func TestAwaitTurnCompletion_TurnCompletedSuccess(t *testing.T) {
	c, _ := newTurnLoopClient(t, []string{
		`{"method":"turn/completed","params":{"lastAssistantMessage":"all done","continue":false}}`,
	})
	if err := c.awaitTurnCompletion(context.Background()); err != nil {
		t.Fatalf("awaitTurnCompletion() = %v; want nil on turn/completed success", err)
	}
	if got, want := c.summary(), "all done"; got != want {
		t.Errorf("summary() = %q; want %q", got, want)
	}
	if c.continueRun {
		t.Errorf("continueRun = true; want false (params.continue=false)")
	}
	if got, want := runtimeEventNames(c), []string{task.EventTurnCompleted}; !slices.Equal(got, want) {
		t.Errorf("runtime events = %v; want %v", got, want)
	}
}

func TestAwaitTurnCompletion_TurnCompletedSetsContinueRun(t *testing.T) {
	c, _ := newTurnLoopClient(t, []string{
		`{"method":"turn/completed","params":{"continue":true,"lastAssistantMessage":"keep going"}}`,
	})
	if err := c.awaitTurnCompletion(context.Background()); err != nil {
		t.Fatalf("awaitTurnCompletion() = %v; want nil", err)
	}
	if !c.continueRun {
		t.Errorf("continueRun = false; want true (params.continue=true)")
	}
}

func TestAwaitTurnCompletion_TurnCompletedFailedStatusIsTurnFailed(t *testing.T) {
	c, _ := newTurnLoopClient(t, []string{
		`{"method":"turn/completed","params":{"status":"failed","reason":"boom"}}`,
	})
	err := c.awaitTurnCompletion(context.Background())
	if cat, ok := ErrorCategory(err); !ok || cat != CategoryTurnFailed {
		t.Fatalf("ErrorCategory(%v) = %q,%v; want %q,true", err, cat, ok, CategoryTurnFailed)
	}
	if got, want := runtimeEventNames(c), []string{task.EventTurnEndedWithError}; !slices.Equal(got, want) {
		t.Errorf("runtime events = %v; want %v (failed turn/completed records turn_ended_with_error)", got, want)
	}
}

func TestAwaitTurnCompletion_TurnFailed(t *testing.T) {
	c, _ := newTurnLoopClient(t, []string{
		`{"method":"turn/failed","params":{"reason":"explode"}}`,
	})
	err := c.awaitTurnCompletion(context.Background())
	if cat, ok := ErrorCategory(err); !ok || cat != CategoryTurnFailed {
		t.Fatalf("ErrorCategory(%v) = %q,%v; want %q,true", err, cat, ok, CategoryTurnFailed)
	}
	if got, want := runtimeEventNames(c), []string{task.EventTurnFailed}; !slices.Equal(got, want) {
		t.Errorf("runtime events = %v; want %v", got, want)
	}
}

func TestAwaitTurnCompletion_TurnCancelled(t *testing.T) {
	c, _ := newTurnLoopClient(t, []string{
		`{"method":"turn/cancelled","params":{"reason":"user aborted"}}`,
	})
	err := c.awaitTurnCompletion(context.Background())
	if cat, ok := ErrorCategory(err); !ok || cat != CategoryTurnCancelled {
		t.Fatalf("ErrorCategory(%v) = %q,%v; want %q,true", err, cat, ok, CategoryTurnCancelled)
	}
	if got, want := runtimeEventNames(c), []string{task.EventTurnCancelled}; !slices.Equal(got, want) {
		t.Errorf("runtime events = %v; want %v", got, want)
	}
}

func TestAwaitTurnCompletion_UsageLimitIsQuotaBackoff(t *testing.T) {
	c, _ := newTurnLoopClient(t, []string{
		`{"method":"turn/completed","params":{"status":"failed","turn":{"error":{"codexErrorInfo":"usageLimitExceeded","message":"please try again in 30 seconds"}}}}`,
	})
	err := c.awaitTurnCompletion(context.Background())
	var quota *QuotaBackoffError
	if !errors.As(err, &quota) {
		t.Fatalf("awaitTurnCompletion() = %v; want *QuotaBackoffError", err)
	}
	if quota.RetryAfter <= 0 {
		t.Errorf("QuotaBackoffError.RetryAfter = %v; want >0 parsed from retry text", quota.RetryAfter)
	}
	if got, want := runtimeEventNames(c), []string{task.EventTurnEndedWithError}; !slices.Equal(got, want) {
		t.Errorf("runtime events = %v; want %v", got, want)
	}
}

func TestAwaitTurnCompletion_NotificationRecordedThenContinues(t *testing.T) {
	c, _ := newTurnLoopClient(t, []string{
		`{"method":"item/agentMessage","params":{"message":"thinking"}}`,
		`{"method":"turn/completed","params":{}}`,
	})
	if err := c.awaitTurnCompletion(context.Background()); err != nil {
		t.Fatalf("awaitTurnCompletion() = %v; want nil", err)
	}
	want := []string{task.EventNotification, task.EventTurnCompleted}
	if got := runtimeEventNames(c); !slices.Equal(got, want) {
		t.Errorf("runtime events = %v; want %v", got, want)
	}
}

func TestAwaitTurnCompletion_OtherMessageRecordedThenContinues(t *testing.T) {
	c, _ := newTurnLoopClient(t, []string{
		`{"foo":"bar"}`,
		`{"method":"turn/completed","params":{}}`,
	})
	if err := c.awaitTurnCompletion(context.Background()); err != nil {
		t.Fatalf("awaitTurnCompletion() = %v; want nil", err)
	}
	want := []string{task.EventOtherMessage, task.EventTurnCompleted}
	if got := runtimeEventNames(c); !slices.Equal(got, want) {
		t.Errorf("runtime events = %v; want %v (method-less message records other_message)", got, want)
	}
}

func TestAwaitTurnCompletion_MalformedProtocolLineRecordedThenContinues(t *testing.T) {
	c, _ := newTurnLoopClient(t, []string{
		`{"oops": not-valid-json`,
		`{"method":"turn/completed","params":{}}`,
	})
	if err := c.awaitTurnCompletion(context.Background()); err != nil {
		t.Fatalf("awaitTurnCompletion() = %v; want nil (malformed protocol-like line is reported then skipped)", err)
	}
	want := []string{task.EventMalformed, task.EventTurnCompleted}
	if got := runtimeEventNames(c); !slices.Equal(got, want) {
		t.Errorf("runtime events = %v; want %v", got, want)
	}
}

func TestAwaitTurnCompletion_NonProtocolLineReturnsDecodeError(t *testing.T) {
	c, _ := newTurnLoopClient(t, []string{`plain text not json`})
	err := c.awaitTurnCompletion(context.Background())
	// A line that is not even a JSON object is a hard decode failure surfaced
	// as a CategoryResponseError (classify by category, not by message text —
	// AGENTS.md rule 8), not a recorded-and-skipped malformed line.
	if cat, ok := ErrorCategory(err); !ok || cat != CategoryResponseError {
		t.Fatalf("awaitTurnCompletion() error category = %q,%v; want %q,true", cat, ok, CategoryResponseError)
	}
	if got := runtimeEventNames(c); len(got) != 0 {
		t.Errorf("runtime events = %v; want none (non-protocol garbage is a hard decode failure)", got)
	}
}

func TestAwaitTurnCompletion_InputRequiredNotification(t *testing.T) {
	c, _ := newTurnLoopClient(t, []string{`{"method":"turn/input_required","params":{}}`})
	err := c.awaitTurnCompletion(context.Background())
	if !IsInputRequired(err) {
		t.Fatalf("awaitTurnCompletion() = %v; want *InputRequiredError", err)
	}
	var ire *InputRequiredError
	errors.As(err, &ire)
	if ire.Method != "turn/input_required" {
		t.Errorf("InputRequiredError.Method = %q; want %q", ire.Method, "turn/input_required")
	}
	if got, want := runtimeEventNames(c), []string{task.EventTurnInputRequired}; !slices.Equal(got, want) {
		t.Errorf("runtime events = %v; want %v", got, want)
	}
}

func TestAwaitTurnCompletion_ServerRequestAutoApprovedThenContinues(t *testing.T) {
	c, stdin := newTurnLoopClient(t, []string{
		`{"id":7,"method":"item/commandExecution/requestApproval","params":{"command":"ls"}}`,
		`{"method":"turn/completed","params":{}}`,
	})
	if err := c.awaitTurnCompletion(context.Background()); err != nil {
		t.Fatalf("awaitTurnCompletion() = %v; want nil", err)
	}
	want := []string{task.EventApprovalAutoApproved, task.EventTurnCompleted}
	if got := runtimeEventNames(c); !slices.Equal(got, want) {
		t.Errorf("runtime events = %v; want %v", got, want)
	}
	if reply := stdin.String(); !strings.Contains(reply, "acceptForSession") {
		t.Errorf("server-request reply = %q; want it to carry acceptForSession", reply)
	}
}

func TestAwaitTurnCompletion_ServerRequestDeclinedIsInputRequired(t *testing.T) {
	c, stdin := newTurnLoopClient(t, []string{
		`{"id":9,"method":"item/commandExecution/requestApproval","params":{"command":"rm -rf /"}}`,
	}, func(c *appServerClient) { c.approvalPolicy = "on-request" })
	err := c.awaitTurnCompletion(context.Background())
	if !IsInputRequired(err) {
		t.Fatalf("awaitTurnCompletion() = %v; want *InputRequiredError under operator-supervised policy", err)
	}
	if got, want := runtimeEventNames(c), []string{task.EventTurnInputRequired}; !slices.Equal(got, want) {
		t.Errorf("runtime events = %v; want %v", got, want)
	}
	if reply := stdin.String(); !strings.Contains(reply, "decline") {
		t.Errorf("server-request reply = %q; want it to carry decline", reply)
	}
}

func TestAwaitTurnCompletion_RequestUserInputIsInputRequired(t *testing.T) {
	c, stdin := newTurnLoopClient(t, []string{
		`{"id":3,"method":"item/tool/requestUserInput","params":{"questions":[{"id":"q1"}]}}`,
	})
	err := c.awaitTurnCompletion(context.Background())
	if !IsInputRequired(err) {
		t.Fatalf("awaitTurnCompletion() = %v; want *InputRequiredError", err)
	}
	if got, want := runtimeEventNames(c), []string{task.EventTurnInputRequired}; !slices.Equal(got, want) {
		t.Errorf("runtime events = %v; want %v", got, want)
	}
	if reply := stdin.String(); !strings.Contains(reply, nonInteractiveInputReply) {
		t.Errorf("user-input reply = %q; want it to carry the non-interactive answer", reply)
	}
}

func TestAwaitTurnCompletion_UnsupportedToolCallRecordedThenContinues(t *testing.T) {
	// With an empty tool set, item/tool/call resolves as an unsupported tool:
	// the wire still gets a structured failure result and the loop continues,
	// refreshing the stall clock, to the terminal turn/completed.
	c, stdin := newTurnLoopClient(t, []string{
		`{"id":5,"method":"item/tool/call","params":{"tool":"nope","arguments":{}}}`,
		`{"method":"turn/completed","params":{}}`,
	})
	if err := c.awaitTurnCompletion(context.Background()); err != nil {
		t.Fatalf("awaitTurnCompletion() = %v; want nil", err)
	}
	want := []string{task.EventUnsupportedToolCall, task.EventTurnCompleted}
	if got := runtimeEventNames(c); !slices.Equal(got, want) {
		t.Errorf("runtime events = %v; want %v", got, want)
	}
	if reply := stdin.String(); !strings.Contains(reply, "unsupported dynamic tool") {
		t.Errorf("tool-call reply = %q; want it to carry the unsupported-tool failure", reply)
	}
}

func TestReadTurnMessage_PreReadBudgetExhaustedReturnsStall(t *testing.T) {
	// When the stall budget is already spent at the top of an iteration,
	// readTurnMessage returns a *StallError WITHOUT reading — distinct from the
	// in-read timeout, which carries a non-nil Cause. The live loop resets
	// lastTerminal every iteration so this guard is defensive; drive the helper
	// directly to pin both the Cause==nil discriminator and the no-read
	// short-circuit (awaitTurnCompletion resets lastTerminal on entry, so the
	// branch is unreachable through the full loop).
	c, _ := newTurnLoopClient(t, []string{`{"method":"turn/completed","params":{}}`},
		func(c *appServerClient) { c.stallTimeoutMs = 10 })
	c.lastTerminal = time.Now().Add(-time.Second) // budget already exhausted
	msg, raw, stallBudget, err := c.readTurnMessage(context.Background())
	var stall *StallError
	if !errors.As(err, &stall) {
		t.Fatalf("readTurnMessage() err = %v; want *StallError on an already-spent budget", err)
	}
	if stall.Cause != nil {
		t.Errorf("StallError.Cause = %v; want nil (pre-read exhaustion has no underlying read error)", stall.Cause)
	}
	if msg != nil || raw != nil {
		t.Errorf("readTurnMessage() msg=%v raw=%v; want nil,nil (no read performed)", msg, raw)
	}
	if got, want := stallBudget, 10*time.Millisecond; got != want {
		t.Errorf("readTurnMessage() stallBudget = %v; want %v", got, want)
	}
	// The scanner is untouched: the unread turn/completed line is still there,
	// proving readTurnMessage short-circuited before reading.
	if line, lerr := c.readLine(context.Background()); lerr != nil || !strings.Contains(string(line), "turn/completed") {
		t.Errorf("next line after pre-read stall = %q (err %v); want the unconsumed turn/completed line", line, lerr)
	}
}

func TestHandleTurnNotification_LateNotificationSurfacesStall(t *testing.T) {
	// A notification dispatched after the stall budget has already elapsed ends
	// the turn with a *StallError. In the live loop the read deadline is tied to
	// the same budget, so the read-budget stall (classifyTurnReadError) normally
	// fires first and this notification-path branch is defensive; drive the
	// extracted handler directly to pin it without relying on read timing.
	c := &appServerClient{out: io.Discard, stallTimeoutMs: 10}
	c.lastTerminal = time.Now().Add(-time.Second) // already past the 10ms budget
	done, err := c.handleTurnNotification(map[string]any{"method": "item/agentMessage"}, "item/agentMessage")
	if !done {
		t.Fatalf("handleTurnNotification() done = false; want true when the stall budget is already spent")
	}
	if !IsStall(err) {
		t.Fatalf("handleTurnNotification() err = %v; want *StallError", err)
	}
}

func TestAwaitTurnCompletion_StallTimeoutWhenStreamSilent(t *testing.T) {
	// A live stream that stops emitting for longer than stall_timeout_ms must
	// surface a *StallError rather than block forever. io.Pipe never delivers a
	// line, so the per-read stall budget elapses on the first iteration.
	pr, pw := io.Pipe()
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 0, appServerScannerInitialBuf), maxAppServerLineBytes+1)
	c := &appServerClient{scanner: sc, out: io.Discard, approvalPolicy: "never", stallTimeoutMs: 50}
	c.startStdoutReader()
	// The reader parks in scanner.Scan on the silent pipe; the stall fires via the
	// consumer's per-read context deadline, not the reader. Close the pipe so the
	// parked reader observes EOF (it cannot reach EOF on its own here), then drain
	// readCh to join it.
	t.Cleanup(func() {
		_ = pw.Close()
		for range c.readCh { //nolint:revive // drain-to-close joins the reader
		}
	})
	err := c.awaitTurnCompletion(context.Background())
	if !IsStall(err) {
		t.Fatalf("awaitTurnCompletion() = %v; want *StallError when the stream goes silent", err)
	}
}

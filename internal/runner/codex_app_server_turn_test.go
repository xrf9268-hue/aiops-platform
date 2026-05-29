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

	"github.com/xrf9268-hue/aiops-platform/internal/task"
)

// newTurnLoopClient builds an appServerClient whose stdout stream is the given
// newline-delimited JSON-RPC lines. Server-request replies and tool-call
// outputs land in the returned stdin buffer. opts mutate the client before the
// loop runs (e.g. to set stallTimeoutMs or override the approval policy).
func newTurnLoopClient(lines []string, opts ...func(*appServerClient)) (*appServerClient, *bytes.Buffer) {
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
	c, _ := newTurnLoopClient([]string{
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
	c, _ := newTurnLoopClient([]string{
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
	c, _ := newTurnLoopClient([]string{
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
	c, _ := newTurnLoopClient([]string{
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
	c, _ := newTurnLoopClient([]string{
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
	c, _ := newTurnLoopClient([]string{
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
	c, _ := newTurnLoopClient([]string{
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
	c, _ := newTurnLoopClient([]string{
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
	c, _ := newTurnLoopClient([]string{
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
	c, _ := newTurnLoopClient([]string{`plain text not json`})
	err := c.awaitTurnCompletion(context.Background())
	if err == nil || !strings.Contains(err.Error(), "decode codex app-server message") {
		t.Fatalf("awaitTurnCompletion() = %v; want decode error", err)
	}
	if got := runtimeEventNames(c); len(got) != 0 {
		t.Errorf("runtime events = %v; want none (non-protocol garbage is a hard decode failure)", got)
	}
}

func TestAwaitTurnCompletion_InputRequiredNotification(t *testing.T) {
	c, _ := newTurnLoopClient([]string{`{"method":"turn/input_required","params":{}}`})
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
	c, stdin := newTurnLoopClient([]string{
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
	c, stdin := newTurnLoopClient([]string{
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
	c, stdin := newTurnLoopClient([]string{
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
	c, stdin := newTurnLoopClient([]string{
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

func TestAwaitTurnCompletion_StallTimeoutWhenStreamSilent(t *testing.T) {
	// A live stream that stops emitting for longer than stall_timeout_ms must
	// surface a *StallError rather than block forever. io.Pipe never delivers a
	// line, so the per-read stall budget elapses on the first iteration.
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 0, appServerScannerInitialBuf), maxAppServerLineBytes+1)
	c := &appServerClient{scanner: sc, out: io.Discard, approvalPolicy: "never", stallTimeoutMs: 50}
	err := c.awaitTurnCompletion(context.Background())
	if !IsStall(err) {
		t.Fatalf("awaitTurnCompletion() = %v; want *StallError when the stream goes silent", err)
	}
}

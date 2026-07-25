package runner

// codex_app_server_turn_test.go holds characterization tests that drive
// (*appServerClient).awaitTurnCompletion directly over an in-memory JSON-RPC
// stream, without spawning a real `codex app-server` subprocess. They pin the
// turn loop's input→(returned error, recorded runtime events) behavior at the
// exact function boundary before #499 decomposes the
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
	"encoding/json"
	"errors"
	"io"
	"reflect"
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
		`{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","items":[],"status":"completed"}}}`,
	})
	if err := c.awaitTurnCompletion(context.Background()); err != nil {
		t.Fatalf("awaitTurnCompletion() = %v; want nil on turn/completed success", err)
	}
	if got, want := runtimeEventNames(c), []string{task.EventTurnCompleted}; !slices.Equal(got, want) {
		t.Errorf("runtime events = %v; want %v", got, want)
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

func TestAwaitTurnCompletion_LocalEventSinkTimeIsNotStreamSilence(t *testing.T) {
	c, _ := newTurnLoopClient(t, []string{
		`{"method":"item/agentMessage","params":{"message":"thinking"}}`,
		`{"method":"turn/completed","params":{}}`,
	}, func(c *appServerClient) {
		c.turnTimeoutMs = 20
		c.runtimeEventSink = func(event task.RuntimeEvent) {
			if event.Event == task.EventNotification {
				time.Sleep(50 * time.Millisecond)
			}
		}
	})

	if err := c.awaitTurnCompletion(context.Background()); err != nil {
		t.Fatalf("awaitTurnCompletion() = %v; want local sink time excluded from stream silence", err)
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

func TestAwaitTurnCompletion_AutoAnswersRecognizedMCPQuestionsAtomically(t *testing.T) {
	c, stdin := newTurnLoopClient(t, []string{
		`{"id":3,"method":"item/tool/requestUserInput","params":{"questions":[{"id":"mcp_tool_call_approval_session","options":[{"label":"Approve Once"},{"label":"Approve this Session"},{"label":"Deny"}]},{"id":"mcp_tool_call_approval_once","options":[{"label":"Allow this tool"},{"label":"Approve Once"}]},{"id":"mcp_tool_call_approval_allow","options":[{"label":"Deny"},{"label":"  ALLOW for this request  "}]},{"id":"mcp_tool_call_approval_approve","options":[{"label":"Deny"},{"label":"  approve for this repository  "}]}]}}`,
		`{"method":"turn/completed","params":{}}`,
	})
	if err := c.awaitTurnCompletion(context.Background()); err != nil {
		t.Fatalf("awaitTurnCompletion() = %v; want nil", err)
	}
	if got, want := runtimeEventNames(c), []string{task.EventApprovalAutoApproved, task.EventTurnCompleted}; !slices.Equal(got, want) {
		t.Errorf("runtime events = %v; want %v", got, want)
	}

	var reply map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(stdin.Bytes()), &reply); err != nil {
		t.Fatalf("decode single user-input reply %q: %v", stdin.String(), err)
	}
	wantResult := map[string]any{"answers": map[string]any{
		"mcp_tool_call_approval_session": map[string]any{"answers": []any{"Approve this Session"}},
		"mcp_tool_call_approval_once":    map[string]any{"answers": []any{"Approve Once"}},
		"mcp_tool_call_approval_allow":   map[string]any{"answers": []any{"  ALLOW for this request  "}},
		"mcp_tool_call_approval_approve": map[string]any{"answers": []any{"  approve for this repository  "}},
	}}
	if got := reply["result"]; !reflect.DeepEqual(got, wantResult) {
		t.Fatalf("user-input result = %#v; want %#v", got, wantResult)
	}
	if reply["jsonrpc"] != "2.0" || reply["id"] != float64(3) {
		t.Errorf("user-input reply envelope = %#v; want jsonrpc 2.0 and id 3", reply)
	}
	eventPayload, _ := c.runtimeEvents[0].Payload.(map[string]any)
	if got := eventPayload["method"]; got != "item/tool/requestUserInput" {
		t.Errorf("approval_auto_approved method = %#v; want item/tool/requestUserInput", got)
	}
	if got := eventPayload["decision"]; got != "Approve this Session" {
		t.Errorf("approval_auto_approved decision = %#v; want Approve this Session", got)
	}
	wireResult, _ := json.Marshal(reply["result"])
	eventResult, _ := json.Marshal(eventPayload["result"])
	if !bytes.Equal(eventResult, wireResult) {
		t.Errorf("approval_auto_approved result = %s; want wire result %s", eventResult, wireResult)
	}
}

func TestAwaitTurnCompletion_UnsafeUserInputWritesNothing(t *testing.T) {
	recognized := `{"id":"mcp_tool_call_approval_ok","options":[{"label":"Approve this Session"}]}`
	tests := []struct {
		name   string
		policy any
		params string
	}{
		{
			name:   "generic Allow Deny",
			policy: "never",
			params: `{"questions":[{"id":"generic-question","options":[{"label":"Allow"},{"label":"Deny"}]}]}`,
		},
		{
			name:   "freeform",
			policy: "never",
			params: `{"questions":[{"id":"freeform-question","options":null}]}`,
		},
		{name: "empty questions", policy: "never", params: `{"questions":[]}`},
		{
			name:   "malformed question",
			policy: "never",
			params: `{"questions":[{"id":7,"options":[{"label":"Approve Once"}]}]}`,
		},
		{
			name:   "near miss approval id",
			policy: "never",
			params: `{"questions":[{"id":"x-mcp_tool_call_approval_call-1","options":[{"label":"Approve this Session"}]}]}`,
		},
		{
			name:   "mixed generic question",
			policy: "never",
			params: `{"questions":[` + recognized + `,{"id":"generic-question","options":[{"label":"Approve Once"}]}]}`,
		},
		{
			name:   "mixed MCP question without approval option",
			policy: "never",
			params: `{"questions":[` + recognized + `,{"id":"mcp_tool_call_approval_denied","options":[{"label":"Deny"}]}]}`,
		},
		{name: "on request", policy: "on-request", params: `{"questions":[` + recognized + `]}`},
		{name: "on failure", policy: "on-failure", params: `{"questions":[` + recognized + `]}`},
		{name: "untrusted", policy: "untrusted", params: `{"questions":[` + recognized + `]}`},
		{name: "case changed never", policy: "Never", params: `{"questions":[` + recognized + `]}`},
		{
			name:   "granular",
			policy: map[string]any{"granular": map[string]any{"sandbox_approval": true}},
			params: `{"questions":[` + recognized + `]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, stdin := newTurnLoopClient(t, []string{
				`{"id":3,"method":"item/tool/requestUserInput","params":` + tt.params + `}`,
			}, func(c *appServerClient) { c.approvalPolicy = tt.policy })
			err := c.awaitTurnCompletion(context.Background())
			var inputErr *InputRequiredError
			if !errors.As(err, &inputErr) || inputErr.Method != "item/tool/requestUserInput" {
				t.Fatalf("awaitTurnCompletion() = %v; want item/tool/requestUserInput *InputRequiredError", err)
			}
			if got, want := runtimeEventNames(c), []string{task.EventTurnInputRequired}; !slices.Equal(got, want) {
				t.Errorf("runtime events = %v; want %v", got, want)
			}
			if stdin.Len() != 0 {
				t.Errorf("user-input wire output = %q; want no fabricated, partial, or error response", stdin.String())
			}
		})
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
	// streamDeadlineArmedAt every iteration so this guard is defensive; drive the helper
	// directly to pin both the Cause==nil discriminator and the no-read
	// short-circuit (awaitTurnCompletion resets streamDeadlineArmedAt on entry, so the
	// branch is unreachable through the full loop).
	c, _ := newTurnLoopClient(t, []string{`{"method":"turn/completed","params":{}}`},
		func(c *appServerClient) { c.stallTimeoutMs = 10 })
	c.streamDeadlineArmedAt = time.Now().Add(-time.Second) // budget already exhausted
	msg, raw, err := c.readTurnMessage(context.Background())
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
	// The scanner is untouched: the unread turn/completed line is still there,
	// proving readTurnMessage short-circuited before reading.
	if line, lerr := c.readLine(context.Background()); lerr != nil || !strings.Contains(string(line), "turn/completed") {
		t.Errorf("next line after pre-read stall = %q (err %v); want the unconsumed turn/completed line", line, lerr)
	}
}

func TestReadTurnMessage_PreCanceledParentWinsSpentLocalBudget(t *testing.T) {
	c := &appServerClient{
		readCh:                make(chan []byte),
		out:                   io.Discard,
		turnTimeoutMs:         1,
		streamDeadlineArmedAt: time.Now().Add(-time.Second),
		approvalPolicy:        "never",
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	msg, raw, err := c.readTurnMessage(ctx)
	if msg != nil || raw != nil {
		t.Fatalf("readTurnMessage() msg=%v raw=%q; want nil,nil after parent cancellation", msg, raw)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("readTurnMessage() = %T %[1]v; want context.Canceled", err)
	}
	if IsTurnTimeout(err) || IsStall(err) || IsReadTimeout(err) || IsTimeout(err) {
		t.Fatalf("readTurnMessage() = %T %[1]v; want parent cancellation without timeout relabeling", err)
	}
}

func TestHandleTurnNotification_DoesNotReclassifyAlreadyReadFrame(t *testing.T) {
	// readTurnMessage owns the stream deadline. Once it returns a frame, local
	// scheduler/handler delay must not recheck wall time and relabel that
	// already-received output as silence.
	c := &appServerClient{out: io.Discard, stallTimeoutMs: 10}
	c.streamDeadlineArmedAt = time.Now().Add(-time.Second)
	done, err := c.handleTurnNotification(map[string]any{"method": "item/agentMessage"}, "item/agentMessage")
	if done {
		t.Fatalf("handleTurnNotification() done = true; want already-read notification to continue")
	}
	if err != nil {
		t.Fatalf("handleTurnNotification() err = %v; want nil for already-read notification", err)
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

func TestAwaitTurnCompletion_EarliestStreamDeadlineKeepsTypedFailure(t *testing.T) {
	tests := []struct {
		name      string
		turnMs    int
		stallMs   int
		wantTurn  bool
		wantStall bool
	}{
		{name: "turn before stall", turnMs: 20, stallMs: 200, wantTurn: true},
		{name: "stall before turn", turnMs: 200, stallMs: 20, wantStall: true},
		{name: "equal budgets preserve turn precedence", turnMs: 20, stallMs: 20, wantTurn: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &appServerClient{
				readCh:           make(chan []byte),
				out:              io.Discard,
				approvalPolicy:   "never",
				turnTimeoutMs:    tt.turnMs,
				stallTimeoutMs:   tt.stallMs,
				lastRuntimeEvent: task.EventTurnStarted,
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			err := c.awaitTurnCompletion(ctx)
			if got := IsTurnTimeout(err); got != tt.wantTurn {
				t.Fatalf("IsTurnTimeout(%v) = %v; want %v", err, got, tt.wantTurn)
			}
			if got := IsStall(err); got != tt.wantStall {
				t.Fatalf("IsStall(%v) = %v; want %v", err, got, tt.wantStall)
			}
		})
	}
}

func TestAwaitTurnCompletion_ParentCancellationIsNotRelabeled(t *testing.T) {
	c := &appServerClient{
		readCh:         make(chan []byte),
		out:            io.Discard,
		approvalPolicy: "never",
		readTimeoutMs:  30000,
		turnTimeoutMs:  30000,
		stallTimeoutMs: 0,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timer := time.AfterFunc(20*time.Millisecond, func() {
		defer recoverPanic("runner.test_turn_parent_cancel")
		cancel()
	})
	defer timer.Stop()

	start := time.Now()
	err := c.awaitTurnCompletion(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("awaitTurnCompletion() = %T %[1]v; want context.Canceled", err)
	}
	if IsTurnTimeout(err) || IsStall(err) || IsReadTimeout(err) || IsTimeout(err) || isAppServerReadTimeout(err) {
		t.Fatalf("awaitTurnCompletion() = %T %[1]v; want parent cancellation without timeout relabeling", err)
	}
	if elapsed := time.Since(start); elapsed >= 5*time.Second {
		t.Fatalf("parent cancellation returned after %s; want prompt return under 5s", elapsed)
	}
}

func TestResolveTurnReadResult_ParentCancellationWinsReadyLocalResult(t *testing.T) {
	tests := []struct {
		name       string
		msg        map[string]any
		raw        []byte
		readCtxErr error
		readErr    error
	}{
		{name: "valid frame", msg: map[string]any{"method": "turn/completed"}},
		{name: "malformed frame", raw: []byte(`{"bad":`), readErr: errors.New("decode failed")},
		{name: "read timeout", readErr: &appServerReadTimeoutError{afterMs: 10}},
		{name: "turn deadline", readCtxErr: context.DeadlineExceeded, readErr: context.DeadlineExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &appServerClient{streamDeadlineArmedAt: time.Now()}
			msg, raw, err := c.resolveTurnReadResult(
				tt.msg,
				tt.raw,
				context.Canceled,
				tt.readCtxErr,
				tt.readErr,
				turnStreamDeadline{kind: turnStreamTimeoutTurn, timeout: time.Second},
			)
			if msg != nil || raw != nil {
				t.Fatalf("resolveTurnReadResult() msg=%v raw=%q; want nil,nil after parent cancellation", msg, raw)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("resolveTurnReadResult() = %T %[1]v; want context.Canceled", err)
			}
			if IsTurnTimeout(err) || IsStall(err) || IsReadTimeout(err) || IsTimeout(err) || isAppServerReadTimeout(err) {
				t.Fatalf("resolveTurnReadResult() = %T %[1]v; want parent cancellation without timeout relabeling", err)
			}
		})
	}
}

func TestAwaitTurnCompletion_OuterDeadlineIsNotRelabeled(t *testing.T) {
	c := &appServerClient{
		readCh:         make(chan []byte),
		out:            io.Discard,
		approvalPolicy: "never",
		readTimeoutMs:  30000,
		turnTimeoutMs:  30000,
		stallTimeoutMs: 0,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := c.awaitTurnCompletion(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("awaitTurnCompletion() = %T %[1]v; want context.DeadlineExceeded", err)
	}
	if IsTurnTimeout(err) || IsStall(err) || IsReadTimeout(err) || IsTimeout(err) || isAppServerReadTimeout(err) {
		t.Fatalf("awaitTurnCompletion() = %T %[1]v; want outer deadline without local timeout relabeling", err)
	}
}

func TestAwaitTurnCompletion_ReadTimeoutDoesNotPreemptStallBudget(t *testing.T) {
	c := &appServerClient{
		readCh:         make(chan []byte),
		out:            io.Discard,
		approvalPolicy: "never",
		readTimeoutMs:  1,
		stallTimeoutMs: 20,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := c.awaitTurnCompletion(ctx)
	if !IsStall(err) {
		t.Fatalf("awaitTurnCompletion() = %T %[1]v; want stall timeout despite shorter read timeout", err)
	}
	if isAppServerReadTimeout(err) || IsReadTimeout(err) {
		t.Fatalf("awaitTurnCompletion() = %T %[1]v; want stall timeout distinct from read timeout", err)
	}
}

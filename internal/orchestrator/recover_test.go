package orchestrator

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// captureOrchestratorLog redirects the stdlib log writer to a buffer for
// the duration of fn so the recover-panic site assertions can grep the
// emitted line without picking up unrelated output from concurrent
// goroutines.
func captureOrchestratorLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	var mu sync.Mutex
	origOut := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(safeBufferWriter{buf: &buf, mu: &mu})
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(origOut)
		log.SetFlags(origFlags)
	})
	fn()
	mu.Lock()
	defer mu.Unlock()
	return buf.String()
}

type safeBufferWriter struct {
	buf *bytes.Buffer
	mu  *sync.Mutex
}

func (w safeBufferWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func TestSafeGoConfinesPanic(t *testing.T) {
	done := make(chan struct{})
	got := captureOrchestratorLog(t, func() {
		safeGo("test.site", func() {
			defer close(done)
			panic("boom")
		})
		<-done
		// Give the deferred recoverPanic time to flush its log line.
		time.Sleep(10 * time.Millisecond)
	})
	for _, want := range []string{
		"event=panic",
		"site=test.site",
		"panic=boom",
		"stack=",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("safeGo panic log missing %q in:\n%s", want, got)
		}
	}
}

func TestRecoverPanicEmitsStructuredLine(t *testing.T) {
	got := captureOrchestratorLog(t, func() {
		func() {
			defer recoverPanic("test.deferred_site")
			panic("nil-deref")
		}()
	})
	if !strings.Contains(got, "event=panic site=test.deferred_site") {
		t.Errorf("recoverPanic log missing event/site in:\n%s", got)
	}
	if !strings.Contains(got, "panic=nil-deref") {
		t.Errorf("recoverPanic log missing panic= in:\n%s", got)
	}
}

// panickyOp is a stateOp that panics from apply. The actor's
// applyWithRecover guard must catch the panic, log it, and keep
// draining ops instead of crashing the process.
type panickyOp struct {
	panicVal any
}

func (p panickyOp) apply(_ *OrchestratorState) func() { panic(p.panicVal) }

// counterOp increments a counter from apply so the test can verify the
// actor goroutine kept processing after the panicky op.
type counterOp struct {
	counter *atomic.Int64
}

func (c counterOp) apply(_ *OrchestratorState) func() {
	c.counter.Add(1)
	return nil
}

// TestActorRunSurvivesOpApplyPanic is the SPEC §7.4 / closing-#296
// acceptance test: a panic inside `op.apply` must not crash the actor
// goroutine. The test submits a panicky op followed by a counter op
// and asserts (a) the panic is logged with the typed site tag and (b)
// the actor accepted the subsequent op.
func TestActorRunSurvivesOpApplyPanic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	orch := New(NewOrchestratorState(30000, 1), Deps{
		Dispatcher: &cancellationDispatcher{},
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})

	var counter atomic.Int64
	got := captureOrchestratorLog(t, func() {
		go orch.Run(ctx)
		if err := orch.WaitStarted(ctx); err != nil {
			t.Fatalf("wait for orchestrator: %v", err)
		}
		if err := orch.submit(ctx, panickyOp{panicVal: "synthetic-test-panic"}); err != nil {
			t.Fatalf("submit panicky op: %v", err)
		}
		if err := orch.submit(ctx, counterOp{counter: &counter}); err != nil {
			t.Fatalf("submit counter op: %v", err)
		}
		// Wait briefly for the actor to process both ops.
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) && counter.Load() == 0 {
			time.Sleep(5 * time.Millisecond)
		}
	})

	if counter.Load() != 1 {
		t.Fatalf("actor stopped draining ops after panic; counter=%d, log=%s", counter.Load(), got)
	}
	if !strings.Contains(got, "event=panic site=orchestrator.op_apply") {
		t.Errorf("expected structured panic log line, got:\n%s", got)
	}
	if !strings.Contains(got, "panic=synthetic-test-panic") {
		t.Errorf("panic value not in log, got:\n%s", got)
	}
}

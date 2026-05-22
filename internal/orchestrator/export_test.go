package orchestrator

import "context"

// WithStateForTest runs fn against OrchestratorState on the actor goroutine.
// Test-only helper for surgical mutations (e.g. backdating LastCodexAt to
// trigger a stall reconciliation without sleeping for the full budget).
// Production code must go through the typed op surface.
func (o *Orchestrator) WithStateForTest(fn func(*OrchestratorState)) {
	done := make(chan struct{})
	_ = o.submit(context.Background(), opFunc(func(st *OrchestratorState) func() {
		fn(st)
		return func() { close(done) }
	}))
	<-done
}

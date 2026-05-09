package worker

import (
	"context"
	"sync"
)

// recordedEvent holds a captured task event for test assertions.
type recordedEvent struct {
	Kind    string
	Message string
	Payload any
}

// fakeEmitter is an in-memory EventEmitter used by package-internal tests
// (secretscan_test.go, print_config_test.go) that live in package worker.
// The external test file (run_test.go, package worker_test) defines its own
// copy because Go does not share _test.go types across package boundaries.
type fakeEmitter struct {
	mu     sync.Mutex
	events []recordedEvent
	err    error
}

func (f *fakeEmitter) AddEvent(_ context.Context, _ string, kind, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, recordedEvent{Kind: kind, Message: msg})
	return f.err
}

func (f *fakeEmitter) AddEventWithPayload(_ context.Context, _ string, kind, msg string, payload any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, recordedEvent{Kind: kind, Message: msg, Payload: payload})
	return f.err
}

// byKind returns the recorded events whose Kind matches.
func (f *fakeEmitter) byKind(kind string) []recordedEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recordedEvent
	for _, e := range f.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

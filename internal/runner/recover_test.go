package runner

import (
	"bytes"
	"log"
	"strings"
	"sync"
	"testing"
)

type syncLogBuffer struct {
	buf *bytes.Buffer
	mu  *sync.Mutex
}

func (w syncLogBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// TestRecoverPanicConfinesAndLogs asserts the package-local recoverPanic both
// swallows a panic (the calling goroutine survives instead of crashing the
// process) and emits the orchestrator-shaped structured line. If the
// recover() inside recoverPanic is removed, the panic escapes this deferred
// guard and crashes the test binary, so the test bites on the recover itself.
func TestRecoverPanicConfinesAndLogs(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	origOut, origFlags := log.Writer(), log.Flags()
	log.SetOutput(syncLogBuffer{buf: &buf, mu: &mu})
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(origOut); log.SetFlags(origFlags) })

	func() {
		defer recoverPanic("runner.test_site")
		panic("boom")
	}()

	mu.Lock()
	got := buf.String()
	mu.Unlock()
	for _, want := range []string{"event=panic", "site=runner.test_site", "panic=boom", "stack="} {
		if !strings.Contains(got, want) {
			t.Errorf("recoverPanic log missing %q in:\n%s", want, got)
		}
	}
}

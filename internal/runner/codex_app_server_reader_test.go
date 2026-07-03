package runner

// codex_app_server_reader_test.go pins the single long-lived stdout reader's
// lifecycle (#666): the per-read timeout, context cancellation, and stream-EOF
// exit paths a request/response consumer relies on, plus a deterministic join
// proving the reader goroutine does not outlive shutdown. These exercise the
// reader directly over an io.Pipe so a test controls exactly when a line
// arrives, the stream EOFs, or the read blocks — the conditions a real
// `codex app-server` produces but a canned-buffer scanner cannot.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// newPipeReaderClient builds a client whose stdout is an in-memory pipe and
// starts its single long-lived reader. It returns the write end so a test can
// feed lines, close the stream, or leave it silent. The reader is joined at
// test end: close the pipe so a Scan-parked reader EOFs, then drain readCh to
// its deferred close (which also receives any line a reader is parked handing
// off, after which it loops back to Scan and observes the EOF).
func newPipeReaderClient(t *testing.T, opts ...func(*appServerClient)) (*appServerClient, *io.PipeWriter) {
	t.Helper()
	pr, pw := io.Pipe()
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 0, appServerScannerInitialBuf), maxAppServerLineBytes+1)
	c := &appServerClient{scanner: sc, out: io.Discard}
	for _, opt := range opts {
		opt(c)
	}
	c.startStdoutReader()
	t.Cleanup(func() {
		_ = pw.Close()
		for range c.readCh { //nolint:revive // drain-to-close joins the reader
		}
	})
	return c, pw
}

// TestStdoutReader_ReadTimeoutWhileReaderBlocked pins that a per-read timeout
// fires while the reader is parked in scanner.Scan with no line available, and
// that the reader survives (its lifetime is the process, not one read).
func TestStdoutReader_ReadTimeoutWhileReaderBlocked(t *testing.T) {
	c, _ := newPipeReaderClient(t, func(c *appServerClient) { c.readTimeoutMs = 50 })
	start := time.Now()
	_, err := c.readLine(context.Background())
	if !isAppServerReadTimeout(err) {
		t.Fatalf("readLine() err = %v; want an app-server read timeout while the reader is blocked", err)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Errorf("readLine() returned after %v; want it to wait for the ~50ms read timeout", elapsed)
	}
}

// TestStdoutReader_ContextCancelWhileReaderBlocked pins that cancelling the
// caller's context unblocks readLine even though the stdout reader is parked in
// scanner.Scan on a silent stream — the failure class #666 fixes (a per-read
// goroutine that stayed parked after its caller moved on).
func TestStdoutReader_ContextCancelWhileReaderBlocked(t *testing.T) {
	c, _ := newPipeReaderClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := c.readLine(ctx)
		errCh <- err
	}()
	// Let readLine reach its select with the reader parked on the empty pipe.
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("readLine() err = %v; want context.Canceled after cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readLine() did not return after context cancel (reader-blocked leak)")
	}
}

// TestStdoutReader_ReturnsEOFWhenStreamCloses pins that once the stream closes
// (the app-server exited / stdout closed), the reader's sticky terminal error
// is io.EOF and readLine surfaces it via the closed readCh.
func TestStdoutReader_ReturnsEOFWhenStreamCloses(t *testing.T) {
	c, pw := newPipeReaderClient(t)
	_ = pw.Close()
	if _, err := c.readLine(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("readLine() err = %v; want io.EOF after the stream closes", err)
	}
}

// TestStdoutReader_DeliversLineThenEOF pins the happy path: a line is delivered
// to the consumer, and the next read observes EOF once the stream closes.
func TestStdoutReader_DeliversLineThenEOF(t *testing.T) {
	c, pw := newPipeReaderClient(t)
	go func() {
		_, _ = pw.Write([]byte("{\"jsonrpc\":\"2.0\"}\n"))
		_ = pw.Close()
	}()
	line, err := c.readLine(context.Background())
	if err != nil {
		t.Fatalf("readLine() err = %v; want the delivered line", err)
	}
	if got, want := string(line), `{"jsonrpc":"2.0"}`; got != want {
		t.Fatalf("readLine() = %q; want %q", got, want)
	}
	if _, err := c.readLine(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("second readLine() err = %v; want io.EOF after stream close", err)
	}
}

// TestStdoutReader_HoldsLineAcrossTimeoutThenDelivers pins the production timing
// window the per-read timeout exists for: a line that arrives just after a read
// times out is held by the reader (parked on the readCh send) and delivered
// intact to the NEXT read — no loss, no reorder. The old per-read goroutine
// orphaned such a line; the single reader preserves it.
func TestStdoutReader_HoldsLineAcrossTimeoutThenDelivers(t *testing.T) {
	c, pw := newPipeReaderClient(t, func(c *appServerClient) { c.readTimeoutMs = 40 })
	if _, err := c.readLine(context.Background()); !isAppServerReadTimeout(err) {
		t.Fatalf("first readLine() err = %v; want a read timeout on the silent stream", err)
	}
	go func() { _, _ = pw.Write([]byte("late-line\n")) }()
	line, err := c.readLine(context.Background())
	if err != nil {
		t.Fatalf("second readLine() err = %v; want the late line", err)
	}
	if got, want := string(line), "late-line"; got != want {
		t.Fatalf("second readLine() = %q; want %q (a line held across the first read's timeout must be delivered intact)", got, want)
	}
}

// TestStdoutReader_DrainsFloodWhileConsumerBusy pins the #1033 deadlock
// scenario: codex keeps streaming stdout while the consumer is parked in a
// synchronous dynamic-tool call and reads nothing. Every producer write must
// still complete (the pump queues in memory, keeping the pipe drained); with
// a direct scan→consumer handoff the scan goroutine parks on the second line
// and the producer stalls — exactly the OS-pipe backpressure that deadlocks
// the real subprocess. Order must survive the queueing.
func TestStdoutReader_DrainsFloodWhileConsumerBusy(t *testing.T) {
	c, pw := newPipeReaderClient(t)
	const n = 200
	wrote := make(chan error, 1)
	go func() {
		for i := 0; i < n; i++ {
			if _, err := fmt.Fprintf(pw, "line-%d\n", i); err != nil {
				wrote <- err
				return
			}
		}
		wrote <- nil
	}()
	// The consumer stays busy: nothing reads readCh until the flood is fully
	// written.
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatalf("producer write err = %v; want all %d lines written with no consumer", err, n)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("producer blocked with the consumer busy — stdout is not being drained (#1033)")
	}
	for i := 0; i < n; i++ {
		line, err := c.readLine(context.Background())
		if err != nil {
			t.Fatalf("readLine(%d) err = %v; want buffered line", i, err)
		}
		if got, want := string(line), fmt.Sprintf("line-%d", i); got != want {
			t.Fatalf("readLine(%d) = %q; want %q (queueing must preserve order)", i, got, want)
		}
	}
}

// TestStdoutReader_BacklogOverflowFailsTypedNotOOM pins the pump's memory
// bound: when the scanned-but-unconsumed backlog exceeds the cap, the
// consumer observes a typed backlog error through the closed readCh instead
// of the worker growing without bound, and the producer still completes (the
// pump keeps discarding to EOF so neither the scan goroutine nor the
// subprocess blocks).
func TestStdoutReader_BacklogOverflowFailsTypedNotOOM(t *testing.T) {
	c, pw := newPipeReaderClient(t, func(c *appServerClient) { c.stdoutBufferBytes = 256 })
	wrote := make(chan error, 1)
	go func() {
		for i := 0; i < 100; i++ {
			if _, err := fmt.Fprintf(pw, "%064d\n", i); err != nil {
				wrote <- err
				return
			}
		}
		wrote <- nil
	}()
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatalf("producer write err = %v; want the overflow path to keep draining the pipe", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("producer blocked after backlog overflow — pump must discard to EOF")
	}
	_, err := c.readLine(context.Background())
	if !isStdoutBacklogOverflow(err) {
		t.Fatalf("readLine() err = %v; want the typed stdout-backlog overflow error", err)
	}
	if cat, ok := ErrorCategory(err); !ok || cat != CategoryResponseError {
		t.Fatalf("ErrorCategory(%v) = %q, %v; want %q, true", err, cat, ok, CategoryResponseError)
	}
}

// TestStdoutReader_BacklogOverflowCountsEmptyLines pins the second half of the
// backlog cap: each queued message must carry a per-item charge, not just
// len(line), so blank-line floods cannot grow q.items forever while q.bytes
// stays at zero.
func TestStdoutReader_BacklogOverflowCountsEmptyLines(t *testing.T) {
	c, pw := newPipeReaderClient(t, func(c *appServerClient) { c.stdoutBufferBytes = 1 })
	wrote := make(chan error, 1)
	go func() {
		for i := 0; i < 100; i++ {
			if _, err := fmt.Fprintln(pw); err != nil {
				wrote <- err
				return
			}
		}
		if err := pw.Close(); err != nil {
			wrote <- err
			return
		}
		wrote <- nil
	}()
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatalf("producer write err = %v; want empty-line overflow path to keep draining the pipe", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("producer blocked after empty-line backlog overflow — pump must count items and keep draining")
	}
	_, err := c.readLine(context.Background())
	if !isStdoutBacklogOverflow(err) {
		t.Fatalf("readLine() err = %v; want stdout-backlog overflow for queued empty lines", err)
	}
}

// TestSend_StdinWriteTimeoutFailsInsteadOfHanging pins the write half of the
// #1033 stdio deadlock over a real OS pipe (the production stdin is an
// *os.File from cmd.StdinPipe): once the pipe's kernel buffer is full and the
// reader side stops draining — codex blocked mid-write on its own stdout —
// send must fail with os.ErrDeadlineExceeded after the write timeout instead
// of parking the consumer goroutine until the run's outer deadline.
func TestSend_StdinWriteTimeoutFailsInsteadOfHanging(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() err = %v", err)
	}
	defer func() { _ = pr.Close() }()
	defer func() { _ = pw.Close() }()

	// Fill the kernel buffer with bounded writes until one times out; nobody
	// reads pr, modeling a codex that stopped draining stdin.
	junk := bytes.Repeat([]byte("x"), 32<<10)
	for {
		if err := pw.SetWriteDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
			t.Skipf("pipe write deadlines unsupported on this platform: %v", err)
		}
		if _, err := pw.Write(junk); err != nil {
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("filling pipe: write err = %v; want os.ErrDeadlineExceeded", err)
			}
			break
		}
	}

	// Clear the fill loop's leftover deadline: send must arm its own, or a
	// stale expired deadline would mask a send() that sets none.
	if err := pw.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatalf("clear write deadline: %v", err)
	}

	c := &appServerClient{stdin: pw, out: io.Discard, stdinWriteTimeout: 100 * time.Millisecond}
	sent := make(chan error, 1)
	go func() { sent <- c.send(map[string]any{"jsonrpc": "2.0", "method": "turn/start"}) }()
	select {
	case err := <-sent:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("send() on a full undrained stdin pipe err = %v; want os.ErrDeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("send() still blocked after 5s — the stdin write timeout must bound it (#1033)")
	}
}

// panickingReader is an io.Reader whose Read panics, to drive the stdout
// reader's panic-recovery path.
type panickingReader struct{}

func (panickingReader) Read([]byte) (int, error) { panic("scanner boom") }

// TestStdoutReader_PanicInScanSurfacesErrorNotHang pins that a panic in the scan
// loop is recovered and surfaced as readErr through the closed readCh, so the
// consumer gets an error instead of hanging on a never-closed channel — the
// failure class #666 set out to eliminate. It also guards the subtle LIFO-defer
// ordering (recover publishes readErr before close(readCh)).
func TestStdoutReader_PanicInScanSurfacesErrorNotHang(t *testing.T) {
	c := &appServerClient{scanner: bufio.NewScanner(panickingReader{}), out: io.Discard}
	c.startStdoutReader()
	done := make(chan error, 1)
	go func() {
		_, err := c.readLine(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "panic") {
			t.Fatalf("readLine() err = %v; want a surfaced reader-panic error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readLine() hung after a reader panic — recovery must close readCh")
	}
}

// TestStdoutReader_ExitsOnShutdownNoLeak is the lifecycle/leak assertion: after a
// read times out with the reader parked in Scan, the production shutdown (the
// child closing stdout, modeled by closing the pipe, then draining readCh) must
// let the reader goroutine exit. If it leaked, the drain never observes the
// deferred close(readCh) and the 2s guard fires.
func TestStdoutReader_ExitsOnShutdownNoLeak(t *testing.T) {
	pr, pw := io.Pipe()
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 0, appServerScannerInitialBuf), maxAppServerLineBytes+1)
	c := &appServerClient{scanner: sc, out: io.Discard, readTimeoutMs: 20}
	c.startStdoutReader()

	if _, err := c.readLine(context.Background()); !isAppServerReadTimeout(err) {
		t.Fatalf("readLine() err = %v; want a read timeout while the reader is parked", err)
	}

	// Mirror RunCodexAppServer's shutdown: the child closing stdout EOFs the
	// Scan-parked reader, and draining readCh to its deferred close joins it.
	_ = pw.Close()
	joined := make(chan struct{})
	go func() {
		for range c.readCh { //nolint:revive // drain-to-close joins the reader
		}
		close(joined)
	}()
	select {
	case <-joined:
	case <-time.After(2 * time.Second):
		t.Fatal("stdout reader did not exit within 2s of shutdown — goroutine leak")
	}
}

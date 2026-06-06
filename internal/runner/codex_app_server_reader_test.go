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
	"context"
	"errors"
	"io"
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

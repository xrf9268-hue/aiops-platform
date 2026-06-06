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
	"testing"
	"time"
)

// newPipeReaderClient builds a client whose stdout is an in-memory pipe and
// starts its single long-lived reader. It returns the write end so a test can
// feed lines, close the stream, or leave it silent. The reader is joined at
// test end (close the pipe so a Scan-parked reader EOFs, signal readDone for a
// handoff-parked one, then drain readCh to its deferred close).
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
		close(c.readDone)
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

// TestStdoutReader_ExitsOnShutdownNoLeak is the lifecycle/leak assertion: after
// a read times out with the reader parked in Scan, the production shutdown
// sequence (close the stream, signal readDone, drain readCh) must let the reader
// goroutine exit. If it leaked, the drain never observes the deferred
// close(readCh) and the 2s guard fires.
func TestStdoutReader_ExitsOnShutdownNoLeak(t *testing.T) {
	pr, pw := io.Pipe()
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 0, appServerScannerInitialBuf), maxAppServerLineBytes+1)
	c := &appServerClient{scanner: sc, out: io.Discard, readTimeoutMs: 20}
	c.startStdoutReader()

	if _, err := c.readLine(context.Background()); !isAppServerReadTimeout(err) {
		t.Fatalf("readLine() err = %v; want a read timeout while the reader is parked", err)
	}

	// Mirror RunCodexAppServer's shutdown: the closed stream EOFs a Scan-parked
	// reader, close(readDone) releases a handoff-parked one, draining joins it.
	_ = pw.Close()
	joined := make(chan struct{})
	go func() {
		close(c.readDone)
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

package runner

// codex_app_server_transport.go is the JSON-RPC 2.0 stdio transport for the
// `codex app-server` session: framing requests/notifications onto stdin, the
// single long-lived stdout reader and its panic/terminal-error lifecycle, and
// the per-read timeout machinery. The protocol client that drives sessions and
// turns over this transport lives in codex_app_server.go.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"runtime/debug"
	"strings"
	"time"
)

const (
	// appServerScannerInitialBuf is the Scanner's starting buffer size; it grows
	// as needed up to maxAppServerLineBytes.
	appServerScannerInitialBuf = 64 << 10
	// maxAppServerLineBytes caps each Codex app-server stdio line per SPEC §10.1
	// ("Max line size: 10 MB"). Lines exceeding this surface as bufio.ErrTooLong
	// instead of growing the buffer unbounded and OOMing the worker.
	maxAppServerLineBytes = 10 * 1024 * 1024
)

func (c *appServerClient) request(ctx context.Context, method string, params any) (map[string]any, error) {
	id := c.nextID
	c.nextID++
	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for {
		msg, err := c.readMessage(ctx)
		if err != nil {
			return nil, err
		}
		if result, matched, rpcErr := responseForID(msg, id); matched {
			return result, rpcErr
		}
		c.handleNotification(msg)
	}
}
func (c *appServerClient) notify(method string, params map[string]any) error {
	return c.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}
func (c *appServerClient) send(msg map[string]any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = c.stdin.Write(b)
	return err
}
func (c *appServerClient) readMessage(ctx context.Context) (map[string]any, error) {
	msg, raw, err := c.readProtocolMessage(ctx)
	if err != nil {
		if raw != nil {
			return nil, fmt.Errorf("decode codex app-server message: %w", err)
		}
		return nil, err
	}
	return msg, nil
}
func (c *appServerClient) readProtocolMessage(ctx context.Context) (map[string]any, []byte, error) {
	select {
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	default:
	}
	line, err := c.readLine(ctx)
	if err != nil {
		return nil, nil, err
	}
	// Scanner strips the line terminator; restore one in the transcript so
	// successive JSON-RPC messages remain visually separated.
	_, _ = c.out.Write(line)
	_, _ = c.out.Write([]byte{'\n'})
	var msg map[string]any
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil, line, NewError(CategoryResponseError, "decode codex app-server message", err)
	}
	return msg, line, nil
}

// startStdoutReader launches the single long-lived stdout reader. It is the
// source stage of https://go.dev/blog/pipelines: one goroutine owns the
// bufio.Scanner (which is not safe for concurrent use) and continuously scans
// lines, handing each to the request/response consumer over readCh. Its lifetime
// is the app-server process: it exits when scanner.Scan returns false (stdout
// EOF or error — the process closed stdout on exit), recording a sticky readErr.
//
// The reader has no separate stop signal: the consumer never abandons readCh, it
// drains to EOF at shutdown (RunCodexAppServer drains before cmd.Wait), so the
// reader always keeps the pipe flowing and reaches EOF rather than parking on a
// send forever (Go Code Review Comments, "Goroutine Lifetimes"). `defer
// close(readCh)` runs on every exit path, so a consumer waiting on readCh is
// always released and the close is the happens-before edge that publishes
// readErr. readCh is created here so the goroutine and its channel have one owner.
func (c *appServerClient) startStdoutReader() {
	c.readCh = make(chan []byte)
	go func() {
		defer close(c.readCh)
		defer c.recoverReaderPanic()
		for c.scanner.Scan() {
			// Scanner.Bytes() is invalidated by the next Scan (bufio docs); copy
			// before handing the line across the goroutine boundary.
			line := append([]byte(nil), c.scanner.Bytes()...)
			c.readCh <- line
		}
		c.readErr = scanTerminalError(c.scanner)
	}()
}

// recoverReaderPanic is the stdout reader's deferred recovery: a panic in the
// scan loop is published as readErr (so the consumer sees an error rather than
// hanging on a never-closed readCh) and logged with a stack, not swallowed —
// matching the orchestrator's recoverPanicValue convention (recover.go).
func (c *appServerClient) recoverReaderPanic() {
	r := recover()
	if r == nil {
		return
	}
	if err, ok := r.(error); ok {
		c.readErr = fmt.Errorf("codex app-server stdout reader panic: %w", err)
	} else {
		c.readErr = fmt.Errorf("codex app-server stdout reader panic: %v", r)
	}
	log.Printf("event=codex_app_server_reader_panic panic=%v stack=%q", r, debug.Stack())
}

// scanTerminalError maps a finished scanner to the terminal error the consumer
// should observe once readCh closes. Scanner.Err is nil on io.EOF (bufio docs),
// so a clean end-of-stream normalizes to io.EOF; an over-cap line keeps the
// existing wrapped bufio.ErrTooLong contract.
func scanTerminalError(sc *bufio.Scanner) error {
	err := sc.Err()
	if err == nil {
		return io.EOF
	}
	if errors.Is(err, bufio.ErrTooLong) {
		return fmt.Errorf("codex app-server line exceeded %d bytes: %w", maxAppServerLineBytes, err)
	}
	return err
}

func (c *appServerClient) readLine(ctx context.Context) ([]byte, error) {
	readTimeout := time.Duration(c.readTimeoutMs) * time.Millisecond
	deadlineTimeout, hasDeadline := deadlineDuration(ctx)
	if c.readTimeoutMs <= 0 || (hasDeadline && deadlineTimeout < readTimeout) {
		return c.readLineOnce(ctx, nil)
	}
	return c.readLineOnce(ctx, time.After(readTimeout))
}

// readLineOnce waits for the next line from the long-lived reader, the
// per-read timeout, or context cancellation — whichever fires first. A closed
// readCh means the reader has exited; it surfaces the reader's sticky readErr
// (io.EOF when it stopped cleanly). The DeadlineExceeded preference is retained
// so a read that loses the race to an expiring context is classified as the
// deadline, not as the EOF the dying process subsequently produced.
func (c *appServerClient) readLineOnce(ctx context.Context, timeout <-chan time.Time) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timeout:
		return nil, &appServerReadTimeoutError{afterMs: c.readTimeoutMs}
	case line, ok := <-c.readCh:
		if !ok {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, ctx.Err()
			}
			if c.readErr != nil {
				return nil, c.readErr
			}
			return nil, io.EOF
		}
		return line, nil
	}
}
func deadlineDuration(ctx context.Context) (time.Duration, bool) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0, false
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0, true
	}
	return remaining, true
}

// appServerReadTimeoutError signals that a single stdio read exceeded the
// configured codex read_timeout_ms budget. It is a typed error so the
// classification paths detect it via errors.As rather than substring-matching
// the message (AGENTS.md clean-code rule 8). The message is unchanged from the
// original fmt.Errorf so existing logs and string-asserting tests still hold.
type appServerReadTimeoutError struct {
	afterMs int
}

func (e *appServerReadTimeoutError) Error() string {
	return fmt.Sprintf("codex app-server read timeout after %dms", e.afterMs)
}

// isAppServerReadTimeout reports whether err is (or wraps) the read-timeout
// signal. Both classifyAppServerOutcome and classifyTurnReadError consume it, so
// the typed check lives here once rather than being inlined at each call site.
func isAppServerReadTimeout(err error) bool {
	var rt *appServerReadTimeoutError
	return errors.As(err, &rt)
}
func protocolMessageCandidate(raw []byte) bool {
	return strings.HasPrefix(strings.TrimLeft(string(raw), " \t\r\n"), "{")
}
func trimProtocolLine(raw []byte) string {
	return strings.TrimRight(string(raw), "\r\n")
}

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
	// maxAppServerBufferedBytes caps how many bytes of scanned-but-unconsumed
	// stdout lines the reader pump may hold while the consumer is busy (e.g. a
	// synchronous dynamic-tool HTTP call, bounded by its own 30s timeout). The
	// Elixir reference gets this buffering for free — Port output queues into
	// the process mailbox — and the pump is the explicit Go compensation
	// (AGENTS.md cross-cutting checklist item 2); the cap keeps the
	// compensation memory-bounded and turns a pathological flood into a typed
	// error instead of an OOM or a stdio deadlock.
	maxAppServerBufferedBytes = 64 << 20
	// lineQueueItemOverheadBytes is a conservative charge for each queued stdout
	// item in addition to payload bytes. The cap is a safety budget, not exact
	// heap accounting; charging per item keeps blank/tiny-line floods bounded.
	lineQueueItemOverheadBytes = 64
	// appServerStdinWriteTimeout bounds a single write to codex stdin. The read
	// path has read_timeout_ms; without a write bound the symmetric half of a
	// stdio deadlock (codex blocked writing stdout, harness blocked writing
	// stdin) hangs until the run's outer deadline instead of failing fast.
	appServerStdinWriteTimeout = 30 * time.Second
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
		if done, reqErr := c.dispatchInterleavedMessage(ctx, msg); done {
			return nil, reqErr
		}
	}
}

// dispatchInterleavedMessage handles a message read while an RPC awaits its
// response. Codex may interleave a server->client request (approval prompt,
// dynamic tool call, user input, elicitation — carries both id and method);
// it must be answered through the same machinery the turn loop uses, because
// treating it as a notification leaves codex blocked on the reply and the
// request loop blocked on the response — a mutual deadlock surfacing as a
// misleading read timeout/stall (#1031). item/tool/call routes through the
// dynamic-tool handler (not the approval table, which would reply "Method
// not found" and reject an advertised linear_graphql/gitea_issue_labels call
// solely for arriving early); everything else follows handleServerRequest.
// True notifications keep flowing to handleNotification. done=true aborts
// the pending RPC with reqErr (an InputRequiredError or a wire-write
// failure).
func (c *appServerClient) dispatchInterleavedMessage(ctx context.Context, msg map[string]any) (done bool, reqErr error) {
	method, ok := serverRequestMethod(msg)
	if !ok {
		c.handleNotification(msg)
		return false, nil
	}
	if method == "item/tool/call" {
		if err := c.handleDynamicToolCall(ctx, msg); err != nil {
			return true, err
		}
		return false, nil
	}
	return c.handleServerRequest(msg, method)
}

// serverRequestMethod reports whether msg is a server->client JSON-RPC request
// (id and method both present) and returns its method name.
func serverRequestMethod(msg map[string]any) (string, bool) {
	if _, hasID := msg["id"]; !hasID {
		return "", false
	}
	method, _ := msg["method"].(string)
	if method == "" {
		return "", false
	}
	return method, true
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
	// Bound the write when stdin supports deadlines (the real subprocess pipe
	// is an *os.File; test buffers are not and skip this). If codex stops
	// draining its stdin while we still owe it a reply, the write fails with
	// os.ErrDeadlineExceeded instead of hanging the consumer goroutine — the
	// write-side twin of read_timeout_ms.
	timeout := c.stdinWriteTimeout
	if timeout <= 0 {
		timeout = appServerStdinWriteTimeout
	}
	if d, ok := c.stdin.(interface{ SetWriteDeadline(time.Time) error }); ok {
		if derr := d.SetWriteDeadline(time.Now().Add(timeout)); derr == nil {
			defer func() { _ = d.SetWriteDeadline(time.Time{}) }()
		}
	}
	if _, err = c.stdin.Write(b); err != nil {
		return fmt.Errorf("write codex app-server stdin: %w", err)
	}
	return nil
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

// startStdoutReader launches the long-lived stdout reader as a two-stage
// pipeline (https://go.dev/blog/pipelines): a scan goroutine that owns the
// bufio.Scanner (not safe for concurrent use) and keeps the OS pipe drained,
// and a pump goroutine that queues scanned lines in memory and feeds the
// request/response consumer over readCh.
//
// The pump exists because the consumer can be busy for tens of seconds — a
// dynamic-tool HTTP call runs synchronously on the consumer goroutine — while
// codex keeps streaming stdout without waiting for the tool reply. With a
// direct unbuffered handoff the scan goroutine parks on the send, nobody
// drains the OS pipe (~64 KiB), codex blocks writing stdout, and the tool
// reply written to codex stdin can then deadlock against codex's blocked
// write (#1033). The Elixir reference never sees this because Port output
// queues into the process mailbox; the pump is the explicit Go compensation
// (AGENTS.md cross-cutting checklist item 2), memory-bounded by
// maxAppServerBufferedBytes so a pathological flood fails typed instead of
// OOMing.
//
// Lifetimes: the scan goroutine exits at stdout EOF/error (the process closed
// stdout), publishing its terminal error through scanErr (buffered 1) before
// closing lines. The pump exits after lines closes and the queue drains — the
// consumer never abandons readCh, it drains to EOF at shutdown
// (RunCodexAppServer drains before cmd.Wait) — or on queue overflow, where it
// keeps discarding scanned lines to EOF so neither the scan goroutine nor
// codex ever blocks. Only the pump writes c.readErr, and `defer close(readCh)`
// on its every exit path is the happens-before edge that publishes it.
func (c *appServerClient) startStdoutReader() {
	c.readCh = make(chan []byte)
	lines := make(chan []byte)
	scanErr := make(chan error, 1)
	go func() {
		defer close(lines)
		defer c.recoverReaderPanic(scanErr)
		for c.scanner.Scan() {
			// Scanner.Bytes() is invalidated by the next Scan (bufio docs); copy
			// before handing the line across the goroutine boundary.
			line := append([]byte(nil), c.scanner.Bytes()...)
			lines <- line
		}
		scanErr <- scanTerminalError(c.scanner)
	}()
	go c.pumpStdoutLines(lines, scanErr)
}

// pumpStdoutLines is the buffering stage between the scan goroutine and the
// consumer: it queues lines while the consumer is busy and forwards them in
// order. On overflow it records the typed error and switches to discarding
// the rest of the stream so the scan goroutine still reaches EOF (a parked
// scan goroutine would leak and re-create the very pipe stall this stage
// removes).
func (c *appServerClient) pumpStdoutLines(lines <-chan []byte, scanErr <-chan error) {
	defer close(c.readCh)
	defer recoverPanicValue("runner.app_server_stdout_pump")
	q := lineQueue{capBytes: c.stdoutBufferBytes}
	if q.capBytes <= 0 {
		q.capBytes = maxAppServerBufferedBytes
	}
	for lines != nil || q.len() > 0 {
		out, head := q.sendArm(c.readCh)
		select {
		case line, ok := <-lines:
			if !ok {
				lines = nil
			} else if !q.push(line) {
				c.readErr = &stdoutBacklogOverflowError{capBytes: q.capBytes}
				// Fail the consumer immediately (deferred close(readCh) runs on
				// return) while a detached drain keeps the scan goroutine and the
				// subprocess unblocked until stdout EOFs — the failing run tears
				// the process down, so the drain's lifetime is bounded by it.
				go drainLines(lines)
				return
			}
		case out <- head:
			q.pop()
		}
	}
	c.readErr = <-scanErr
}

// lineQueue is the pump's in-order, byte-bounded backlog of scanned lines.
type lineQueue struct {
	capBytes int
	items    [][]byte
	// bytes tracks the cap charge, including per-item overhead, rather than
	// only len(line); otherwise blank-line floods never hit the limit.
	bytes int
}

func (q *lineQueue) len() int { return len(q.items) }

// push appends line and reports whether the backlog is still within capBytes.
func (q *lineQueue) push(line []byte) bool {
	q.items = append(q.items, line)
	q.bytes += queuedLineBytes(line)
	return q.bytes <= q.capBytes
}

func (q *lineQueue) pop() {
	q.bytes -= queuedLineBytes(q.items[0])
	q.items[0] = nil
	q.items = q.items[1:]
}

func queuedLineBytes(line []byte) int {
	return len(line) + lineQueueItemOverheadBytes
}

// sendArm returns the select arm for forwarding the head line: a nil channel
// (never ready) when the queue is empty, so the select blocks on receive only.
func (q *lineQueue) sendArm(readCh chan []byte) (chan<- []byte, []byte) {
	if len(q.items) == 0 {
		return nil, nil
	}
	return readCh, q.items[0]
}

// drainLines consumes a line channel to close, discarding content, so the
// producing goroutine can always finish its sends and exit.
func drainLines(lines <-chan []byte) {
	defer recoverPanicValue("runner.app_server_stdout_overflow_drain")
	for range lines {
	}
}

type stdoutBacklogOverflowError struct {
	capBytes int
}

func (e *stdoutBacklogOverflowError) Error() string {
	return fmt.Sprintf("codex app-server stdout backlog exceeded %d bytes with the consumer busy", e.capBytes)
}

func (e *stdoutBacklogOverflowError) category() RunnerErrorCategory {
	return CategoryResponseError
}

func isStdoutBacklogOverflow(err error) bool {
	var overflow *stdoutBacklogOverflowError
	return errors.As(err, &overflow)
}

// recoverPanicValue logs a recovered panic with a stack instead of letting it
// kill the worker — the runner-local twin of the orchestrator's recoverPanic
// convention (internal/orchestrator/recover.go).
func recoverPanicValue(site string) {
	if r := recover(); r != nil {
		log.Printf("event=runner_goroutine_panic site=%s panic=%v stack=%q", site, r, debug.Stack())
	}
}

// recoverReaderPanic is the scan goroutine's deferred recovery: a panic in the
// scan loop is published through scanErr (so the pump forwards it as readErr
// and the consumer sees an error rather than hanging) and logged with a
// stack, not swallowed — matching the orchestrator's recoverPanic convention
// (recover.go).
func (c *appServerClient) recoverReaderPanic(scanErr chan<- error) {
	r := recover()
	if r == nil {
		return
	}
	if err, ok := r.(error); ok {
		scanErr <- fmt.Errorf("codex app-server stdout reader panic: %w", err)
	} else {
		scanErr <- fmt.Errorf("codex app-server stdout reader panic: %v", r)
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

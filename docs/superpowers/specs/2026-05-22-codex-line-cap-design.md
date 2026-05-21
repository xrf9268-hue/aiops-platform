# Codex app-server stdio max-line cap — design

**Date:** 2026-05-22
**Issue:** [#226](https://github.com/xrf9268-hue/aiops-platform/issues/226)

## Problem

`internal/runner/codex_app_server.go:372` reads each JSON-RPC framed line via `c.reader.ReadBytes('\n')`. `bufio.Reader.ReadBytes` has no built-in line cap and grows its internal buffer until either a newline arrives or memory is exhausted. A misbehaving or malicious Codex app-server that emits a 4 GB line without a newline OOM-kills the worker (and any concurrent agents sharing the host).

SPEC §10.1 RECOMMENDS "Max line size: 10 MB (for safe buffering)". Current code enforces no cap.

## Decision

Replace the `*bufio.Reader` field with a `*bufio.Scanner` configured via `Buffer(make([]byte, 0, 64<<10), maxAppServerLineBytes)` where `maxAppServerLineBytes = 10 * 1024 * 1024`. `bufio.Scanner.Scan` enforces the cap during the read — when a line exceeds the buffer, `Scan` returns `false` and `Err` returns `bufio.ErrTooLong`. Memory stays bounded at the cap.

### Why Scanner and not a post-read length check on ReadBytes

A post-read length check would still let `ReadBytes` allocate the entire (potentially gigabyte) line into the internal buffer before returning. The check rejects the result but the memory damage is already done. Scanner enforces the cap during the read itself.

### Why not `io.LimitReader`-wrap stdout

`io.LimitReader` exhausts after the cap. The Codex app-server session is long-running and emits many lines; a per-stream limit would break the second message. A per-line cap requires Scanner.

### Why 10 MiB exactly (not configurable yet)

SPEC says 10 MB. We adopt the recommendation as a constant. If operators report legitimate Codex responses above this (e.g. very long tool result payloads), exposing `codex.max_line_bytes` becomes a follow-up. The error path explicitly surfaces the cap value so the failure mode is diagnosable.

## What changes

| File | Change |
| --- | --- |
| `internal/runner/codex_app_server.go` | Replace `reader *bufio.Reader` field with `scanner *bufio.Scanner`. Construct via `bufio.NewScanner(stdout)` + `Buffer(make([]byte, 0, 64<<10), maxAppServerLineBytes)` at the same call site (currently line 84). |
| `internal/runner/codex_app_server.go` (`readLine`) | Replace the `ReadBytes('\n')` goroutine with one that calls `scanner.Scan()`; on success, copy `scanner.Bytes()` (the slice is invalidated by the next `Scan`); on failure, surface `scanner.Err()` (or `io.EOF` if nil). |
| `internal/runner/codex_app_server.go` (`readProtocolMessage`) | After `c.out.Write(line)`, also write `\n` to preserve transcript readability. Scanner strips the terminator from `Bytes()`. |
| `internal/runner/codex_app_server.go` (constant) | Add `const maxAppServerLineBytes = 10 * 1024 * 1024`. |
| `internal/runner/codex_app_server_test.go` | Add `TestAppServerClient_RejectsOversizedLine`: drive a fake stdin/stdout where the upstream emits a line longer than 10 MiB without `\n`; assert `readLine` (via a small wrapper) returns an error containing `"exceeded"` or `bufio.ErrTooLong` and does not OOM. |

## Concrete change

```go
const (
    appServerScannerInitialBuf = 64 << 10
    maxAppServerLineBytes      = 10 * 1024 * 1024 // SPEC §10.1
)

type appServerClient struct {
    stdin   io.Writer
    scanner *bufio.Scanner
    out     io.Writer
    // ... rest unchanged
}

// at construction site:
sc := bufio.NewScanner(stdout)
sc.Buffer(make([]byte, 0, appServerScannerInitialBuf), maxAppServerLineBytes)
client := &appServerClient{
    stdin:   stdin,
    scanner: sc,
    out:     buf,
}

// readLine:
go func() {
    if c.scanner.Scan() {
        line := append([]byte(nil), c.scanner.Bytes()...)
        ch <- readResult{line: line, err: nil}
        return
    }
    err := c.scanner.Err()
    if err == nil {
        err = io.EOF
    }
    if errors.Is(err, bufio.ErrTooLong) {
        err = fmt.Errorf("codex app-server line exceeded %d bytes: %w", maxAppServerLineBytes, err)
    }
    ch <- readResult{err: err}
}()

// readProtocolMessage:
_, _ = c.out.Write(line)
_, _ = c.out.Write([]byte{'\n'})
```

## Non-goals

- Don't make the cap configurable — SPEC value is the right default; revisit if operators report legitimate over-cap payloads.
- Don't touch the mock runner — it emits internal Go data structures, not stdio bytes. The cap is a stdio-protocol property.
- Don't add a `DEVIATIONS.md` row — this PR closes the deviation; SPEC §10.1 is now honored.

## Acceptance criteria

- [ ] Stdio reader bounded at 10 MiB.
- [ ] Overflow surfaces as a wrapped error containing the cap value (and `bufio.ErrTooLong` per `errors.Is`).
- [ ] Regression test simulates an oversized response and asserts graceful error (no OOM).
- [ ] Existing app-server tests continue to pass; transcript output still includes line boundaries.

## Refs

- Issue: https://github.com/xrf9268-hue/aiops-platform/issues/226
- Code: `internal/runner/codex_app_server.go:82-86, 169-175, 369-382, 346-362`
- SPEC §10.1 ("Max line size: 10 MB"), §15.1 (harness hardening)
- stdlib: `bufio.Scanner.Buffer`, `bufio.ErrTooLong`

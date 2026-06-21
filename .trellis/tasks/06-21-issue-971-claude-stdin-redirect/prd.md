# Issue #971: wire claude runner PROMPT.md via cmd.Stdin

## Goal

Deliver `PROMPT.md` to the one-shot `claude` runner on the child process's
stdin at the `exec.Cmd` level, instead of string-concatenating a shell
redirection onto the operator-configured command. Robustness hardening of the
existing one-shot path; split out from #547 (which kept the runner one-shot and
rejected the session-based rewrite).

## Background

`internal/runner/shell.go` currently builds:

```go
cmd := exec.CommandContext(ctx, "sh", "-c", command+" < .aiops/PROMPT.md")
```

Appending ` < .aiops/PROMPT.md` to an operator-configured `claude.command`
string misbehaves when that command contains a trailing comment, an existing
redirection/here-doc, a background job (`&`), or a pipeline (redirection binds
to the last stage only). Under the sandbox, the file is opened by the inner
`sh` inside the bwrap/firejail mount, depending on the relative path resolving
there.

## Requirements

- Open `.aiops/PROMPT.md` on the host (relative to `in.Workdir`) and assign the
  handle to `cmd.Stdin`; pass the configured command string to `sh -c`
  unmodified (no ` < .aiops/PROMPT.md` suffix).
- Set `cmd.Stdin` **after** `cmd = wrapped` (alongside the existing
  `cmd.Stdout`/`cmd.Stderr` assignment) so it survives the sandbox wrapper and
  the host-opened fd is passed through to the sandboxed child.
- Close the opened file handle once the run finishes (no fd leak); surface a
  clear error if the prompt file cannot be opened.
- Keep the runner **one-shot** — no session/turn-loop change (that was the
  rejected Option B in #547).
- No change to `claude.command` semantics, env handling, sandbox, or kill/timeout
  behavior.

## Acceptance Criteria

- [ ] `shell.go` no longer concatenates ` < .aiops/PROMPT.md`; `PROMPT.md` is on
      the child's stdin via `cmd.Stdin`.
- [ ] A command string with a trailing comment (e.g. `claude.command:
      "claude # run"`) still receives the prompt on stdin.
- [ ] New test drives the real `ShellRunner` and asserts the child observed
      `PROMPT.md`'s exact bytes on stdin (e.g. a capture stub as `claude.command`),
      mutation-verified by dropping the `cmd.Stdin` assignment.
- [ ] Existing shell-runner tests (kill, timeout classification, env isolation,
      secret deny) still pass.
- [ ] CI gate green locally: `gofmt -l`, `go vet`, golangci-lint (blocking, no
      `--issues-exit-code=0`), `go test -race ./...`, `go build ./cmd/...`.

## Out of scope

- Session-based / SDK-sidecar Claude runner (rejected in #547).
- Any change to `codex-app-server` or `mock` runners.

## PR shape

- `fix(runner): …` Conventional Commit title; `Closes #971`.
- ~1 production file (`shell.go`) + test; `within budget`.

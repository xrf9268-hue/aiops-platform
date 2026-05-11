# Safe Codex Runner Profile

**Issue:** [#17](../../../) — [M4][P0] Implement safe Codex runner profile
**Date:** 2026-05-11
**Status:** Approved (pending implementation plan)

## Problem

`agent.default: codex` already routes through `internal/runner` via the generic `ShellRunner`, which executes `sh -lc "<codex.command> < .aiops/PROMPT.md"`. Three of issue #17's five acceptance criteria are not yet met:

1. **Safe prompt passing.** The PROMPT.md path is glued onto an operator-supplied command string with shell concatenation. There is no argv-form invocation, no opt-in to codex CLI's own sandbox / approval gates, and no way for the worker to be sure what flags the operator actually used.
2. **Logs captured into events or artifacts.** Codex stdout/stderr go to `os.Stdout` / `os.Stderr` and disappear. The worker keeps no per-run record of what codex said. Triage requires re-running the task locally.
3. **A "safe" default profile.** There is no documented "what a personal user gets by default when they flip `agent.default: codex`." Today the default is whatever string they typed into `codex.command`, with no schema-level guidance.

The platform's M4 milestone is "switch from mock to codex for small personal tasks." Without the three items above, that switch is ergonomically inconsistent and operationally opaque.

## Goals

1. A documented `codex.profile` schema field with three values: `safe` (default), `bypass`, `custom`.
2. Profile-driven argv assembly inside a dedicated `CodexRunner`, replacing the shared `ShellRunner` for codex (claude keeps using `ShellRunner`).
3. Codex stdout/stderr captured to `.aiops/CODEX_OUTPUT.txt` (capped at 1 MiB, drop counter recorded) AND surfaced in the `runner_end` event payload as head/tail/bytes/dropped.
4. `codex exec`'s `--output-last-message` artifact picked up to populate `Result.Summary` (so the worker has a one-line "what did codex finish saying" hook without parsing the full output).
5. `WORKFLOW.md`, runbook, and `examples/WORKFLOW.md` updated so an operator can predict behavior from docs alone.

## Non-Goals

- OS-level sandboxing (sandbox-exec, firejail, container isolation). Stays in the codex CLI's own sandbox model.
- Streaming codex output to the event log in real time. Final-only capture is enough for triage; streaming would need a different transport and is YAGNI for personal-scale tasks.
- Migrating to `codex app-server` (Symphony's pattern — see "Prior art" below). One-shot `codex exec` matches the queue's per-task lifecycle; app-server is a separate M5+ design.
- Changing how claude is invoked. `ShellRunner` continues to power `agent.default: claude`.
- A schema field on `Claude` for parity. YAGNI — claude has no equivalent CLI-side approval/sandbox knobs to wrap.

## Profile Contract

| Profile | Argv (after `codex` binary) | Notes |
|---|---|---|
| `safe` (default) | `exec --full-auto --skip-git-repo-check --cd <workdir> -o <workdir>/.aiops/CODEX_LAST_MESSAGE.md` | `--full-auto` is codex's documented shorthand for `--sandbox workspace-write --ask-for-approval=never`. PROMPT.md is fed via stdin (codex CLI reads stdin when no positional PROMPT is provided). The `-o` path is absolute so the artifact lands in the workdir even if `--cd` and `cmd.Dir` ever diverge. |
| `bypass` | `exec --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check --cd <workdir> -o <workdir>/.aiops/CODEX_LAST_MESSAGE.md` | For operators who run the worker inside an already-isolated environment (container, VM) and need codex to write outside the workspace cap. Documented as opt-in. |
| `custom` | (n/a — falls back to `ShellRunner` with `sh -lc <codex.command>`) | Escape hatch. Operator-controlled string; no safety promises, but logs/timeout/output capture all still apply. |

`safe` is the default supplied by `expandConfig`. An empty `Codex.Profile` after YAML load is normalized to `safe`. Unknown values fail load with a clear error path/value pair (same pattern as `supportedTrackerKinds` in `loader.go`).

The `codex.command` field is consulted only when `profile=custom`. For `safe`/`bypass` it is ignored — runner-built argv is the source of truth. The default `codex.command: "codex exec"` stays in the schema so existing configs (which set `profile=custom` to keep their wording) keep working.

### Why a profile enum and not Symphony-style independent fields

Symphony (see `elixir/lib/symphony_elixir/config/schema.ex` `Codex` schema) exposes `command`, `approval_policy`, `thread_sandbox`, `turn_sandbox_policy`, plus three timeouts as independent fields. The richer surface fits Symphony's `codex app-server` long-session model where per-turn sandbox overrides are meaningful.

This platform's M4 is a one-shot `codex exec` per task. The profile enum collapses the "what does safe mean" decision to a single named bundle, mirroring codex CLI's own `--full-auto` shorthand. If a future milestone migrates to `codex app-server`, the profile enum can either remain as a coarse switch or be replaced wholesale; either way the smaller surface today does not paint us into a corner.

Where naming intersects, we adopt Symphony's terms verbatim: `workspace-write`, `danger-full-access` semantics. Migration paths stay legible.

## API Surface

### `internal/workflow/config.go`

```go
type CommandConfig struct {
    Command string `yaml:"command" json:"command"`
    // Profile is consulted only for the codex runner. Allowed values:
    // "safe" (default), "bypass", "custom". The field exists on the
    // shared CommandConfig type to avoid splitting CodexConfig out for
    // a single field; loader validation rejects non-empty Profile on
    // the Claude embed.
    Profile string `yaml:"profile,omitempty" json:"profile,omitempty"`
}
```

`DefaultConfig()` keeps `Codex: CommandConfig{Command: "codex exec", Profile: "safe"}`.

`expandConfig(&cfg)` adds:

```go
if cfg.Codex.Profile == "" {
    cfg.Codex.Profile = "safe"
}
```

`validateConfig(path, cfg)` adds a `supportedCodexProfiles` map (`safe | bypass | custom`) and rejects unknown values with `"%s: codex.profile %q is not supported (allowed: safe, bypass, custom)"`. `cfg.Claude.Profile != ""` returns `"%s: claude.profile is not supported"` — this prevents copy-paste mistakes silently doing nothing.

### `internal/runner/runner.go`

```go
type Result struct {
    Summary       string
    OutputBytes   int64  // bytes written to the artifact (post-cap)
    OutputDropped int64  // bytes dropped because output exceeded cap
    OutputHead    string // first ~4 KiB of combined stdout/stderr
    OutputTail    string // last ~4 KiB; empty when overlapping head
}

func New(name string) (Runner, error) {
    switch name {
    case "", "mock":   return MockRunner{}, nil
    case "codex":      return CodexRunner{}, nil   // was: ShellRunner{Name: "codex"}
    case "claude":     return ShellRunner{Name: "claude"}, nil
    default:           return nil, fmt.Errorf("unknown runner: %s", name)
    }
}
```

`TimeoutError`, `IsTimeout`, `deadlineBudget` unchanged.

### `internal/runner/codex.go` (new)

```go
type CodexRunner struct{}

func (r CodexRunner) Run(ctx context.Context, in RunInput) (Result, error)
```

Implementation outline:

1. Validate PROMPT.md exists at `<workdir>/.aiops/PROMPT.md`. If not, return a wrapped error (not a `TimeoutError`).
2. Build `*exec.Cmd` based on `in.Workflow.Config.Codex.Profile`:
   - `safe`: `exec.CommandContext(ctx, "codex", "exec", "--full-auto", "--skip-git-repo-check", "--cd", in.Workdir, "-o", ".aiops/CODEX_LAST_MESSAGE.md")`. PROMPT.md piped via stdin.
   - `bypass`: same as `safe` but `--full-auto` replaced by `--dangerously-bypass-approvals-and-sandbox`.
   - `custom`: `exec.CommandContext(ctx, "sh", "-lc", in.Workflow.Config.Codex.Command)`. PROMPT.md piped via stdin (NOT via the legacy `< .aiops/PROMPT.md` shell redirect — uniform stdin handling across profiles means the capture path is one branch). Operator's `codex.command` must therefore consume stdin (typically `codex exec` or any binary that does so when no positional prompt is given). This is a documented behavior change vs the previous ShellRunner-for-codex path: existing operators using `codex.command: "codex exec"` keep working; operators with custom shell pipelines that already used stdin keep working; operators relying on the legacy `< .aiops/PROMPT.md` glue need to migrate to stdin (which is what their command would have received anyway since `sh -lc "codex exec < .aiops/PROMPT.md"` was the old composition). The runbook update calls this out.
3. Common path for all three profiles: `cmd.Dir = in.Workdir`; `configurePlatformKill(cmd)`; `cmd.WaitDelay = killGrace`.
4. `cmd.Stdin, _ = os.Open(filepath.Join(in.Workdir, ".aiops/PROMPT.md"))`. Close after `cmd.Run()` returns.
5. `buf := &cappedWriter{Cap: CodexOutputCap}; cmd.Stdout = buf; cmd.Stderr = buf`.
6. `runErr := cmd.Run()`. Compute `elapsed := time.Since(start)`.
7. Always write `.aiops/CODEX_OUTPUT.txt` from `buf` (with a trailing `...output truncated at N bytes\n` line iff `buf.Dropped() > 0`). Write failures are logged but do not mask the runner's outcome.
8. Read `.aiops/CODEX_LAST_MESSAGE.md` if present; trim and use as `Result.Summary`. If absent or empty, fall back to `"codex completed"`.
9. On `runErr != nil`, distinguish ctx-driven termination (return `*TimeoutError`) from non-zero exit (return `runErr` raw), matching `ShellRunner` semantics.
10. Populate `Result.OutputBytes` (= bytes kept by the buffer), `OutputDropped` (= dropped count), `OutputHead` (first 4 KiB of buffered bytes), `OutputTail` (last 4 KiB; empty when buffered length ≤ 4 KiB so it does not duplicate head).

### `internal/runner` shared output buffer

A small `cappedWriter` lives in a new `internal/runner/capture.go`. It mirrors `workspace.cappedBuffer` (1 MiB cap, drop counter, never blocks). Duplicating ~30 lines is preferable to forcing `internal/runner` to import `internal/workspace` (today the dependency goes the other way is moot — workspace doesn't import runner — but introducing the reverse edge for one helper would entangle the layering).

Constants:

```go
// CodexOutputCap bounds bytes kept in the runner-side capture buffer.
// Matches workspace.VerifyOutputCap so artifact ergonomics are uniform
// across the verify and runner phases.
const CodexOutputCap = 1 << 20 // 1 MiB

// CodexOutputPath is the artifact location for the captured codex
// stdout+stderr. Always relative to the workdir.
const CodexOutputPath = ".aiops/CODEX_OUTPUT.txt"

// CodexLastMessagePath is the file codex CLI writes when invoked with
// -o; we ingest its trimmed contents into Result.Summary on success.
const CodexLastMessagePath = ".aiops/CODEX_LAST_MESSAGE.md"

// CodexEventOutputCap bounds head/tail bytes embedded in the runner_end
// event payload (per side). 4 KiB stays well under typical Postgres
// JSONB row-size budgets and keeps the events table cheap to query.
const CodexEventOutputCap = 4 << 10 // 4 KiB
```

### `internal/worker/runtask.go`

`RunRunnerWithTimeout`'s `runner_end` payload (success and failure paths) is enriched conditionally:

```go
endPayload := map[string]any{
    "model":       in.Task.Model,
    "duration_ms": elapsed.Milliseconds(),
    "ok":          true,
}
if res.Summary != "" {
    endPayload["summary"] = res.Summary
}
if res.OutputBytes > 0 {
    endPayload["output_bytes"] = res.OutputBytes
}
if res.OutputDropped > 0 {
    endPayload["output_dropped"] = res.OutputDropped
}
if res.OutputHead != "" {
    endPayload["output_head"] = res.OutputHead
}
if res.OutputTail != "" {
    endPayload["output_tail"] = res.OutputTail
}
```

The same conditional block applies to the failure-path `runner_end` and to the `runner_timeout` event (so a hung codex still leaves head/tail crumbs in the event log). Mock runs (which leave the new fields zero) emit no extra keys, preserving backward compatibility for tests that diff payloads exactly.

## Data Flow

```
runtask.go
  → workflow.Resolve(workdir)                            (existing)
  → runner.New("codex") → CodexRunner{}
  → RunRunnerWithTimeout(ctx, ev, runner, RunInput{
      Task, Workflow{Codex.Profile=safe|bypass|custom},
      Workdir, Prompt}, agent.timeout, workflowSource)
       → emit runner_start
       → CodexRunner.Run
            switch Codex.Profile
              safe   → argv = [codex exec --full-auto ...]
              bypass → argv = [codex exec --dangerously-bypass... ...]
              custom → ShellRunner.Run + capture wrap
            cmd.Stdin = open(.aiops/PROMPT.md)
            cmd.Stdout/Stderr = cappedWriter
            run; writeArtifact; readLastMessage
            return Result{Summary, Output*}
       → emit runner_end (or runner_timeout) with payload
         conditionally including output_head/tail/bytes/dropped
```

## Error Handling

| Failure | Behavior |
|---|---|
| PROMPT.md missing/unreadable | Return wrapped `fmt.Errorf("read PROMPT.md: %w", err)`. NOT a `TimeoutError`. RunRunnerWithTimeout records `runner_end ok=false`. |
| `codex` binary missing on PATH | `exec.LookPath` pre-check; return `fmt.Errorf("codex binary not found in PATH; install codex CLI or set agent.default to claude/mock")`. |
| Codex non-zero exit | Return raw `*exec.ExitError` (or wrapped equivalent). `IsTimeout=false`. Existing failure routing in `handleTaskFailure` applies. |
| Context deadline exceeded | Wrap in `*TimeoutError{Timeout, Elapsed, Cause}` exactly like `ShellRunner` today. `runner_timeout` event fires. |
| Output cap hit | Silent — `cappedWriter` drops bytes and increments the drop counter. Artifact gets a trailing truncation line; event payload reports `output_dropped`. |
| `.aiops/CODEX_OUTPUT.txt` write fails | Logged via `log.Printf`; runner outcome unchanged. The triage signal already lives in event payload head/tail. |
| `.aiops/CODEX_LAST_MESSAGE.md` missing | Treated as success with `Result.Summary = "codex completed"`. (Codex CLI writes the file only when it has a final message; absence is benign.) |

The `custom` profile inherits ShellRunner's semantics (today's behavior). The new artifact + payload enrichment still applies because the wrapper captures stdout/stderr regardless of which profile branch built the command.

## Testing

### `internal/runner/codex_test.go` (new)

Tests use a temp-PATH stub: write a small bash script named `codex` into `t.TempDir()`, prepend that dir to `PATH`. The script can record argv, dump stdin to a file, print fixed output, exit with a chosen code, or sleep, depending on the test.

| Case | Assertion |
|---|---|
| `safe` profile builds expected argv | recorded argv equals `[exec --full-auto --skip-git-repo-check --cd <workdir> -o .aiops/CODEX_LAST_MESSAGE.md]` (positional PROMPT absent → stdin path). |
| `bypass` profile builds expected argv | argv contains `--dangerously-bypass-approvals-and-sandbox`; does NOT contain `--full-auto`. |
| `custom` profile uses operator command via `sh -lc` with stdin PROMPT | Stub binary not consulted; `codex.command: "cat"` runs through `sh -lc`, copies PROMPT.md from stdin to stdout; artifact captures the prompt bytes. |
| stdin carries PROMPT.md | Stub copies stdin to a temp file; contents equal PROMPT.md bytes. |
| stdout/stderr captured to artifact | Stub prints a canary string; `<workdir>/.aiops/CODEX_OUTPUT.txt` exists and contains the canary. |
| Output exceeds cap | Stub prints 1.5 MiB; artifact size ≤ cap + truncation footer; `Result.OutputDropped > 0`; trailing line matches `...output truncated at 1048576 bytes\n`. |
| `--output-last-message` pickup | Stub writes `.aiops/CODEX_LAST_MESSAGE.md` containing `done!\n`; `Result.Summary == "done!"`. |
| Missing last-message file | Stub does not write the file; `Result.Summary == "codex completed"`. |
| Timeout kills process group | Stub sleeps 30s; ctx 50ms; returns `*TimeoutError`; elapsed < 10s; (asserts that the existing group-kill path applies via `configurePlatformKill`). |
| Non-zero exit not misclassified | Stub `exit 3`; error returned, `IsTimeout(err) == false`, `errors.As` to `*exec.ExitError` succeeds. |
| Missing PROMPT.md | No `.aiops/PROMPT.md` in workdir; runner returns wrapped error mentioning PROMPT.md; codex stub never invoked. |
| Missing codex binary | Empty PATH; runner returns the documented "codex binary not found" error before exec. |
| Result fields on success path | `OutputBytes` matches artifact size on disk; `OutputHead` ≤ 4 KiB; `OutputTail` empty when total output ≤ 4 KiB, set otherwise. |

### `internal/workflow/config_test.go` (extend)

| Case | Expected |
|---|---|
| Default config has `Codex.Profile == "safe"` | Pass. |
| `codex.profile: safe|bypass|custom` accepted | Load returns nil error. |
| `codex.profile: yolo` rejected | Error includes path and the offending value `"yolo"` and the allowed list. |
| `codex.profile` empty after YAML load | `expandConfig` normalizes to `"safe"`. |
| `claude.profile: safe` rejected | Error mentions `claude.profile is not supported` and the path. |

### `internal/worker/run_test.go` (extend)

| Case | Expected |
|---|---|
| `runner_end` payload includes `output_*` keys when Result populates them | Fake runner returns Result with all four fields set; payload contains all four. |
| `runner_end` payload omits `output_*` for mock | Mock runner returns Result with zero output fields; payload diff against existing snapshot is unchanged. |
| `runner_timeout` payload includes `output_head/tail` if Result captured anything before kill | Stub runner that emits some bytes and then sleeps; ctx fires; payload still carries head/tail. |

### `internal/runner/runner_test.go` (revise)

The two existing codex-via-ShellRunner tests (`TestShellRunnerKillsRunawayProcess`, `TestShellRunnerNonTimeoutErrorNotMisclassified`) keep their kill/exit-class assertions but bind to `ShellRunner{Name: "claude"}` so they remain meaningful for the runner that still uses the shell path. Codex-specific kill/exit tests live in `codex_test.go`.

## Documentation

- **`examples/WORKFLOW.md`**: under the `codex:` block, add a commented-out `profile: safe` line plus a one-paragraph note that `safe` is the default, `bypass` is for already-isolated hosts, and `custom` falls back to the `command` string.
- **`docs/runbooks/personal-daily-workflow.md`**: in the `codex` runner subsection, describe profile semantics and the trade-offs between `safe` and `bypass`. Add a short "Reading codex output after a run" pointer to `.aiops/CODEX_OUTPUT.txt` and the `runner_end` event payload fields.
- **`docs/symphony-integration.md`**: remove "advanced sandboxing" from the "Not yet implemented" list (or refine wording to "OS-level sandboxing not yet; codex CLI sandbox is wired"). Add a one-line "see also Symphony's app-server-based codex integration in `lib/symphony_elixir/codex/app_server.ex`" pointer for future M5+ context.
- **`README.md`**: if/where the README mentions `codex.command`, add a sentence pointing at `codex.profile`. No new top-level section required.

## Files Changed

1. `internal/workflow/config.go` — add `CommandConfig.Profile`; default to `"safe"` for codex in `DefaultConfig`.
2. `internal/workflow/loader.go` — `supportedCodexProfiles`; validate `Codex.Profile`; reject `Claude.Profile`; `expandConfig` normalizes empty `Codex.Profile`.
3. `internal/workflow/config_test.go` — schema cases above.
4. `internal/runner/runner.go` — extend `Result`; route `New("codex")` to `CodexRunner`.
5. `internal/runner/codex.go` (new) — profile dispatch, argv assembly, capture, artifact write, last-message pickup.
6. `internal/runner/capture.go` (new) — small `cappedWriter` (1 MiB cap, drop counter); shared by codex and `custom`-fallback paths.
7. `internal/runner/codex_test.go` (new) — tests above using PATH-stub codex.
8. `internal/runner/runner_test.go` — rebind the two ShellRunner tests to `claude` (or split into `shell_test.go`).
9. `internal/worker/runtask.go` — `RunRunnerWithTimeout` payload enrichment for `runner_end` and `runner_timeout`.
10. `internal/worker/run_test.go` — payload assertions above.
11. `examples/WORKFLOW.md` — `codex.profile` comment block.
12. `docs/runbooks/personal-daily-workflow.md` — profile section + output reading pointer.
13. `docs/symphony-integration.md` — sandboxing wording + Symphony app-server pointer.
14. `README.md` — codex.profile mention near codex.command.

## Acceptance Criteria Mapping

| Issue #17 criterion | Where addressed |
|---|---|
| `agent.default: codex` runs through `internal/runner` | `runner.New("codex") → CodexRunner` (no regression in routing). |
| Runner passes `.aiops/PROMPT.md` safely | argv-form `exec.CommandContext` + stdin pipe in `safe`/`bypass`; no shell concatenation in those branches. |
| Runner respects configured command | `profile=custom` reads `codex.command` and runs through the `ShellRunner` path; new artifact/payload enrichment still applies. |
| Timeout is applied | Existing `RunRunnerWithTimeout` + `agent.timeout` + `configurePlatformKill` group-kill, reused verbatim. |
| Logs are captured into task events or artifacts | `.aiops/CODEX_OUTPUT.txt` artifact (capped + truncation footer) AND `runner_end` payload `output_head/tail/bytes/dropped`. |

The Notes line "Start with a personal demo repository" is operational guidance for the operator — not a code deliverable in this PR. It is surfaced via the runbook update and the `examples/WORKFLOW.md` comment so the path is discoverable.

## Prior Art

- **Symphony (`elixir/lib/symphony_elixir/config/schema.ex` and `codex/app_server.ex`)**: uses `codex app-server` for a long-lived JSON-RPC session and exposes `command`, `approval_policy`, `thread_sandbox`, `turn_sandbox_policy`, plus three timeouts as independent config fields. Richer surface, fits the per-turn-override semantics of app-server. Out of scope here; profile enum collapses to a single decision per task and stays migration-friendly should this platform later adopt app-server.
- **Codex CLI `codex-rs/exec/src/cli.rs`**: documents `--full-auto` as the canonical "automatic + workspace-write" shorthand and `--dangerously-bypass-approvals-and-sandbox` (alias `--yolo`) for already-isolated environments. Profile names map directly: `safe = --full-auto`, `bypass = --dangerously-bypass-approvals-and-sandbox`.

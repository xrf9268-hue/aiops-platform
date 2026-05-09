# Workflow Discovery and Fallback Behavior

**Issue:** [#10](../../../) — [M2][P0] Define workflow discovery and fallback behavior
**Date:** 2026-05-09
**Status:** Approved (pending implementation plan)

## Problem

The worker is non-deterministic about *where* `WORKFLOW.md` lives and *what was used* on a given run. Today:

- Discovery is hardcoded to `<workdir>/WORKFLOW.md` (`cmd/worker/main.go:95`).
- `workflow.LoadOptional` silently substitutes built-in defaults when the file is absent — but no task event records that this happened, so an operator inspecting the event stream cannot tell apart "ran with the repo's WORKFLOW.md", "ran with a prompt-only file", and "ran with zero repo configuration".
- README mentions `WORKFLOW.md` in passing but does not define a discovery contract.

This is the central contract for Symphony-style operation; downstream M3 work (Linear status transitions) and M4 work (verifier independence) both assume it is settled.

## Goals

1. Worker loads repo `WORKFLOW.md` from a documented set of locations, with a documented precedence.
2. Worker uses safe built-in defaults when no `WORKFLOW.md` is found.
3. Every task event stream records which workflow source was used and where it was found.
4. README documents the contract so a repo author can predict worker behavior without reading Go.
5. A `worker --print-config <workdir>` debug entry-point lets a human inspect the effective config without consuming a task or touching the database.

## Non-Goals

- Changing `cmd/linear-poller`. The poller takes an explicit path argument; discovery is a worker-side concern.
- Deleting `workflow.LoadOptional`. It is marked deprecated; existing callers (notably any future `cmd/api` use) keep working until they migrate.
- A `--diff-against-defaults` mode, multiple output formats, or YAML output for `--print-config`.
- Replacing the worker's `os.Args` handling with the `flag` package. One positional sub-command does not justify it.
- Emitting a separate `workflow_shadowed` warning event. The shadow list is carried inside `workflow_resolved`.

## Discovery Contract

Worker searches the workdir in declared order. **First hit wins.**

| Priority | Path |
|----------|------|
| 1 | `<workdir>/WORKFLOW.md` |
| 2 | `<workdir>/.aiops/WORKFLOW.md` |
| 3 | `<workdir>/.github/WORKFLOW.md` |

If multiple files exist, the lower-priority ones are recorded as shadowed (data only — no warning event, no error).

If none of the three exist, the worker proceeds with built-in defaults. This preserves the current `LoadOptional` behavior of "every repo can run, even pre-Symphony adoption."

### Source classification

The `Resolution.Source` field is one of:

- `file` — a `WORKFLOW.md` was found and contains a YAML front-matter block.
- `prompt_only` — a `WORKFLOW.md` was found but has no front matter; the worker uses the file's body as the prompt template and built-in defaults for everything else. Backward-compatible with the contract pinned by `TestLoad_AcceptsPromptOnlyFile`.
- `default` — no file found; both config and prompt come from `DefaultConfig()` / `DefaultPrompt()`.

### Error semantics

- Schema validation errors from a found file (missing `repo.clone_url`, unsupported `tracker.kind`, etc.) propagate. The worker does **not** silently fall back to defaults — that would mask a real configuration mistake.
- `os.Stat` errors other than `IsNotExist` (e.g., permission denied) propagate, matching `LoadOptional`'s current semantics.

## API Surface

New, in `internal/workflow`:

```go
type Source string

const (
    SourceFile       Source = "file"
    SourcePromptOnly Source = "prompt_only"
    SourceDefault    Source = "default"
)

type Resolution struct {
    Source     Source
    Path       string   // repo-relative, "" when Source=default
    ShadowedBy []string // repo-relative, declaration order, omitempty
}

func Resolve(workdir string) (*Workflow, *Resolution, error)
```

Rationale for `Resolution` as a separate type rather than fields on `Workflow`:

- `Workflow` is a description of *what* the configuration is. `Resolution` is a runtime fact about *where it came from* on this particular invocation. Mixing them muddles two lifetimes (config can be cached/serialized; resolution cannot).
- The shadow list has no meaning outside discovery. Putting it on `Workflow` would force every other `Workflow` constructor (tests, future cached loads) to think about it.

### Implementation sketch

```go
func Resolve(workdir string) (*Workflow, *Resolution, error) {
    candidates := []string{
        "WORKFLOW.md",
        ".aiops/WORKFLOW.md",
        ".github/WORKFLOW.md",
    }
    var found string
    var shadows []string
    for _, rel := range candidates {
        abs := filepath.Join(workdir, rel)
        if _, err := os.Stat(abs); err == nil {
            if found == "" {
                found = rel
            } else {
                shadows = append(shadows, rel)
            }
        } else if !os.IsNotExist(err) {
            return nil, nil, err
        }
    }
    if found == "" {
        cfg := DefaultConfig()
        expandConfig(&cfg)
        return &Workflow{Config: cfg, PromptTemplate: DefaultPrompt()},
            &Resolution{Source: SourceDefault},
            nil
    }
    wf, err := Load(filepath.Join(workdir, found))
    if err != nil {
        return nil, nil, err
    }
    src := SourceFile
    if !hasFrontMatterAt(filepath.Join(workdir, found)) {
        src = SourcePromptOnly
    }
    return wf, &Resolution{Source: src, Path: found, ShadowedBy: shadows}, nil
}
```

`hasFrontMatterAt` is a small helper that re-runs `splitFrontMatter` on the bytes (`Load` already does this internally; rather than threading a flag out of `Workflow`, classify here). The double parse is cheap relative to `git clone` / runner invocation and keeps `Workflow` unchanged.

### Deprecation

`LoadOptional` gets a doc comment:

```
// Deprecated: use Resolve(workdir) for repo-relative discovery and
// resolution metadata. Retained for callers that pass an explicit path.
```

`Load(path)` is unchanged.

## Worker Integration

`cmd/worker/main.go runTask`:

```go
wf, res, err := workflow.Resolve(workdir)
if err != nil {
    return workflow.Config{}, err
}
payload := map[string]any{
    "source":        string(res.Source),
    "agent_default": wf.Config.Agent.Default,
    "policy_mode":   wf.Config.Policy.Mode,
    "tracker_kind":  wf.Config.Tracker.Kind,
}
if res.Path != "" {
    payload["path"] = res.Path
}
if len(res.ShadowedBy) > 0 {
    payload["shadowed_by"] = res.ShadowedBy
}
emit(ctx, ev, t.ID, task.EventWorkflowResolved, "workflow resolved", payload)
```

`path` and `shadowed_by` are added conditionally so that the `default` source case does not emit an empty `path: ""` or `shadowed_by: []`. `map[string]any` has no `omitempty` mechanism — the conditional inserts make the contract explicit.

The existing `runner_start` payload gets one additional key:

```go
"workflow_source": string(res.Source)
```

`res` is threaded through `runTask` as a local; no changes to existing function signatures other than the worker's own.

## New Event

`internal/task/task.go`:

```go
EventWorkflowResolved = "workflow_resolved"
```

Emitted exactly once per task, after `Resolve` succeeds, before `runner.New`. If `Resolve` returns an error (schema validation), no `workflow_resolved` event is emitted — there was nothing to resolve. The task fails through the existing error path.

## `worker --print-config <workdir>` Subcommand

### Behavior

- `worker --print-config <workdir>` runs `workflow.Resolve(workdir)` and writes JSON to stdout.
- Schema validation errors go to stderr, exit code 1.
- Does **not** open a database connection or read `DATABASE_URL`.
- Output shape:

```json
{
  "resolution": {
    "source": "file",
    "path": ".aiops/WORKFLOW.md",
    "shadowed_by": ["WORKFLOW.md"]
  },
  "config": {
    "repo": { "...": "..." },
    "tracker": { "api_key": "***", "...": "..." },
    "agent": { "default": "codex", "...": "..." }
  },
  "prompt_template": {
    "length": 1234,
    "first_line": "You are working on a personal AI coding task."
  }
}
```

### Secret masking

Before serialization, replace `Tracker.APIKey` with `"***"` if non-empty.

This is the only secret-bearing field today. The list lives next to `printConfig` so a future schema addition has an obvious place to land.

### Why prompt body is summarized, not printed

The prompt template is treated as opaque text by the worker, and `prompt_only` `WORKFLOW.md` files allow the body to be anything an author chooses to put there. `--print-config` output gets pasted into shell history, support bundles, gists, and CI logs — venues with broader visibility than the repo. Echoing the entire prompt verbatim would create an asymmetric safety contract (we mask `api_key` but dump unbounded body bytes next to it).

Instead the debug command emits a small, deterministic summary:

```go
type promptSummary struct {
    Length    int    `json:"length"`     // utf-8 byte length of the trimmed body
    FirstLine string `json:"first_line"` // first non-empty line, truncated at 200 bytes
}
```

A reader who needs the full prompt can `cat <resolution.path>` directly — the file is right there. We deliberately do **not** add an `--include-prompt` flag: extra knobs invite misuse, and the local file is the canonical source.

This is a usability/safety contract decision, not a redaction policy: we make no claim that the summary scrubs secrets a determined operator embeds in a prompt. The contract is "this debug command does not enlarge the leak surface beyond the config fields whose schema we own."

### Wiring

```go
func main() {
    if len(os.Args) >= 3 && os.Args[1] == "--print-config" {
        os.Exit(printConfig(os.Args[2]))
    }
    // existing main loop
}
```

A bare `os.Args` check, not the `flag` package: one sub-command, one positional.

## Testing

### `internal/workflow/loader_test.go`

| Case | Expected |
|------|----------|
| All three locations empty | `Source=default, Path="", ShadowedBy=nil` |
| Only root, with front matter | `Source=file, Path="WORKFLOW.md"` |
| Only `.aiops/` | `Source=file, Path=".aiops/WORKFLOW.md"` |
| Only `.github/` | `Source=file, Path=".github/WORKFLOW.md"` |
| Root + `.aiops/` | `Path="WORKFLOW.md"`, `ShadowedBy=[".aiops/WORKFLOW.md"]` |
| Root + `.aiops/` + `.github/` | `ShadowedBy=[".aiops/WORKFLOW.md", ".github/WORKFLOW.md"]` (declared order) |
| Only root, prompt-only body | `Source=prompt_only, Path="WORKFLOW.md"` |
| Only root, front matter missing `clone_url` | error mentions `repo.clone_url` and the path |

### `cmd/worker/main_test.go`

- Fake `eventEmitter` captures emitted events; assert `workflow_resolved` is present with `source` and `agent_default` keys on the success path.
- Assert `runner_start` payload contains `workflow_source`.
- `--print-config` against a temp workdir for each of the three layouts (root / `.aiops/` / `.github/`); decode JSON; assert `resolution.source` and `resolution.path`.
- `--print-config` with `Tracker.APIKey` set in front matter; assert output contains `"api_key":"***"`, never the original value.
- `--print-config` with a long prompt body (e.g. 5 KB containing a recognizable canary string `SHOULD_NOT_LEAK_xyz`); decode JSON; assert `prompt_template.length` equals the trimmed byte count, `prompt_template.first_line` is truncated at 200 bytes, and the canary string does NOT appear anywhere in stdout.

## Documentation

`README.md` gains a "WORKFLOW.md discovery" section near line 31 (the existing `internal/workflow` mention). Contents:

- Three search locations, in priority order.
- What "absent" means: built-in defaults, with the operationally interesting ones spelled out (`agent.default: mock`, `agent.timeout: 30m`, `pr.draft: false`, `pr.labels: [ai-generated, needs-review]`, no verify commands, no policy deny-paths). Reader should not need to open Go to predict behavior.
- Prompt-only files are supported and produce `Source=prompt_only`.
- Pointer to `worker --print-config <workdir>` for debugging.
- Pointer to the `workflow_resolved` event for post-hoc inspection.

## Files Changed

1. `internal/workflow/loader.go` — `Source` constants, `Resolution` type, `Resolve` function, `LoadOptional` deprecation comment, `hasFrontMatterAt` helper.
2. `internal/workflow/loader_test.go` — discovery test matrix above.
3. `internal/task/task.go` — `EventWorkflowResolved` constant.
4. `cmd/worker/main.go` — `runTask` switches to `Resolve`; emits `workflow_resolved`; `runner_start` payload gains `workflow_source`; `main()` entry handles `--print-config`; new `printConfig` and secret-masking helpers.
5. `cmd/worker/main_test.go` — emit assertions and `--print-config` tests.
6. `README.md` — discovery section.

## Acceptance Criteria Mapping

| Issue #10 criterion | Where addressed |
|---------------------|-----------------|
| Worker loads repo `WORKFLOW.md` when present | `Resolve` discovery (root → `.aiops/` → `.github/`) |
| Worker uses safe defaults when absent | `Source=default` branch in `Resolve` |
| Task event records which workflow file/default was used | `EventWorkflowResolved` + `workflow_source` on `runner_start` |
| README documents where `WORKFLOW.md` should live | New discovery section |

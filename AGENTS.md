# AGENTS.md

Project guide for AI coding agents (Codex CLI, Claude Code, etc.) working in this repo.

**This file is the single source of truth for all engineering rules.** Other
coding agents read it via thin bridge files: `CLAUDE.md` imports it with
`@AGENTS.md` for Claude Code. Do not duplicate content in the bridge files —
update only this file.

## What this project is

`aiops-platform` is a Go-based, self-hostable AI coding orchestrator that
implements the [OpenAI Symphony SPEC](https://github.com/openai/symphony/blob/main/SPEC.md).
The orchestrator polls a tracker (Linear, soon Gitea), prepares a deterministic
per-issue workspace, runs a coding agent in that workspace, and watches the
agent's lifecycle. Per SPEC §1, the **agent** is what writes tickets, opens
PRs, and pushes branches — through tools the orchestrator advertises
(`linear_graphql` and equivalent for Gitea). The orchestrator is the
scheduler/runner and tracker *reader*, not a tracker writer.

The Go module path is `github.com/xrf9268-hue/aiops-platform` — keep it as-is even if the GitHub repo is temporarily mirrored elsewhere.

> **Transitional notes** — several pieces of the current implementation deviate
> from this SPEC-aligned picture and are being reverted. Do not design new
> code that depends on the legacy behavior:
>
> - **Postgres queue**: removed under #407 as the second half of #73. Do not
>   reintroduce `internal/queue`, `migrations/`, `cmd/linear-poller`, or
>   `cmd/gitea-poller`; tracker polling belongs to `cmd/worker`.
> - **Gitea webhook ingress** (`cmd/trigger-api`, `internal/triggerapi`,
>   `internal/gitea/webhook*.go`): not in SPEC; removed under #74 in favor
>   of tracker polling.
> - **Orchestrator-driven PR creation, git push, and Linear status writes**:
>   closed under #76 after #64/#14; the worker must not reintroduce
>   `CommitAndPush`, `CreatePR`, `OnClaim`, or `OnPRCreated`-style handoff.
> - **Per-tick reconciliation and agent-cancel propagation** landed under
>   PR #131 (D9 closed). The worker should continue to stop active runs
>   when tracker state changes make them ineligible.
> - **Multi-path WORKFLOW.md discovery** (`internal/workflow/resolver.go`):
>   closed under #72. The resolver now uses the canonical root
>   `WORKFLOW.md` only; do not reintroduce `.aiops/WORKFLOW.md` or
>   `.github/WORKFLOW.md` fallback discovery.

## SPEC alignment is a hard requirement

This project is positioned as a Symphony port. **Three upstream sources are
jointly authoritative**:

1. The protocol contract: [Symphony SPEC.md](https://github.com/openai/symphony/blob/main/SPEC.md).
2. The reference implementation: [`openai/symphony` Elixir tree](https://github.com/openai/symphony/tree/main/elixir).
   When SPEC text is ambiguous, the reference's behavior is the tiebreaker.
   Pay particular attention to:
   - [`elixir/lib/symphony_elixir/orchestrator.ex`](https://github.com/openai/symphony/blob/main/elixir/lib/symphony_elixir/orchestrator.ex) — in-process GenServer state; no DB; reconcile-on-startup via tracker fetch.
   - [`elixir/lib/symphony_elixir/codex/app_server.ex`](https://github.com/openai/symphony/blob/main/elixir/lib/symphony_elixir/codex/app_server.ex) — long-running JSON-RPC 2.0 over stdio; not one-shot exec.
   - [`elixir/lib/symphony_elixir/tracker.ex`](https://github.com/openai/symphony/blob/main/elixir/lib/symphony_elixir/tracker.ex) and adapters — polling model with `:poll_interval_ms`; no webhook ingress.
   - [`elixir/lib/symphony_elixir/config/schema.ex`](https://github.com/openai/symphony/blob/main/elixir/lib/symphony_elixir/config/schema.ex) — canonical config keys, defaults, and types.
3. The authors' announcement post, mirrored locally as
   [`docs/research/2026-04-27-openai-symphony-blog.md`](docs/research/2026-04-27-openai-symphony-blog.md).
   Provides the design rationale and the SPEC §1 problem statement. Direct
   quotes to anchor on:
   - "Symphony is a scheduler/runner and tracker reader." (defines our boundary)
   - "Ticket writes (state transitions, comments, PR links) are typically
     performed by the coding agent using tools available in the workflow/runtime
     environment." (where #76 comes from)
   - "Support restart recovery without requiring a persistent database."
     (where #73 comes from)
   - "Poll the issue tracker on a fixed cadence" (where #74 comes from)
   - "Symphony continuously watches the task board and ensures that every
     active task has an agent running in the loop until it's done."
     (where #78 comes from)
   - "We use [dynamic tool calls] to expose the raw `linear_graphql` function...
     without relying on MCP or exposing the access token to containers."
     (where the token-isolation requirement in #76 comes from)

**Practitioner accounts** (advisory; not authoritative on SPEC, but useful
for matching observed Symphony behavior in real deployments and for
calibrating harness-engineering decisions):

- [`docs/research/2026-05-george-symphony-electron-rewrite.md`](docs/research/2026-05-george-symphony-electron-rewrite.md)
  — first-hand operator report of running 50 Linear tickets to 30 merged
  PRs overnight. Pins the user-visible state-machine semantics
  ("Cancel a ticket — the agent stops on the next poll") and reinforces
  that the WORKFLOW.md prompt is the leverage point, not the orchestrator
  ("Symphony is plumbing; the prompt teaches the agent how to plan,
  test, handle review feedback, and constrain scope").
- [`docs/research/2026-05-addy-osmani-harness-engineering.md`](docs/research/2026-05-addy-osmani-harness-engineering.md)
  — Addy Osmani's harness-engineering thread. Provides the
  vocabulary and principles for evaluating components (see "Harness
  engineering principles" below).

The project is **pre-release** — there are no users to migrate, so the cost of
aligning with SPEC and the reference is at its minimum **right now**. Treat
alignment as a non-negotiable goal, not a future cleanup.

## Harness engineering principles

Adopted from
[Addy Osmani's harness-engineering thread](docs/research/2026-05-addy-osmani-harness-engineering.md).
These complement (not replace) the SPEC-alignment rules above; they
govern *how* we evaluate components inside the SPEC-aligned envelope.

1. **Behavior first.** Every component must name the specific behavior
   it delivers. If you cannot state that behavior in one sentence, the
   component does not belong in the harness — remove it. This is
   exactly the test that retired the Postgres queue (#73),
   Gitea webhook (#74), and multi-path WORKFLOW.md (#72) as
   "deliberate extensions": none of them had a nameable behavior that
   SPEC alignment didn't already cover.
2. **Earned rules.** Every rule in `AGENTS.md`, `WORKFLOW.md`,
   `DEVIATIONS.md`, and the prompt template should trace back to a
   specific, observed failure. Treat the files like a pilot's checklist,
   not a style guide. When in doubt, leave a rule out until you have a
   failure that demands it. (Pre-release exception: rules derived
   directly from SPEC or the Elixir reference are earned by the
   protocol contract itself.)
3. **Failures are configuration problems.** When the agent does the
   wrong thing, the default response is to tighten the harness — add
   a hook, sharpen a tool description, tighten the prompt, narrow a
   permission — not to wait for a smarter model. The harness is a
   living artifact: every observed failure should produce a permanent
   constraint that prevents the same failure next time.
4. **Constraints have a lifecycle.** Rules added to fix a failure may
   become redundant when a later model handles the case natively.
   Periodically audit `AGENTS.md` and the prompt template; remove
   scaffolding that is no longer earning its keep, and use the
   freed-up surface to reach the next horizon.
5. **Few sharp tools beat many overlapping ones.** When the
   agent's tool surface lands (#76), aim for the smallest set of
   focused tools (`linear_graphql`, one Gitea PR tool, etc.); resist
   the temptation to wrap every Gitea / Linear endpoint as a separate
   tool.
6. **Upstream absence is an over-design signal; delete, don't relocate.**
   Before adding any worker/orchestrator phase, gate, artifact, or config
   that acts on the agent's output (verify, secret scan, run summary, diff
   policy, push, PR, tracker write, …), `grep` the Elixir reference for an
   equivalent *first*. **Upstream having no equivalent is a strong signal the
   component is over-design, not a feature gap to fill.** The default home for
   "check the agent's work before handoff" is the WORKFLOW prompt (agent-owned,
   pre-push, *preventive*), not a worker post-turn phase — a worker phase runs
   *after* the agent has already pushed (#76), so it can only flag, never
   prevent, and it races the D9 reconcile-cancel / §16.5 self-stop, which is
   exactly what caused #557. When a component is found on the wrong side of the
   boundary, the fix is to **delete it**, not relocate it (move-to-prompt) or
   merely document it (a new `DEVIATIONS.md` row); relocating or documenting
   preserves scaffolding that no longer earns its place (principle 4). Keep a
   piece only if you can name a behavior SPEC's scheduler/runner boundary
   genuinely permits *and* that the prompt cannot replicate — and confirm that
   bar against SPEC before claiming it.

   Narrow exception: upstream absence can also mean the reference is
   under-hardened, but only after live evidence proves the SPEC/upstream
   behavior itself is operationally defective. In that case, do not resurrect
   the deleted mechanism by habit and do not treat the exception as a
   keep/change/remove menu. Record the defect evidence, prove that the prompt
   or tracker-state eligibility cannot enforce the needed invariant, add or
   update a precise `DEVIATIONS.md` row, and implement the smallest
   scheduler/runner-side hardening that SPEC's boundary still permits.
   **Exception earned by:** #621 / PR #625 reproduced SPEC §7.1's unbounded clean
   continuation loop on upstream Symphony; #627 captured the process lesson;
   PR #628 accepted D34 as a narrow clean-turn budget rather than restoring the
   removed D29/D30 failure and continuation-spawn caps.

   **Default rule earned by:** this port has
   built-then-unwound the same misplacement repeatedly — Postgres queue
   (#73/#407), Gitea webhook (#74), orchestrator PR/push/tracker-writes (#76),
   and the worker verify / secret-scan / RUN_SUMMARY / policy gates (#557 /
   #561) — each a dedicated removal on top of the original build, plus the bugs
   the misplacement itself caused (the #557 reconcile-cancel race was a direct
   consequence of running verify as a worker post-turn phase). The `policy`
   path/diffstat gate looked like a legitimate keep because of its
   violation-retry loop, but SPEC §3.2 homes scope/validation rules in the
   `WORKFLOW.md` prompt, the loop fired post-push and raced reconcile-cancel,
   and upstream has no such config — so it is being removed under #561, not
   kept.
7. **Research to a verdict before proposing; bring the verdict, not a menu.**
   Before proposing how to handle a component (keep / change / remove), finish
   the SPEC + Elixir-reference research that would settle it — including what
   SPEC says the *correct* home is (e.g. §3.2 places ticket-handling,
   validation, and scope rules in the operator's `WORKFLOW.md` prompt, not a
   worker gate). When that research settles the verdict — most often that an
   extension upstream lacks and SPEC homes elsewhere should be removed — decide
   and act on it directly; do **not** hand the operator a keep / relocate /
   document multiple-choice. Such a menu is usually a symptom that the SPEC +
   reference research which would rule out "keep" was not finished; it
   re-litigates settled evidence and biases toward preserving the scaffolding.
   Reserve operator choices for genuine scope, intent, or safety forks SPEC
   leaves open. **Earned by:** the #561 `policy`-gate decision was first handed
   back as a keep-vs-remove-vs-document menu *twice*, when SPEC §3.2 + the
   post-push reconcile-cancel race + the upstream config gap already settled it
   as a removal — research that should have produced a direct verdict up front,
   before any option was offered.

## Cross-cutting checklist when porting from the Elixir reference

The SPEC-alignment rules above govern *protocol contract*. Three
classes of failure have shown up in shipped PRs anyway, because the
SPEC text does not surface them. Use this checklist in addition to
reading SPEC and the reference module — every item is tied to a
specific observed failure per the "Earned rules" principle.

1. **Audit adjacent paths for aiops-platform extensions before
   touching a SPEC code path.** Upstream Symphony is single-project;
   this port layers extensions on top of the SPEC algorithm
   (service routing via `selectRoutedCandidates`, multi-tracker
   fan-out, per-state capacity caps, reconciliation hooks).
   When you touch a path that consumes a SPEC concept (candidates,
   retries, dispatch, reconcile), `grep` the symbol and list the
   other consumers. Every aiops-platform-specific filter they apply
   must either also apply on your new path or carry a written reason
   to differ. **Earned by:** PR #287 retry-fire bypassed
   `selectRoutedCandidates` because SPEC §16.6 has no service-routing
   concept; the poll loop's filter was missed for several review
   rounds.

2. **Replicate Elixir's implicit runtime guarantees explicitly in
   Go.** BEAM gives a few things for free that the SPEC text takes
   for granted but a Go port inherits in weaker form:

   - `GenServer.call` has a default timeout → wrap every external
     I/O call from a followup goroutine in `context.WithTimeout`,
     even when the underlying client claims to honor `ctx`.
   - Process link + supervisor isolates panics → `defer
     recoverPanic(site)` on every `go func` and `time.AfterFunc`,
     or route through `safeGo` (`internal/orchestrator/recover.go`).
   - `Process.cancel_timer` is mandatory before reassigning a timer
     → store the `*time.Timer` on state and `Stop()` it before
     replacing.

   A 1:1 port from Elixir without these compensations is a latent
   stuck-state bug. **Earned by:** PR #287 retry-fire fetch had no
   timeout (upstream relied on GenServer's implicit deadline), so a
   tracker client that ignored `ctx` could orphan a claim forever;
   PR #304 had to retrofit `recoverPanic` across actor + timer
   goroutines after a panic in one path crashed the whole worker.

3. **Treat generated downstream protocol schemas as separate authorities.**
   Symphony's Elixir config schema is the reference for workflow/config shape,
   but it is not the authority for every downstream agent wire protocol. When
   touching `internal/runner` paths that speak to `codex app-server`, validate
   the request structs against the current OpenAI Codex app-server protocol
   schema for that Codex version. Do not hand-build `turn/start` payloads with
   free `map[string]any`, do not translate legacy sandbox shapes such as
   `mode: workspace-write`, and keep local schema contract tests in step with
   any Codex CLI upgrade. The contract test
   (`internal/runner/codex_app_server_schema_test.go`) validates the runner's
   actual request payloads against a vendored bundle; regenerate it only via
   `scripts/refresh-codex-schema.sh` and bump the single source of truth
   `CodexProtocolVersion` (`internal/runner/codex_version.go`) — a parity test
   fails if it, the Dockerfile `ARG CODEX_CLI_VERSION`, and the vendored schema
   filename drift. **Generate the bundle with `--experimental`:** the runner
   enables the experimental API and sends experimental request fields (e.g.
   `thread/start` `dynamicTools`), which the default `generate-json-schema`
   export strips — a non-experimental bundle silently drops them and falsely
   flags a working field as removed. **Earned by:** Codex CLI 0.133 rejected the
   old aiops-platform `turn/start` payload because `UserInput.Text` required
   `text_elements` and `SandboxPolicy` required typed variants such as
   `type: workspaceWrite`; the #446 hand-written snapshot it spawned then rotted
   into a placebo (asserting `text_elements` required at 0.133 while the runtime
   moved to 0.135/0.136), and a non-`--experimental` regen made `dynamicTools`
   look removed in 0.136 when the upstream `ThreadStartParams` struct was
   byte-identical and merely experimental-gated.

4. **Run an adversarial pass on your own diff before asking a human
   to look.** `@codex review` reads SPEC + the Elixir reference +
   this file, and surfaces precisely the gaps that human review
   tends to miss. Trigger it on the head commit before marking the
   PR ready, and for each finding either fix it or document in the
   PR body why it's deferred. Treat the agent's first draft as a
   candidate, not a delivery. **Earned by:** PR #287's HIGH +
   MEDIUM findings were both surfaced by `@codex review` after
   submit; both could have been caught one round earlier.

Rules for agents working on this repo:

1. **Read SPEC.md and the relevant Elixir module before designing any
   architectural change.** When SPEC describes the behavior of a subsystem you
   are touching (workflow file, agent runner, tracker, state machine, recovery,
   sandboxing, tools), the SPEC text is the default and the Elixir reference
   resolves any ambiguity. Deviations require a written justification.
2. **Every accepted deviation lives in [`DEVIATIONS.md`](DEVIATIONS.md).** If you
   find behavior that violates SPEC or contradicts the reference and is not
   already listed there, **do not add a new "deliberate extension" to make the
   discrepancy disappear**. File an issue with the `area:spec-alignment` label
   so the deviation is visible and tracked. The umbrella tracker is
   [#67](https://github.com/xrf9268-hue/aiops-platform/issues/67).
3. **"Has better value than SPEC" is a high bar.** Cosmetic convenience (e.g.
   "let users park a config file in a hidden directory") does not clear it.
   Things that initially look like better value but match neither SPEC nor the
   Elixir reference are usually wrong on closer inspection — see #74 (Gitea
   webhook ingress, sold as "lower latency" but ultimately reverted) for a
   recent worked example. When in doubt, default to SPEC and open an issue.
4. **Do not introduce new deviations to fix bugs.** If a SPEC-aligned design
   would make the bug easier to fix, prefer that over patching around a deviation.
5. **Observability is not a substitute for alignment during pre-release.** The
   project briefly chose to *document* a deviation rather than fix it (#69 for
   D4); that approach has since been reversed (#72) and should not be the
   default playbook again. If a deviation is wrong, fix it; do not log it.
6. **When in doubt, port from the Elixir tree.** This project is a Go port of a
   working Elixir reference. If you are unsure how a subsystem should behave,
   read the corresponding Elixir module first; the answer is usually there.

The current set of tracked deviations is in `DEVIATIONS.md` (each row carries a
status showing whether it is open, partial, reverting, or closed). Any
new SPEC-violating change you make must either (a) close an existing deviation,
(b) be tracked as a new deviation with an issue, or (c) be reverted.

## Layout

| Path | What lives there |
|------|------------------|
| `cmd/worker` | Polls trackers, dispatches eligible issues, and runs the Symphony loop; PR handoff is agent-side |
| `internal/workflow` | Loads `WORKFLOW.md` (front matter + prompt body) |
| `internal/runner` | Runner abstraction: `mock`, `codex-app-server`, `claude` |
| `internal/workspace` | Deterministic git workspace, hooks, run artifacts |
| `internal/tracker` | Tracker abstraction with Linear client |
| `internal/gitea` | Gitea tracker/client support and PR/tool helpers |
| `internal/worker` | Worker lifecycle |
| `internal/task` | Task event constants |
| `docs/adr/` | Architectural decisions (start here for "why") |
| `docs/runbooks/` | Operational guides (CI, local dev, workspace cache) |
| `test/e2e/` | Build-tagged E2E suite (`-tags e2e`) using Gitea containers |

## Build, test, lint

The CI gate is the authoritative checklist — match it locally before pushing:

```bash
gofmt -l $(git ls-files '*.go')         # must be empty
go mod tidy && git diff --exit-code -- go.mod go.sum
go vet ./...
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run --config=.golangci.yml --issues-exit-code=0
go test -run '^TestProductionGoFilesStayWithinSizeBudget$' -count=1 ./scripts
go test -race -covermode=atomic ./...
go build ./cmd/worker ./cmd/tui
```

E2E (requires Docker, pulls `gitea/gitea:1.26.1-rootless`):

```bash
go test -tags e2e -race -timeout 15m ./test/e2e/...
```

Inspect the effective `WORKFLOW.md` resolution for a workdir without consuming a task:

```bash
go run ./cmd/worker --print-config /path/to/repo/clone
```

Go toolchain: pinned via `go.mod` (Go 1.25). Don't edit `go.mod`'s `go` directive opportunistically.

## Clean code

These rules apply to every PR. The project is pre-release — the cost of doing
it right is at its minimum now. Each rule is earned by a specific observed
failure per the "Earned rules" principle above.

1. **No technical debt by default.** If a decision produces debt (duplicate
   fields, back-compat shims, dead code paths), fix it before merging or open
   a tracking issue tagged `area:tech-debt` with concrete acceptance criteria.
   Do not merge both in the same PR. **Earned by:** PR #338 dual-emitting
   `last_codex_at` + `last_event_at` and deferring removal; required a
   separate cleanup PR #342.

2. **No unnecessary backward compatibility.** Pre-release means no external
   users to protect. Remove old wire names, deprecated fields, and legacy code
   paths outright rather than aliasing or dual-emitting. If a consumer inside
   this repo uses an old name, update the consumer in the same PR. **Earned
   by:** same as rule 1 (#338 / #342).

3. **One source of truth per concept.** Never emit the same data under two
   keys or store the same value in two fields. When SPEC renames a concept
   (e.g. `last_codex_at` → `last_event_at`), rename it everywhere — struct
   field, JSON tag, dashboard, runbook, test — in a single atomic change.
   **Earned by:** PR #342 audit found that the wire rename in #338 left the
   internal `LastCodexAt` field untouched across four files, violating the
   rule the PR itself introduced.

4. **Names must match the domain.** Internal Go identifiers should mirror the
   SPEC vocabulary, not a historical implementation artefact. If SPEC says
   `last_event_at`, the struct field is `LastEventAt`, not `LastCodexAt`.
   **Earned by:** same as rule 3 (#342 audit finding HIGH).

5. **Every comment explains *why*, not *what*.** A comment that restates what
   the code already says is noise. Back-compat comments ("retained for §13.7…")
   that outlive the back-compat code are doubly harmful — delete both together.
   **Earned by:** PR #338 left a five-line comment block explaining a
   dual-field strategy that PR #342 then deleted.

6. **Tests must assert the new code path.** A test that would pass even if the
   feature were deleted is a placebo. After writing a test, verify it fails
   when the production code is broken (mutation or negative assertion). Run that
   mutation against the committed artifact, not the working tree — commit the
   fix first, then break it, restore with `git checkout`, and confirm the tree
   matches HEAD; a passing local test is not proof of the shipped commit when
   the working tree and HEAD have diverged. **Earned by:** PR #342 audit (LOW)
   noted the back-compat test asserted `last_codex_at` presence but not its
   absence after removal; and a #469/PR #483 session where a backup/restore during
   mutation testing silently reverted the fix, so local tests stayed green while
   the commit lacked it and only CI caught the regression.

7. **Function and file size budgets.** New code must keep single functions at
   or below 80 lines and single files at or below 800 lines, excluding test
   files and generated code. When porting from Elixir, split oversized upstream
   modules on the Go side instead of preserving a monolith. CI machine-enforces
   the **per-function** budgets: the blocking golangci-lint gate runs
   `funlen`/`gocognit` over all code, with the existing baseline grandfathered
   in-line via `//nolint:gocognit[,funlen] // baseline (#521)` directives on each
   known-debt function (removed as #521 decomposes it). A new oversized /
   over-complex non-test function — or in-place growth of an un-annotated one —
   fails CI; do not add a new `//nolint` to dodge the gate. CI also
   machine-enforces the ≤800-line **file** budget with an uncached
   `go test -run '^TestProductionGoFilesStayWithinSizeBudget$' -count=1 ./scripts`
   step backed by `scripts/file_size_budget_test.go`: existing oversized
   production Go files are listed with exact line-count baselines, new
   oversized production files fail, and any reduction of an existing oversized
   file must lower or remove that baseline in the same PR.
   **Earned by:** #410 found `RunTask` at 244 lines, `validateConfig` at 186
   lines, and `actor.go` at 2138 lines; PR #342 showed that one concept rename
   had to touch four files partly because large files hid the domain boundary.

8. **Errors are wrapped and classified, not string-matched.** Wrap underlying
   errors with `%w`, classify them with `errors.Is` / `errors.As`, and keep
   sentinel errors in the package that owns the failure mode. Error strings
   are lowercase unless they begin with a proper noun or acronym. Never compare
   error text with `strings.Contains`. **Earned by:** mixed `%w` / `%s` /
   `errors.New` styles made review-time reasoning about `errors.Is` /
   `errors.As` unreliable.

9. **Test failures must print the input, actual value, and expected value.**
   Prefer the Go review-comment shape `Foo(%q) = %v; want %v` over
   assertion-only text like `"expected X"`. A regression test whose failure
   message omits the actual state slows down every future investigation.
   **Earned by:** repository tests using `t.Errorf("expected ...")` without the
   actual value left reviewers guessing which state transition regressed.

10. **Fix the class, not the instance.** A bug, review comment, or CI failure
    is usually one visible member of a broader class. Before pushing, name the
    class and sweep its blast radius. When changing a shared value or contract
    (version pin granularity, renamed field, removed config, changed default),
    `grep` every consumer, derivation, fixture, doc, and CI copy and update them
    atomically. When replacing a mechanism (assertion, gate, guard, retry), list
    the invariants the old mechanism guaranteed and prove the new one preserves
    each required invariant, including failure and deadline paths. Do not let
    `@codex review` discover siblings one push at a time. **Earned by:** a
    standard-library security bump first updated only the module patch pin, so
    later review rounds found the matching container base-image pin, lint-cache
    exposure, and version-doctor granularity one by one; and #602's test
    refactor replaced an external deadline bound with a typed-error assertion
    that initially dropped the "did not wait for the outer deadline" invariant.

## Conventions

- **gofmt is non-negotiable**: CI fails on any diff. Always run before committing. A PostToolUse hook (`.claude/scripts/format-go.sh`) auto-runs `gofmt -w` on `.go` files edited via the Edit/Write tools; files changed through Bash (e.g. `sed`, heredocs) are not covered, so always verify with `gofmt -l` before pushing — CI's `gofmt -l` gate is the backstop.
- **`go mod tidy` must leave `go.mod`/`go.sum` clean**: don't add deps you don't use.
- **All external I/O is timeout-bounded**: every function that talks to a
  tracker, repository service, agent runner, subprocess, or network API must
  enforce a per-request deadline with `context.WithTimeout`, even if the
  underlying client claims to honor `ctx`. This applies to new code, not only
  Elixir ports. **Earned by:** #287 retry-fire fetch had no timeout on an
  Elixir-port path, and #405 found the same failure class in the non-port Gitea
  tracker client.
- **Goroutine lifetimes are explicit**: every `go func` / `time.AfterFunc`
  outside of `package main` boot code must use `safeGo` or install
  `defer recoverPanic("site")`, and the code must make the exit condition
  obvious. `package main` boot goroutines that intentionally share process
  lifetime are exempt, but the call site must say so. Direct async launch
  without panic recovery or a clear stop condition is a stuck-state bug, not
  style drift. **Earned by:** PR #304 retrofitted panic recovery after one path
  could crash the worker; #413's audit found most production goroutine launch
  sites still lacked that guard, so this needs a machine-checkable follow-up
  rather than reviewer memory.
- **Prefer the `gh` CLI over the GitHub MCP server** for GitHub interactions (PRs, issues, CI status, reviews). The SessionStart hook installs `gh` in remote/cloud/web sessions (`.claude/scripts/session-start.sh`); fall back to the GitHub MCP server only when `gh` is unavailable.
- **Task events**: when adding a new lifecycle event, add the kind as a constant in `internal/task` rather than inlining the string at the call site.
- **Secrets**: never commit real credentials. `.env`, `.env.*`, `*.key`, `*.pem` are gitignored; `.env.example` is the only sanctioned env template. Secret-bearing values that arrive via config or CLI (e.g. `clone_url` basic-auth userinfo) must be masked before they reach any log, error string, or state output — env-var-only redaction does not cover them. Mask clone URLs with `workflow.MaskCloneURL`. **Earned by:** #469/PR #483, where a doctor ambiguity/not-found error echoed a credentialed `clone_url` because `redact()` only scrubbed env-var values.
- **Keep worker PRs small — ≤12 changed production files / ≤300 changed
  production LOC is a review guideline, not an LOC-reduction mandate.** Test
  files and generated code are excluded from the count (matching the
  function/file budgets in the Clean-code rules), so test coverage never by
  itself pushes a PR into overage. (These were the
  `policy.max_changed_files` / `policy.max_changed_loc` worker caps; the worker
  no longer enforces them — the path/diffstat gate was removed in #561 because
  it ran post-push and raced reconcile-cancel — so the budget is now a review
  discipline, not config.) Worker PRs are draft + labeled by
  default; shape them small when you can, but the budget exists to catch scope
  creep and force explicit handling — not to incentivize deleting necessary
  tests, weakening state-machine coverage, skipping race coverage, or
  preferring compact code over clear reliable code when review feedback
  exposes a real correctness, safety, performance, or coverage gap. Classify
  every PR into exactly one of three states and surface it in the PR body:
  - `within budget` — production diff fits the ~12-file / ~300-LOC guideline (tests and generated code excluded).
  - `size-gated: justified overage` — over the budget because the extra LOC
    pays for correctness, regression coverage, race/state-machine safety, or
    other best-practice hardening that cannot be split without losing
    atomicity. Requires explicit human size-gate sign-off before merge.
  - `size-gated: split recommended` — over the budget because of scope creep,
    unrelated cleanup, or genuinely separable concerns. Stop and split into
    smaller PRs instead of asking for sign-off.

  Only reduce LOC when the code is genuinely redundant, over-abstracted,
  duplicated without purpose, or outside scope. Never delete meaningful tests
  or safety checks solely to satisfy the budget. **Earned by:** PR #455
  exceeded the default 300 LOC after multiple valid Codex review findings
  required additional race/state-machine coverage; the prevailing workflow
  language nudged the agent toward compressing tests to fit the threshold,
  which is backwards when the extra lines are paying for correctness. Counting
  production LOC only (tests/generated excluded) removes that pressure at the
  source, so a well-tested focused change stays `within budget` instead of
  defaulting to a sign-off-requiring overage.
- **Merged PR review feedback is captured non-blockingly**: `.github/workflows/capture-unresolved-reviews.yml` scans merged PRs for unresolved, non-outdated review discussions and files follow-up GitHub issues keyed by the discussion permalink. This is the only post-merge line of defense for shipped-past bot feedback, not a required merge check; agents should still handle actionable review feedback before merging. After merging a PR with non-trivial bot review activity, sanity-check the next-day `Capture unresolved reviews` Actions history so workflow regressions do not age silently.
- **SPEC deviations are gated at author time, not audit time**: the `PR Metadata`
  workflow (`.github/workflows/pr-metadata.yml` + `.github/scripts/validate-pr-metadata.mjs`)
  blocks a PR that changes a SPEC-sensitive path (`internal/workflow/config.go`, a
  newly-added `internal/orchestrator/`/`internal/worker/` file) while it claims no
  new key/phase — the author must cite an upstream Elixir reference or track a
  `DEVIATIONS.md` row (principle 6/7). Fill the `SPEC alignment` checklist in the
  PR template. This makes a documented deviation cost something *before* merge
  instead of being unwound later (the #73/#74/#76/#557/#561/D25 recurrence). The
  required-check wiring lives in `.github/governance/main-ruleset.json`. **Earned
  by:** #588 — those removals all shipped despite the rules existing, because the
  checks were judgment at audit time rather than mechanical at author time.

## WORKFLOW.md discovery (worker side)

Per SPEC §workflow file, `WORKFLOW.md` is a single service workflow source at
the startup-selected path. By default this is the process working directory's
canonical file:

```
WORKFLOW.md
```

Missing front matter is allowed — the body becomes the prompt template, all
other settings fall back to defaults (see `README.md` table). The
`workflow_resolved` task event captures the resolved source and path.

Legacy alternate paths (`.aiops/WORKFLOW.md`, `.github/WORKFLOW.md`) are not
searched or reported as normal shadow sources.

## Where to read next

- `README.md` — user-facing quick start and current component overview
- `docs/runbooks/local-dev.md` — local dev loop
- `docs/runbooks/codex-app-server-docker.md` — production-style Docker notes for real `codex app-server`
- `docs/runbooks/ci.md` — CI behavior, release flow, pre-push checks
- `docs/runbooks/workspace-cache.md` — workspace lifecycle and cleanup
- `docs/runbooks/batch-issue-processing.md` — running a `/goal` over a set of issues: one-issue-per-PR, parallelism, deferral protocol, and the authorized auto-merge flow
- `docs/adr/0001-symphony-style-personal-orchestrator.md` — the "why"

## Safety posture for agents

- The agent opens PRs through its workflow/tool surface; the worker must not
  push, open, merge, or otherwise manage PR handoff on the agent's behalf.
- Keep first-time real runs on `agent.default: mock` until the loop is trusted on the target repo.
- Use low-privilege bot accounts for Gitea / Linear / GitHub tokens.
- When in doubt about scope, prefer a narrower change and a clear PR description over speculative refactors.

<!-- TRELLIS:START -->
# Trellis Instructions

These instructions are for AI assistants working in this project.

This project is managed by Trellis. The working knowledge you need lives under `.trellis/`:

- `.trellis/workflow.md` — development phases, when to create tasks, skill routing
- `.trellis/spec/` — package- and layer-scoped coding guidelines (read before writing code in a given layer)
- `.trellis/workspace/` — per-developer journals and session traces
- `.trellis/tasks/` — active and archived tasks (PRDs, research, jsonl context)

If a Trellis command is available on your platform (e.g. `/trellis:finish-work`, `/trellis:continue`), prefer it over manual steps. Not every platform exposes every command.

If you're using Codex or another agent-capable tool, additional project-scoped helpers may live in:
- `.agents/skills/` — reusable Trellis skills
- `.codex/agents/` — optional custom subagents

Managed by Trellis. Edits outside this block are preserved; edits inside may be overwritten by a future `trellis update`.

<!-- TRELLIS:END -->

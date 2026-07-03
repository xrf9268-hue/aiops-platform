# AGENTS.md

Project guide for AI coding agents (Codex CLI, Claude Code, etc.) working in this repo.

**This file is the single source of truth for all engineering rules.** Other
coding agents read it via thin bridge files: `CLAUDE.md` imports it with
`@AGENTS.md` for Claude Code. Do not duplicate content in the bridge files —
update only this file.

Each rule is kept to a scannable imperative; the *provenance* (the observed
failure that earned it, per [harness principle 2](#harness-engineering-principles))
lives in [`docs/engineering-rules-rationale.md`](docs/engineering-rules-rationale.md),
linked per section (and per rule where useful) as `(provenance)`. When you add or
change a rule here, update its rationale entry there too.

## Index

- [What this project is](#what-this-project-is) — scope, the scheduler/runner boundary, transitional reverts
- [SPEC alignment is a hard requirement](#spec-alignment-is-a-hard-requirement) — the authoritative sources
- [Harness engineering principles](#harness-engineering-principles) — how to evaluate components (1–7)
- [Cross-cutting checklist](#cross-cutting-checklist-when-porting-from-the-elixir-reference) — Elixir-port failure classes + rules for agents
- [Layout](#layout) · [Build, test, lint](#build-test-lint)
- [Clean code](#clean-code) — per-PR rules (1–11)
- [Conventions](#conventions) — gofmt, timeouts, goroutines, secrets, PR size, gates
- [WORKFLOW.md discovery](#workflowmd-discovery-worker-side) · [Where to read next](#where-to-read-next) · [Safety posture](#safety-posture-for-agents)
- [`DEVIATIONS.md`](DEVIATIONS.md) — tracked SPEC deviations · [`docs/engineering-rules-rationale.md`](docs/engineering-rules-rationale.md) — rule provenance

## What this project is

`aiops-platform` is a Go-based, self-hostable AI coding orchestrator that
implements the [OpenAI Symphony SPEC](docs/research/SPEC.md).
The orchestrator polls a tracker (Linear, Gitea, or GitHub), prepares a
deterministic per-issue workspace, runs a coding agent in that workspace, and
watches the agent's lifecycle. Per SPEC §1, the **agent** is what writes
tickets, opens PRs, and pushes branches — through tooling available in its
workflow/runtime: the orchestrator-advertised dynamic tools `linear_graphql`
(Linear) and `gitea_issue_labels` (Gitea), or `gh`/local workflow tooling for
GitHub (which has no orchestrator-advertised tool). The orchestrator is the
scheduler/runner and tracker *reader*, not a tracker writer.

The Go module path is `github.com/xrf9268-hue/aiops-platform` — keep it as-is even if the GitHub repo is temporarily mirrored elsewhere.

> **Transitional notes** — several pieces of earlier implementations deviated
> from this SPEC-aligned picture and have since been reverted. Do not design new
> code that depends on the legacy behavior, and do not reintroduce it:
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
>   closed under #72; do not reintroduce `.aiops/WORKFLOW.md` or
>   `.github/WORKFLOW.md` fallback discovery. The canonical-root-only behavior
>   is documented once in [WORKFLOW.md discovery](#workflowmd-discovery-worker-side).

## SPEC alignment is a hard requirement

This project is positioned as a Symphony port. **Three upstream sources are
jointly authoritative**:

1. The protocol contract: [Symphony SPEC.md](docs/research/SPEC.md) — mirrored
   verbatim from
   [upstream](https://github.com/openai/symphony/blob/main/SPEC.md) so the
   contract this port targets cannot drift (upstream is an unmaintained demo
   repo); re-sync by bumping the commit hash in the mirror's header (#799).
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
- [`docs/research/2026-06-16-langchain-art-of-loop-engineering.md`](docs/research/2026-06-16-langchain-art-of-loop-engineering.md)
  — LangChain's four-level loop-engineering frame. Useful for naming
  aiops-platform's supported agent, verification, and event-driven loops,
  and for keeping the trace-driven hill-climbing loop as an explicit
  follow-up rather than an implied worker-side verifier.
- [`docs/research/2026-06-16-zach-lloyd-self-improving-skills.md`](docs/research/2026-06-16-zach-lloyd-self-improving-skills.md)
  — Zach Lloyd's Warp/Oz practitioner account for self-improving
  file-based Skills. Useful for shaping the planned outer improvement loop:
  run evidence plus feedback should produce ordinary Skill/workflow/rubric
  diffs, not worker-owned post-turn mutation.

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
   focused tools (`linear_graphql`, `gitea_issue_labels`, etc.); resist
   the temptation to wrap every Gitea / Linear endpoint as a separate
   tool.
6. **Upstream absence is an over-design signal; delete, don't relocate.**
   Before adding any worker/orchestrator phase, gate, artifact, or config that
   acts on the agent's output (verify, secret scan, run summary, diff policy,
   push, PR, tracker write, …), `grep` the Elixir reference for an equivalent
   *first*. Upstream having no equivalent is a strong signal it is over-design,
   not a feature gap. The default home for "check the agent's work before
   handoff" is the WORKFLOW prompt (agent-owned, pre-push, *preventive*), not a
   worker post-turn phase (which runs after the push and races
   reconcile-cancel — #557). When a component is on the wrong side of the
   boundary, **delete it** — don't relocate or merely document it. *Narrow
   exception:* if live evidence proves the SPEC/upstream behavior itself is
   defective, record the evidence, prove the prompt/tracker-state can't enforce
   the invariant, add a `DEVIATIONS.md` row, and add the smallest
   boundary-permitted hardening (e.g. D34). ([provenance](docs/engineering-rules-rationale.md#harness-principles))
7. **Research to a verdict before proposing; bring the verdict, not a menu.**
   Finish the SPEC + Elixir-reference research that settles "keep / change /
   remove" — including what SPEC says the *correct* home is — then decide and
   act; don't hand the operator a multiple-choice menu (usually a sign the
   research that would rule out "keep" wasn't finished). Reserve operator
   choices for genuine scope/intent/safety forks SPEC leaves open.
   ([provenance](docs/engineering-rules-rationale.md#harness-principles))

## Cross-cutting checklist when porting from the Elixir reference

The SPEC-alignment rules above govern *protocol contract*. Four
classes of failure have shown up in shipped PRs anyway, because the
SPEC text does not surface them. Use this checklist in addition to
reading SPEC and the reference module — every item is tied to a
specific observed failure per the "Earned rules" principle.

1. **Audit adjacent paths for aiops-platform extensions before
   touching a SPEC code path.** Upstream Symphony is single-project;
   this port layers extensions on top of the SPEC algorithm
   (per-state capacity caps via `UpdateMaxConcurrentAgentsByState`,
   the eligibility/required-labels gates in `filterEligibleCandidates`,
   dispatch revalidation via `revalidateDispatchCandidates`,
   reconciliation hooks).
   When you touch a path that consumes a SPEC concept (candidates,
   retries, dispatch, reconcile), `grep` the symbol and list the
   other consumers. Every aiops-platform-specific filter they apply
   must either also apply on your new path or carry a written reason
   to differ. ([provenance](docs/engineering-rules-rationale.md#cross-cutting-checklist))

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
   - Elixir's `Port` drains a subprocess's stdout/stderr for you → a
     Go `exec.Cmd` owner must drain each pipe to EOF *before*
     `cmd.Wait`. `cmd.Wait` closing the pipe is **not** draining: a
     child blocked writing to a full OS pipe never exits, so `Wait`
     never returns and a post-`Wait` drain is never reached. The
     goroutine that owns the pipe must keep reading until EOF, and the
     shutdown path must drain (consume the channel) before `Wait`.

   A 1:1 port from Elixir without these compensations is a latent
   stuck-state bug. ([provenance](docs/engineering-rules-rationale.md#cross-cutting-checklist))

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
   flags a working field as removed. ([provenance](docs/engineering-rules-rationale.md#cross-cutting-checklist))

4. **Preserve local Codex environment inheritance for binary deployments.**
   The upstream Elixir app-server starts `codex.command` from the issue
   workspace and naturally inherits the user environment of the running
   orchestrator. A local aiops-platform binary should preserve that expectation:
   when the worker and `codex app-server` run as the same user, Codex may reuse
   the same `HOME` / `CODEX_HOME`, repo/user/admin/system skills, MCP config,
   operator-enabled Apps/connectors, plugin-provided capability, and shell-tool
   environment according to Codex's own discovery/config rules. Do not disable
   host-local skills/MCP/Apps/connectors by default or drop `CODEX_HOME` from the
   Codex app-server subprocess environment. Repo-owned required skill/MCP
   declarations are portability and diagnostics hardening for containers, remote
   hosts, or explicitly declared dependencies — not a replacement for the local
   binary's inherited Codex environment.
   ([provenance](docs/engineering-rules-rationale.md#cross-cutting-checklist))

5. **Run an adversarial pass on your own diff before asking a human
   to look.** `@codex review` reads SPEC + the Elixir reference +
   this file, and surfaces precisely the gaps that human review
   tends to miss. Trigger it on the head commit before marking the
   PR ready, and for each finding either fix it or document in the
   PR body why it's deferred. Treat the agent's first draft as a
   candidate, not a delivery. An independent adversarial pass catches
   what thorough first-party review does not — run it even when your
   own review found nothing. ([provenance](docs/engineering-rules-rationale.md#cross-cutting-checklist))

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
| `internal/tracker` | Tracker abstraction with Linear and GitHub clients |
| `internal/gitea` | Gitea tracker client and label-state/config helpers for the agent tool surface |
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

The standalone `go vet ./...` line is backed by the CI `Security and
supply-chain` job. The golangci-lint gate also enables the `govet` analyzer,
but that analyzer is an additional lint pass, not a replacement for the
standalone `go vet ./...` step.

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

These rules apply to every PR. Each is earned by a specific observed failure —
[provenance & worked examples](docs/engineering-rules-rationale.md#clean-code).

1. **No technical debt by default.** Fix debt (duplicate fields, back-compat
   shims, dead code) before merging, or open an `area:tech-debt` tracking issue
   with acceptance criteria — never merge both in the same PR.
2. **No unnecessary backward compatibility.** Pre-release: remove old wire
   names / deprecated fields / legacy paths outright rather than aliasing or
   dual-emitting; update in-repo consumers in the same PR.
3. **One source of truth per concept.** Never emit the same data under two keys
   or store the same value in two fields; rename a concept everywhere (struct
   field, JSON tag, dashboard, runbook, test) in one atomic change.
4. **Names must match the domain.** Internal Go identifiers mirror SPEC
   vocabulary, not historical artefacts (SPEC `last_event_at` → `LastEventAt`).
5. **Every comment explains *why*, not *what*.** Delete comments that restate
   the code; delete back-compat comments together with the back-compat code.
6. **Tests must assert the new code path.** A test that passes with the feature
   deleted is a placebo — mutation-verify each new behavior against the
   **committed artifact** (commit the fix, break it, `git checkout` restore,
   confirm tree == HEAD; never `git checkout` a file with other uncommitted
   edits).
7. **Function and file size budgets.** ≤80 lines/function, ≤800 lines/file
   (excl. tests/generated) — a repo-specific maintainability budget, not an
   official Go file-length limit. CI machine-enforces both: the golangci-lint
   gate (`funlen`/`gocognit`, baseline grandfathered via
   `//nolint:… // baseline (#521)`; do **not** add new nolints to dodge it) and
   `TestProductionGoFilesStayWithinSizeBudget` (`scripts/file_size_budget_test.go`),
   which counts raw physical lines in non-test, non-generated Go files and uses
   the baseline/ratchet to burn down debt — new oversized files fail; a reduction
   must lower/remove the baseline in the same PR. Decompose oversized files by
   responsibility first; introduce `internal` helper packages only when a cohesive
   boundary exists, and don't add public API solely to satisfy the budget.
   ([provenance](docs/engineering-rules-rationale.md#clean-code))
8. **Errors are wrapped and classified, not string-matched.** Wrap with `%w`,
   classify with `errors.Is`/`errors.As`, keep sentinels in the owning package;
   lowercase error strings (unless a proper noun/acronym); never
   `strings.Contains` on error text.
9. **Test failures print input, actual, and expected.** Use
   `Foo(%q) = %v; want %v`, not assertion-only text.
10. **Fix the class, not the instance.** Name the class and sweep its blast
    radius before pushing; when changing a shared value/contract, `grep` every
    consumer/derivation/fixture/doc/CI copy and update atomically; when replacing
    a mechanism, prove the new one preserves each invariant (incl.
    failure/deadline paths). Don't let `@codex review` find siblings one push at
    a time.
11. **Mutation-test the wiring seam, not just the leaf.** When a field is
    threaded through a construction seam (config → snapshot → lister/closure, or
    view → DTO), drive the real construction path and mutation-verify by deleting
    the field *at the seam* — a leaf predicate can be correct while production
    wiring drops it. Prefer a compiling mutation (`&&`→`||`, drop the assignment,
    flip `omitempty`) so the test fails on an assertion, not a build error.
    Collapse two builders of one concept to a single source of truth (rule 3).

## Conventions

- **gofmt is non-negotiable**: CI fails on any diff. Always run before committing. A PostToolUse hook (`.claude/scripts/format-go.sh`) auto-runs `gofmt -w` on `.go` files edited via the Edit/Write tools; files changed through Bash (e.g. `sed`, heredocs) are not covered, so always verify with `gofmt -l` before pushing — CI's `gofmt -l` gate is the backstop.
- **`go mod tidy` must leave `go.mod`/`go.sum` clean**: don't add deps you don't use.
- **All external I/O is timeout-bounded**: every function that talks to a
  tracker, repository service, agent runner, subprocess, or network API must
  enforce a per-request deadline with `context.WithTimeout`, even if the
  underlying client claims to honor `ctx`. Applies to new code, not only Elixir
  ports. ([provenance](docs/engineering-rules-rationale.md#conventions))
- **Goroutine lifetimes are explicit**: every `go func` / `time.AfterFunc`
  outside `package main` boot code must use `safeGo` or `defer recoverPanic("site")`
  with an obvious exit condition. Direct async launch without panic recovery or a
  clear stop condition is a stuck-state bug.
  ([provenance](docs/engineering-rules-rationale.md#conventions))
- **Prefer the `gh` CLI over the GitHub MCP server** for GitHub interactions (PRs, issues, CI status, reviews). The SessionStart hook installs `gh` in remote/cloud/web sessions (`.claude/scripts/session-start.sh`); fall back to the GitHub MCP server only when `gh` is unavailable.
- **Task events**: when adding a new lifecycle event, add the kind as a constant in `internal/task` rather than inlining the string at the call site.
- **Secrets**: never commit real credentials. `.env`, `.env.*`, `*.key`, `*.pem` are gitignored; `.env.example` is the only sanctioned env template. Secret-bearing values that arrive via config or CLI (e.g. `clone_url` basic-auth userinfo) must be masked before they reach any log, error string, or state output — env-var-only redaction does not cover them. Mask clone URLs with `workflow.MaskCloneURL`. ([provenance](docs/engineering-rules-rationale.md#conventions))
- **Keep worker PRs small — ≤12 changed production files / ≤300 changed
  production LOC is a review guideline, not an LOC-reduction mandate.** Test
  files and generated code are excluded from the count, so test coverage never
  by itself pushes a PR into overage. Never delete meaningful tests or safety
  checks to fit the budget. Classify every PR into exactly one state in the PR
  body:
  - `within budget` — production diff fits the ~12-file / ~300-LOC guideline.
  - `size-gated: justified overage` — over budget for correctness / regression /
    race-safety coverage that can't be split without losing atomicity. **Needs
    explicit human size-gate sign-off before merge.**
  - `size-gated: split recommended` — over budget for scope creep / separable
    concerns. Split instead.

  ([provenance & rationale](docs/engineering-rules-rationale.md#conventions))
- **Merged PR review feedback is captured non-blockingly**: `.github/workflows/capture-unresolved-reviews.yml` files follow-up issues for unresolved, non-outdated review discussions on merged PRs. This is a post-merge backstop, not a required check; still handle actionable review feedback before merging, and sanity-check the next-day `Capture unresolved reviews` Actions history after a PR with non-trivial bot activity.
- **SPEC deviations are gated at author time, not audit time**: the `PR Metadata`
  workflow (`.github/workflows/pr-metadata.yml` + `.github/scripts/validate-pr-metadata.mjs`)
  blocks a PR that changes a SPEC-sensitive path (`internal/workflow/config.go`, a
  newly-added `internal/orchestrator/`/`internal/worker/` file) while it claims no
  new key/phase — cite an upstream Elixir reference or track a `DEVIATIONS.md` row
  (principle 6/7). It also requires every PR body to carry a `Closes #N` keyword.
  Fill the `SPEC alignment` checklist in the PR template; required-check wiring is
  in `.github/governance/main-ruleset.json`. PRs authored by Dependabot or the
  release-please App are exempt (they cannot write a closing keyword or the
  checklist and never author SPEC deviations); the exemption list lives in
  `validate-pr-metadata.mjs`.
  ([provenance](docs/engineering-rules-rationale.md#conventions))
- **PR titles are Conventional Commits — release-please parses them.** `main`
  merges squash-only (`.github/governance/main-ruleset.json`) with the squash
  subject set to the PR title (`squash_merge_commit_title: PR_TITLE`), and
  release-please reads that subject to build `CHANGELOG.md` and pick the version
  bump. A title whose type is not a recognized Conventional Commit type is
  **dropped silently** — no changelog line, no bump — so mistyped work vanishes
  from releases (this is what dropped the `feat`s #828/#834 and the `fix` #818
  from v0.1.3 — their `cmd:`/`dashboard:`/`hardening:` titles predated this
  check, so they had to be hand-backfilled into the changelog in #851; the
  `build`-class `release:` PRs #827/#829 were correctly hidden, not dropped).
  Title every PR `type(optional-scope): summary` using
  exactly these types; the `Validate PR title (Conventional Commits)` required
  check (`.github/workflows/pr-title-lint.yml`) enforces it:

  | Type | Use for | CHANGELOG |
  |------|---------|-----------|
  | `feat` | a new user-facing capability | ✅ Features (bumps) |
  | `fix` | a bug fix | ✅ Bug Fixes (bumps) |
  | `perf` | a performance change with no behavior change | ✅ Performance Improvements |
  | `revert` | revert of a prior commit | ✅ Reverts |
  | `refactor` | code change, no behavior change (e.g. file decomposition) | hidden |
  | `docs` | docs only (README, runbooks, this file) | hidden |
  | `style` | formatting / non-semantic code style | hidden |
  | `test` | tests only | hidden |
  | `build` | build system / release packaging / build deps | hidden |
  | `ci` | CI, workflows, governance rulesets | hidden |
  | `chore` | housekeeping and version bumps | hidden |

  Only `feat`/`fix`/breaking move the version (`feat`→minor, `fix`→patch,
  breaking→major; pre-1.0 the `bump-*-pre-major` flags downshift these to
  patch/patch/minor). A `type!:` or `BREAKING CHANGE:` footer marks a breaking
  change and surfaces even for an otherwise-hidden type. Don't invent types
  (`maintainability:`, `cmd:`, `deps:` are all dropped) — a refactor is
  `refactor:`, a dependency bump is `chore(deps):`/`build(deps):`.
  ([provenance](docs/engineering-rules-rationale.md#conventions))

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

# Engineering-rules rationale (the "earned by" archive)

This file holds the **provenance** for the rules in [`AGENTS.md`](../AGENTS.md):
the specific observed failures that earned each rule, plus the longer worked
examples. It exists so `AGENTS.md` can stay a scannable list of imperatives while
the "earned rules" discipline (every rule traces to a real failure — see
[harness principle 2](../AGENTS.md#harness-engineering-principles)) is preserved
in full. Each `AGENTS.md` rule links here via a `(provenance)` anchor.

Relocating provenance must never *drop* it. When a rule is added or changed in
`AGENTS.md`, add or update its entry here too.

## Harness principles

### Principle 1 — behavior first
The behavior-first test is exactly what retired the Postgres queue (#73), the
Gitea webhook (#74), and multi-path WORKFLOW.md discovery (#72) as "deliberate
extensions": none had a nameable behavior that SPEC alignment didn't already
cover.

### Principle 6 — upstream absence is an over-design signal
Before adding any worker/orchestrator phase, gate, artifact, or config that acts
on the agent's output (verify, secret scan, run summary, diff policy, push, PR,
tracker write, …), `grep` the Elixir reference for an equivalent *first*.
**Upstream having no equivalent is a strong signal the component is over-design,
not a feature gap to fill.** The default home for "check the agent's work before
handoff" is the WORKFLOW prompt (agent-owned, pre-push, *preventive*), not a
worker post-turn phase — a worker phase runs *after* the agent has already pushed
(#76), so it can only flag, never prevent, and it races the D9 reconcile-cancel /
§16.5 self-stop, which is exactly what caused #557. When a component is found on
the wrong side of the boundary, the fix is to **delete it**, not relocate it
(move-to-prompt) or merely document it (a new `DEVIATIONS.md` row); relocating or
documenting preserves scaffolding that no longer earns its place (principle 4).
Keep a piece only if you can name a behavior SPEC's scheduler/runner boundary
genuinely permits *and* that the prompt cannot replicate — and confirm that bar
against SPEC before claiming it.

**Narrow exception:** upstream absence can also mean the reference is
under-hardened, but only after live evidence proves the SPEC/upstream behavior
itself is operationally defective. In that case, do not resurrect the deleted
mechanism by habit and do not treat the exception as a keep/change/remove menu.
Record the defect evidence, prove that the prompt or tracker-state eligibility
cannot enforce the needed invariant, add or update a precise `DEVIATIONS.md` row,
and implement the smallest scheduler/runner-side hardening that SPEC's boundary
still permits.

**Exception earned by:** #621 / PR #625 reproduced SPEC §7.1's unbounded clean
continuation loop on upstream Symphony; #627 captured the process lesson; PR #628
accepted D34 as a narrow clean-turn budget rather than restoring the removed
D29/D30 failure and continuation-spawn caps.

**Default rule earned by:** this port has built-then-unwound the same
misplacement repeatedly — Postgres queue (#73/#407), Gitea webhook (#74),
orchestrator PR/push/tracker-writes (#76), and the worker verify / secret-scan /
RUN_SUMMARY / policy gates (#557 / #561) — each a dedicated removal on top of the
original build, plus the bugs the misplacement itself caused (the #557
reconcile-cancel race was a direct consequence of running verify as a worker
post-turn phase). The `policy` path/diffstat gate looked like a legitimate keep
because of its violation-retry loop, but SPEC §3.2 homes scope/validation rules
in the `WORKFLOW.md` prompt, the loop fired post-push and raced reconcile-cancel,
and upstream has no such config — so it is being removed under #561, not kept.

### Principle 7 — research to a verdict before proposing
Before proposing how to handle a component (keep / change / remove), finish the
SPEC + Elixir-reference research that would settle it — including what SPEC says
the *correct* home is (e.g. §3.2 places ticket-handling, validation, and scope
rules in the operator's `WORKFLOW.md` prompt, not a worker gate). When that
research settles the verdict — most often that an extension upstream lacks and
SPEC homes elsewhere should be removed — decide and act on it directly; do **not**
hand the operator a keep / relocate / document multiple-choice. Such a menu is
usually a symptom that the SPEC + reference research which would rule out "keep"
was not finished; it re-litigates settled evidence and biases toward preserving
the scaffolding. Reserve operator choices for genuine scope, intent, or safety
forks SPEC leaves open. **Earned by:** the #561 `policy`-gate decision was first
handed back as a keep-vs-remove-vs-document menu *twice*, when SPEC §3.2 + the
post-push reconcile-cancel race + the upstream config gap already settled it as a
removal — research that should have produced a direct verdict up front, before
any option was offered.

### Principle 8 — run the deletion test before expanding scope
During #1117, a review finding about macOS session shutdown grew into a proposed
private Darwin `libproc` / Mach / audit-token adapter of roughly 300
implementation lines, roughly 200 test lines, and six touched files. The code
could improve only a future run: the one-shot historical validation had already
finished, so adding the subsystem after the fact could neither repair its
recorded evidence nor change its `keep disabled pending a named defect` verdict.

The deletion test showed that cross-platform capability was optional while
shutdown safety was not. Removing the Darwin subsystem preserved required
evidence and safety for every remaining in-scope execution because the published
supervisor could reject unsupported hosts before mutating state or spawning
workers, retain the existing Linux pidfd + `/proc` path, and require a fresh
Linux/Docker run for new shutdown evidence. This was a scope-control failure,
not a line-count failure: implementation depth had outrun the issue's ability to
benefit from it. The earned rule is therefore to delete only when the remaining
supported surface stays evidentially sound and safe, and to move optional
expansion into a separately scoped issue instead of letting one PR take control
of the project.

## Cross-cutting checklist

### Item 1 — audit adjacent paths before touching a SPEC code path
**Earned by:** PR #287 retry-fire bypassed `selectRoutedCandidates` because SPEC
§16.6 has no service-routing concept; the poll loop's filter was missed for
several review rounds.

### Item 2 — replicate Elixir's implicit runtime guarantees explicitly in Go
BEAM gives a few things for free that the SPEC text takes for granted but a Go
port inherits in weaker form:

- `GenServer.call` has a default timeout → wrap every external I/O call from a
  followup goroutine in `context.WithTimeout`, even when the underlying client
  claims to honor `ctx`.
- Process link + supervisor isolates panics → `defer recoverPanic(site)` on every
  `go func` and `time.AfterFunc`, or route through `safeGo`
  (`internal/orchestrator/recover.go`).
- `Process.cancel_timer` is mandatory before reassigning a timer → store the
  `*time.Timer` on state and `Stop()` it before replacing.
- Elixir's `Port` drains a subprocess's stdout/stderr for you → a Go `exec.Cmd`
  owner must drain each pipe to EOF *before* `cmd.Wait`. `cmd.Wait` closing the
  pipe is **not** draining: a child blocked writing to a full OS pipe never
  exits, so `Wait` never returns and a post-`Wait` drain is never reached. The
  goroutine that owns the pipe must keep reading until EOF, and the shutdown path
  must drain (consume the channel) before `Wait`.

A 1:1 port from Elixir without these compensations is a latent stuck-state bug.
**Earned by:** PR #287 retry-fire fetch had no timeout (upstream relied on
GenServer's implicit deadline), so a tracker client that ignored `ctx` could
orphan a claim forever; PR #304 had to retrofit `recoverPanic` across actor +
timer goroutines after a panic in one path crashed the whole worker; and PR #688
(#666) drained the codex app-server stdout reader's channel *after* `cmd.Wait` —
a latent deadlock whenever the child emits output after the consumer stops reading,
fixed by draining to EOF before `Wait` (the `os/exec` `StdoutPipe` idiom).

### Item 3 — treat generated downstream protocol schemas as separate authorities
When touching `internal/runner` paths that speak to `codex app-server`, validate
the request structs against the current OpenAI Codex app-server protocol schema
for that Codex version. Do not hand-build `turn/start` payloads with free
`map[string]any`, do not translate legacy sandbox shapes such as
`mode: workspace-write`, and keep local schema contract tests in step with any
Codex CLI upgrade. The contract test
(`internal/runner/codex_app_server_schema_test.go`) validates the runner's actual
request payloads against a vendored bundle; regenerate it only via
`scripts/refresh-codex-schema.sh` and bump the single source of truth
`CodexProtocolVersion` (`internal/runner/codex_version.go`) — a parity test fails
if it, the Dockerfile `ARG CODEX_CLI_VERSION`, and the vendored schema filename
drift. **Generate the bundle with `--experimental`:** the runner enables the
experimental API and sends experimental request fields (e.g. `thread/start`
`dynamicTools`), which the default `generate-json-schema` export strips — a
non-experimental bundle silently drops them and falsely flags a working field as
removed. **Earned by:** Codex CLI 0.133 rejected the old aiops-platform
`turn/start` payload because `UserInput.Text` required `text_elements` and
`SandboxPolicy` required typed variants such as `type: workspaceWrite`; the #446
hand-written snapshot it spawned then rotted into a placebo (asserting
`text_elements` required at 0.133 while the runtime moved to 0.135/0.136), and a
non-`--experimental` regen made `dynamicTools` look removed in 0.136 when the
upstream `ThreadStartParams` struct was byte-identical and merely
experimental-gated.

### Item 4 — preserve local Codex environment inheritance for binary deployments
The upstream Elixir app-server launches `codex.command` from the issue workspace
with `bash -lc` or SSH command execution and does not sanitize away the
orchestrator user's environment; its README also says operators may copy
repository-local skills and that agents can use either Linear MCP or the
injected `linear_graphql` tool. Codex itself discovers skills and MCP
configuration through `HOME` / `CODEX_HOME`, repository skills, user skills,
admin/system skills, and `config.toml`; Codex's `shell_environment_policy`
controls what its own shell tools inherit after app-server startup.

**Earned by:** #1049 initially framed repo-owned skill dependencies as a fix
that should avoid inheriting the operator's current desktop/session skills. That
wording is reasonable for portable/containerized contracts but too broad for
local binary deployments: a direct binary worker running under the same user as
`codex app-server` should preserve the upstream behavior and naturally reuse the
same Codex home. The portability hardening is to validate explicit repo-owned
or workflow-declared dependencies when they are declared, not to amputate
host-local skills/MCP/Apps/connectors/plugin capability from the local binary
path. The same audit found two sibling regressions: the Codex app-server
environment omitted `CODEX_HOME`, breaking non-default Codex homes, and the
default workflow command lacked upstream's `shell_environment_policy.inherit=all`,
narrowing the environment seen by Codex-launched shell tools.

### Item 5 — run an adversarial pass on your own diff before a human looks
`@codex review` reads SPEC + the Elixir reference + `AGENTS.md`, and surfaces
precisely the gaps that human review tends to miss. Trigger it on the head commit
before marking the PR ready, and for each finding either fix it or document in
the PR body why it's deferred. Treat the agent's first draft as a candidate, not
a delivery. An independent adversarial pass catches what thorough first-party
review does not — run it even when your own review found nothing. **Earned by:**
PR #287's HIGH + MEDIUM findings were both surfaced by `@codex review` after
submit; both could have been caught one round earlier; and #666/PR #688's HIGH
stdout-reader deadlock (see item 2) was surfaced by a Codex review *after* an
exhaustive first-party review found nothing — the deadlock would otherwise have
shipped.

## Clean code

- **Rule 1/2 — no tech debt / no back-compat.** **Earned
  by:** PR #338 dual-emitting `last_codex_at` + `last_event_at` and deferring
  removal required a separate cleanup PR #342.
- **Rule 3/4 — one source of truth / names match the domain.**
  **Earned by:** the PR #342 audit found the wire rename in #338 left the
  internal `LastCodexAt` field untouched across four files (and the JSON
  rename without the Go identifier rename), violating the rule the PR introduced.
- **Rule 5 — comments explain *why*.** **Earned by:** PR #338 left a
  five-line comment block explaining a dual-field strategy that PR #342 deleted.
- **Rule 6 — tests assert the new code path.** **Earned by:** the PR
  #342 audit (LOW) noted the back-compat test asserted `last_codex_at` presence
  but not its absence after removal; and a #469/PR #483 session where a
  backup/restore during mutation testing silently reverted the fix, so local
  tests stayed green while the commit lacked it and only CI caught the regression
  (run the mutation against the committed artifact, not the working tree).
- **Rule 7 — function and file size budgets.** CI machine-enforces the
  per-function budgets via the blocking golangci-lint gate (`funlen`/`gocognit`),
  with the existing baseline grandfathered in-line via
  `//nolint:gocognit[,funlen] // baseline (#521)` directives (removed as #521
  decomposes them) — do not add a new `//nolint` to dodge the gate. CI also
  machine-enforces the ≤800-line file budget with the uncached
  `go test -run '^TestProductionGoFilesStayWithinSizeBudget$' -count=1 ./scripts`
  step (`scripts/file_size_budget_test.go`): existing oversized files carry exact
  line-count baselines, new oversized files fail, and any reduction must lower or
  remove that baseline in the same PR. This is a repo-specific maintainability
  budget, not an official Go limit. Decompose oversized files by responsibility
  first; introduce `internal` helper packages only when a cohesive boundary
  exists, and do not create public API solely to satisfy the line budget.
  **Earned by:** #410 found `RunTask` at 244 lines, `validateConfig` at 186
  lines, and `actor.go` at 2138 lines; PR #342 showed one concept rename had to
  touch four files partly because large files hid the domain boundary.
- **Rule 8 — errors are wrapped and classified.** **Earned by:** mixed
  `%w` / `%s` / `errors.New` styles made review-time reasoning about `errors.Is` /
  `errors.As` unreliable.
- **Rule 9 — failure messages print input/actual/expected.** **Earned
  by:** repository tests using `t.Errorf("expected ...")` without the actual
  value left reviewers guessing which state transition regressed.
- **Rule 10 — fix the class, not the instance.** **Earned by:** a
  standard-library security bump first updated only the module patch pin, so later
  review rounds found the matching container base-image pin, lint-cache exposure,
  and version-doctor granularity one by one; and #602's test refactor replaced an
  external deadline bound with a typed-error assertion that initially dropped the
  "did not wait for the outer deadline" invariant.
- **Rule 11 — mutation-test the wiring seam, not just the leaf.**
  **Earned by:** #682's retry-fire `requiredLabels` thread and the production
  `reconciliationConfigForWorkflow` builder were both unwired no-ops that the leaf
  `issueHasRequiredLabels` tests passed; #683's refresh label-carry
  (`issue.Labels = st.Labels`) had no positive test until one was added.

## Workflow and PR protocol

- **Issue workflow negative-constraint preflight.** For issues that cite a
  design doc, runbook, SPEC boundary, redaction/retention rule, or non-goal
  list, convert the negative constraints into a short implementation guardrail
  before writing code, and include that guardrail in the first pre-push reviewer
  brief. Redaction/retention over arbitrary human, agent, protocol, or Go `%v`
  map text defaults to opaque omission unless the issue explicitly asks for
  structured parsing with fixtures. **Earned by:** #938 / PR #942, where the
  design already said bounded evidence, metadata-first output, no raw GraphQL
  payloads, and no worker/runtime mutation, but the first implementation still
  chased a complex parser for GraphQL-like runner output and `%v` map text.
  Review pressure then found parser/redaction edge cases one by one instead of
  challenging the boundary choice up front. This is separate from #943, which
  tracks the LOC/readability budget incentive exposed by the same PR; #944 is
  the direction-setting lesson: choose an opaque boundary before parser edge
  cases exist.

## Conventions

- **All external I/O is timeout-bounded.** **Earned by:** #287
  retry-fire fetch had no timeout on an Elixir-port path, and #405 found the same
  failure class in the non-port Gitea tracker client.
- **Goroutine lifetimes are explicit.** **Earned by:** PR #304
  retrofitted panic recovery after one path could crash the worker; #413's audit
  found most production goroutine launch sites still lacked that guard, so this
  needs a machine-checkable follow-up rather than reviewer memory.
- **Secrets.** Secret-bearing values that arrive via config or
  CLI (e.g. `clone_url` basic-auth userinfo) must be masked before they reach any
  log, error string, or state output — env-var-only redaction does not cover
  them. Mask clone URLs with `workflow.MaskCloneURL`. **Earned by:** #469/PR #483,
  where a doctor ambiguity/not-found error echoed a credentialed `clone_url`
  because `redact()` only scrubbed env-var values.
- **Worker PR size budget.** ≤12 changed production files / ≤300
  changed production LOC is a review guideline, not an LOC-reduction mandate. Test
  files and generated code are excluded from the count, so test coverage never by
  itself pushes a PR into overage. (These were the `policy.max_changed_files` /
  `policy.max_changed_loc` worker caps; the worker no longer enforces them — the
  path/diffstat gate was removed in #561 because it ran post-push and raced
  reconcile-cancel — so the budget is now a review discipline, not config.) Worker
  PRs are draft + labeled by default; shape them small when you can, but the
  budget exists to catch scope creep and force explicit handling — not to
  incentivize deleting necessary tests, weakening state-machine coverage,
  skipping race coverage, or preferring compact code over clear reliable code
  when review feedback exposes a real correctness, safety, performance, or
  coverage gap. Classify every PR into exactly one of three states and surface it
  in the PR body:
  - `within budget` — production diff fits the ~12-file / ~300-LOC guideline (tests and generated code excluded).
  - `size-gated: justified overage` — over the budget because the extra LOC pays for correctness, regression coverage, race/state-machine safety, or other best-practice hardening that cannot be split without losing atomicity. Requires explicit human size-gate sign-off before merge.
  - `size-gated: split recommended` — over the budget because of scope creep, unrelated cleanup, or genuinely separable concerns. Stop and split into smaller PRs instead of asking for sign-off.

  Only reduce LOC when the code is genuinely redundant, over-abstracted,
  duplicated without purpose, or outside scope. Never delete meaningful tests,
  collapse normal formatting, merge unrelated responsibilities, or otherwise make
  code less readable solely to satisfy the budget. **Earned by:** PR #455 exceeded
  the default 300 LOC after multiple valid Codex review findings required
  additional race/state-machine coverage; the prevailing workflow language nudged
  the agent toward compressing tests to fit the threshold, which is backwards when
  the extra lines are paying for correctness. #938 / PR #942 exposed the same
  incentive on readability: a trace-harness report script was briefly compressed
  by removing normal blank lines between functions solely to stay under a physical
  line-count target. Counting production LOC only (tests/generated excluded)
  removes the test pressure at the source; explicit #943 guidance removes the
  remaining incentive to game physical line count with readability-hostile
  compression.
- **SPEC deviations are gated at author time.** The `PR Metadata`
  workflow (`.github/workflows/pr-metadata.yml` +
  `.github/scripts/validate-pr-metadata.mjs`) blocks a PR that changes a
  SPEC-sensitive path (`internal/workflow/config.go`, a newly-added
  `internal/orchestrator/`/`internal/worker/` file) while it claims no new
  key/phase — the author must cite an upstream Elixir reference or track a
  `DEVIATIONS.md` row (principle 6/7). Fill the `SPEC alignment` checklist in the
  PR template. This makes a documented deviation cost something *before* merge
  instead of being unwound later (the #73/#74/#76/#557/#561/D25 recurrence). The
  required-check wiring lives in `.github/governance/main-ruleset.json`. **Earned
  by:** #588 — those removals all shipped despite the rules existing, because the
  checks were judgment at audit time rather than mechanical at author time.
- **PR titles are Conventional Commits.** Squash-merge makes the PR title the
  commit subject release-please parses; titles using freeform `area:` prefixes
  (`maintainability:`, `cmd:`, `stateapi:`, `dashboard:`, …) are dropped silently
  by release-please — no changelog entry, no version bump — while `chore` was
  configured visible, flooding the changelog with housekeeping commits. The fix
  removes the `changelog-sections` override (chore/refactor inherit the
  upstream-default hidden state) and adds the
  `Validate PR title (Conventional Commits)` required check
  (`.github/workflows/pr-title-lint.yml`, pinned
  `amannn/action-semantic-pull-request`); separately, the repo setting
  `squash_merge_commit_title` is set to `PR_TITLE` (a GitHub repo setting applied
  out-of-band, not a committed file) so the parsed subject is the linted title.
  **Earned by:** the v0.1.3 Release PR (#803) was about to ship crediting only
  the single Conventional `feat` (#801), because every other change since v0.1.2
  carried a non-Conventional type that release-please neither listed nor counted
  — `cmd:` version observability (#828), `dashboard:` version chip + favicon
  (#834), `hardening:` goroutine panic recovery (#818), `release:` tarball
  packaging / GHCR images (#827/#829), `maintainability:` decompositions
  (#831/#832). The user-facing `feat`/`fix` losses (#828/#834/#818) had to be
  hand-backfilled into the v0.1.3 changelog after the tag was cut (#851); the
  `release:` and `maintainability:` work maps to the hidden `build`/`refactor`
  types and was correctly absent. The required check now prevents recurrence at
  author time.

# WORKFLOW.md front-matter key reference

The exhaustive operator-facing reference for every key the worker reads from
`WORKFLOW.md` YAML front matter (schema: `internal/workflow/config.go`). The
README's [defaults table](../../README.md#workflowmd-configuration) is the
SPEC §6.4 cheat-sheet view — it deliberately mirrors SPEC's own table; this
page is the complete view, including the keys SPEC's cheat-sheet does not
list. Keep the two consistent: the cheat-sheet stays the SPEC-mapping summary,
this page the single exhaustive source (clean-code rule 3).

For any one workdir, `worker --print-config /path/to/clone` prints the
effective resolved config (with `tracker.api_key` masked) and is the ground
truth this page approximates.

## Loading semantics

- **Front matter is optional.** A `WORKFLOW.md` without a `---` block is a
  prompt-only workflow: the body becomes the prompt template, every setting
  falls back to the defaults below, and schema validation is skipped.
- **Present front matter must decode to a YAML map.** Unknown top-level keys
  are logged and ignored; known-but-removed keys are rejected with an error
  naming the replacement (see [Removed keys](#removed-keys-rejected-at-load)).
- **Env indirection.** `tracker.api_key`, `tracker.endpoint`,
  `repo.clone_url`, `workspace.root`, `codex.command`, `claude.command`, and
  each `sandbox.credential_files[i]` accept a whole-value `$VAR` / `${VAR}`
  reference, resolved from the worker's environment at load. An unset or
  empty variable is a load error; partial interpolation
  (`https://$HOST/path`) is not expanded.
- **Path expansion.** `workspace.root` and `sandbox.credential_files[*]`
  expand a leading `~/`; a relative `workspace.root` resolves against the
  workflow file's directory.
- **Validation order is fixed** (`internal/workflow/validate.go`): tracker
  and repo prerequisites first, then enums, sandbox, server port,
  codex/claude, agent limits, timeouts. The first failure is reported with the workflow
  file path and the offending field.

## `repo`

| Key | Type | Default | Behavior | Validation |
|-----|------|---------|----------|------------|
| `repo.owner` | string | — | Repository owner/org; with `repo.name` it targets the Gitea/GitHub issue API and the agent-side `gitea_issue_labels` tool | — |
| `repo.name` | string | — | Repository name (paired with `repo.owner`) | — |
| `repo.clone_url` | string | — | Git URL each per-issue workspace clones. Embedded basic-auth userinfo is masked (`workflow.MaskCloneURL`) before logs/state output | required; `$VAR` |
| `repo.default_branch` | string | `main` | Base branch checked out for each per-issue workspace | — |

## `server`

| Key | Type | Default | Behavior | Validation |
|-----|------|---------|----------|------------|
| `server.host` | string | `127.0.0.1` | Bind address for the state HTTP server + dashboard. Empty means the loopback default, never bind-all; set `0.0.0.0` only behind a loopback-scoped port mapping with `AIOPS_STATE_API_TOKEN` auth (SPEC §15.3) | — |
| `server.port` | int | `4000` | State server port; `-1` disables the HTTP server and dashboard. CLI `--port` overrides (see `--print-config` provenance) | `-1`, or 1–65535; `0` rejected |

## `tracker`

| Key | Type | Default | Behavior | Validation |
|-----|------|---------|----------|------------|
| `tracker.kind` | string | — | Selects the tracker adapter | **required**; `gitea`, `github`, or `linear` |
| `tracker.api_key` | string | — | Tracker API token, referenced as `$VAR` (e.g. `$LINEAR_API_KEY`, `$GITEA_TOKEN`, `$GITHUB_TOKEN`). Worker-held: both the variable name and its value are denied from every agent `env_passthrough`/`env_allowlist`, and `--print-config` masks it | `$VAR` resolved at load |
| `tracker.endpoint` | string | — | Tracker API base URL (SPEC §5.3.1). Linear defaults to `https://api.linear.app/graphql`. When omitted, GitHub falls back to the `GITHUB_API_BASE_URL` env var, then `https://api.github.com`; Gitea falls back to the `GITEA_BASE_URL` env var, then the local-dev default `http://localhost:3000` | `$VAR` |
| `tracker.team_key` | string | — | Linear team key. Scopes the `linear_graphql` current-issue mutation guard's workflow-state lookup to one team, so state names that repeat across teams resolve unambiguously (`internal/runner/linear_graphql_current_issue_guard.go`) | — |
| `tracker.project_slug` | string | — | Linear project to poll (SPEC §11.2) | required when `kind: linear` |
| `tracker.active_states` | string list | `[Todo, In Progress]` | Issue states polled as dispatch candidates | — |
| `tracker.terminal_states` | string list | `[Closed, Cancelled, Canceled, Duplicate, Done]` | States that end work on an issue (also used by the retry/backoff loop to stop redispatch) | — |
| `tracker.inactive_states` | string list | `[]` | Non-terminal states that make an already-running issue ineligible: poll-tick reconciliation stops in-flight runs when an issue moves here (operator-pause states such as `Backlog`) | — |
| `tracker.required_labels` | string list | `[]` (gate off) | Opt-in dispatch gate (SPEC §4.1.1): an issue must carry every listed label to dispatch or keep running. Entries are trimmed, lowercased, de-duped; a blank entry matches no issue. See the README table row for the Linear 250-label projection ceiling | — |
| `tracker.pagination_max_pages` | int | `0` = adapter default (`github` 10, `gitea` 20, `linear` 200) | Caps one tracker pagination scan. Linear applies the same cap to issue listing and inverse-relation pagination | ≥ 0 |

## `polling`

| Key | Type | Default | Behavior | Validation |
|-----|------|---------|----------|------------|
| `polling.interval_ms` | int | `30000` | Tracker poll cadence (SPEC: "poll the issue tracker on a fixed cadence"). Non-positive values fall back to the default | — |

## `hooks`

Workspace lifecycle hooks (see `docs/runbooks/workspace-cache.md`). Each hook
value may be a single shell-script string, a list of command strings, or a
`{commands: [...]}` map.

| Key | Type | Default | Behavior | Validation |
|-----|------|---------|----------|------------|
| `hooks.after_create` | hook | — | Runs once after a workspace clone is created | — |
| `hooks.before_run` | hook | — | Runs before each agent run in the workspace | — |
| `hooks.after_run` | hook | — | Runs after each agent run | — |
| `hooks.before_remove` | hook | — | Runs before a workspace is removed | — |
| `hooks.timeout_ms` | int | `60000` | Per-hook subprocess timeout | positive when present |
| `hooks.env_passthrough` | string list | `[]` | Env vars hook subprocesses inherit beyond the POSIX baseline (`PATH`/`HOME`/`USER`/`LANG`/`LC_*`/`TZ`/`TERM`); tracker tokens excluded by default | — |

## `workspace`

| Key | Type | Default | Behavior | Validation |
|-----|------|---------|----------|------------|
| `workspace.root` | string | `<system-temp>/symphony_workspaces` | Root for deterministic per-issue workspaces (per-boot on tmpfs — set a long-lived path for persistence). An explicit `workspace.root` wins over the `AIOPS_WORKSPACE_ROOT` env var, which overrides only the built-in default; see `--print-config` provenance | `$VAR`; `~/` expansion |

## `agent`

| Key | Type | Default | Behavior | Validation |
|-----|------|---------|----------|------------|
| `agent.default` | string | `mock` | Runner used for dispatched issues | `mock`, `codex-app-server`, or `claude` |
| `agent.max_concurrent_agents` | int | `10` | Global concurrent-run cap (SPEC §6.4) | > 0 |
| `agent.max_concurrent_agents_by_state` | map[string]int | — | Per-state concurrency caps layered under the global cap. Keys are normalized (trim, lowercase, space→`_`) | each limit > 0; no empty state keys; no duplicates after normalization — violations are load errors |
| `agent.max_turns` | int | `20` | Per-session turn budget for the codex app-server in-session loop (SPEC §5.3.5) | > 0 |
| `agent.max_continuation_turns` | int | = `agent.max_turns` | Issue-level clean-turn budget across fresh + continuation dispatches (accepted deviation D34/#621); exhaustion parks the issue `blocked` (`continuation_budget`) | > 0 |
| `agent.max_retry_backoff_ms` | int | `300000` | Ceiling for the exponential retry backoff (SPEC §8.4; retries are unbounded — move the issue out of active states to stop them) | > 0 |
| `agent.timeout` | duration | `30m` | Wall-clock cap for a single runner invocation (e.g. `timeout: 2h`); exceeding it kills the subprocess and records `runner_timeout` | — |

## `codex`

Settings for the `codex-app-server` runner (SPEC §10; long-running JSON-RPC
over stdio).

| Key | Type | Default | Behavior | Validation |
|-----|------|---------|----------|------------|
| `codex.command` | string | `codex app-server` | Launch command for the app-server subprocess; real-Codex workflow templates add `--config shell_environment_policy.inherit=all` for upstream-style shell-environment inheritance | `$VAR`; a `codex exec` argv is rejected (#541) |
| `codex.env_passthrough` | string list | `[]` | Env vars the Codex app-server subprocess inherits beyond its baseline (`PATH`, `HOME`, `CODEX_HOME`, `USER`, locale, `TZ`, `TERM`) — for model CLI auth/proxy/CA vars. Tracker/repo tokens (`GITHUB_TOKEN`, `GITEA_TOKEN`, `LINEAR_API_KEY`, …) and the `tracker.api_key` variable/value are denied | denied names rejected at load |
| `codex.approval_policy` | map | `granular` with every flag `false` (auto-reject all approval prompts) | Sent as the app-server approval policy | — |
| `codex.thread_sandbox` | string | `workspace-write` | `thread/start` sandbox string; also the single knob the per-turn policy derives from (DEVIATIONS D32) | — |
| `codex.turn_sandbox_policy` | typed map | derived from `thread_sandbox` | Explicit per-turn `sandboxPolicy` override; `type` is required (`dangerFullAccess`, `readOnly`, `externalSandbox`, `workspaceWrite`), with per-type required fields (`writableRoots`, `networkAccess`, …) | strict per-type field checking; legacy `mode:`-style shapes rejected |
| `codex.turn_timeout_ms` | int | `3600000` (1h) | Wall-clock cap for a single turn; exceeding it cancels the turn and surfaces a turn-timeout on the retry path | > 0 |
| `codex.read_timeout_ms` | int | `5000` | Per-read transport budget while waiting for one protocol line outside a stall-governed turn (handshake/control reads); inside a turn the stall budget supersedes it | > 0 |
| `codex.stall_timeout_ms` | int | `300000` (5m) | Stall detection (SPEC §8.5 Part A): max time since the last agent event before the turn is declared stalled, terminated, and retried; `0` disables | ≥ 0 |
| `codex.linear_graphql.allow_mutations` | bool | `false` | With the default, every GraphQL mutation through the agent-visible `linear_graphql` tool is rejected before any request leaves the process; reads are unrestricted | — |
| `codex.linear_graphql.allowed_mutations` | string list | `[]` (= all, once mutations are allowed) | Per-operation allow-list of top-level Mutation field names (e.g. `issueUpdate`, `commentCreate`) | valid GraphQL names, unique; requires `allow_mutations: true` |

For `workspaceWrite`, the issue workspace is the writable project unit, while
`writableRoots` adds other writable roots. The current defaults leave `$TMPDIR`
and `/tmp` writable too. Set both `excludeTmpdirEnvVar: true` and
`excludeSlashTmp: true` in an explicit `workspaceWrite` policy to remove those
automatic grants. By default, the runner injects `GOCACHE` and `GOMODCACHE`
below the worker's temporary directory. Go workflows should normally leave the
applicable temporary root writable. If either cache variable is overridden
through `codex.env_passthrough`, each override path must be writable and visible
to both sandbox layers; when `sandbox:` is enabled, also add each overridden
name to `sandbox.env_allowlist`. Bubblewrap does not mount arbitrary Codex
`writableRoots`, so an external cache root cannot rely on that inner Codex
setting alone. Otherwise Go commands can fail
([#544](https://github.com/xrf9268-hue/aiops-platform/issues/544)). Codex's fixed
protected metadata locations are documented upstream, but this configuration
does not accept an operator-defined repository-subpath denylist.

## `claude`

| Key | Type | Default | Behavior | Validation |
|-----|------|---------|----------|------------|
| `claude.command` | string | `claude` | Launch command for the Claude runner | `$VAR` |
| `claude.env_passthrough` | string list | `[]` | Same deny-list as `codex.env_passthrough`, but the default Claude/generic baseline excludes `CODEX_HOME`; opt in explicitly only when that runner intentionally needs it | denied names rejected at load |

`claude:` shares the `codex:` schema shape, but the app-server fields
(`approval_policy`, `thread_sandbox`, `turn_sandbox_policy`, the `*_timeout_ms`
keys) are only consumed by the codex app-server runner, and
`claude.linear_graphql` is rejected at load — declare narrowing under
`codex.linear_graphql`.

## `policy`

| Key | Type | Default | Behavior | Validation |
|-----|------|---------|----------|------------|
| `policy.mode` | string | `draft_pr` | Prompt-directive selector only (the worker-side policy gate was removed — DEVIATIONS D33, #561): `analysis_only` appends the analysis-only directive and skips push expectations; any other value behaves as `draft_pr` | — |

## `sandbox`

Optional worker-side process hardening around agent invocation (off by
default; Symphony mandates no universal sandbox posture).

The worker process sandbox and Codex turn sandbox are separate layers. The
worker wrapper exposes the complete issue workspace read-write while constraining
the agent process, environment, credential mounts, and network. Host filesystem
visibility is backend-specific: bubblewrap exposes only explicit mounts, while
firejail may still expose host paths accessible to the worker OS user outside
its private home/workdir with their ordinary permissions. Neither layer accepts
repository-relative allow or deny paths; use prompt scope as advisory guidance
and repository permissions, branch protection, review, and CI for enforced
controls over sensitive paths.

| Key | Type | Default | Behavior | Validation |
|-----|------|---------|----------|------------|
| `sandbox.enabled` | bool | `false` | Wraps the agent subprocess in the selected backend | requires a non-`none` backend and a non-empty `env_allowlist` |
| `sandbox.backend` | string | `none` | Sandbox implementation | `none`, `bubblewrap`, or `firejail` |
| `sandbox.network` | string | `none` | Sandbox network mode | `none` or `allowlist` |
| `sandbox.network_allowlist_cidrs` | string list | `[]` | IPv4 CIDRs reachable from the sandbox when `network: allowlist` | required for `allowlist`; IPv4 CIDRs only |
| `sandbox.network_interface` | string | — | Host interface Firejail attaches `--netfilter` to | required for `network: allowlist` (which also requires `backend: firejail`) |
| `sandbox.env_allowlist` | string list | `[]` | Operator-selected env vars the sandboxed child keeps; the worker-injected `GOCACHE` and `GOMODCACHE` paths are carried separately, while operator-supplied cache paths do not receive that exception and tracker-token names remain denied | required when `enabled: true` |
| `sandbox.credential_files` | string list | `[]` | Files bind-mounted into the sandbox for model-CLI credentials | `$VAR`; `~/` expansion |

## `verify`

| Key | Type | Default | Behavior | Validation |
|-----|------|---------|----------|------------|
| `verify.commands` | string list | `[]` | Surfaced to the agent's rendered prompt as its own pre-handoff contract; the worker does not run them (SPEC §1 agent boundary) | — |

## Removed keys (rejected at load)

These once configured worker behavior that no longer exists; the loader fails
loud with the replacement guidance instead of silently dropping them:
`codex.profile`, `claude.profile` (#541), `verify.timeout`,
`verify.allow_failure`, `verify.env_passthrough` (#557), `verify.secret_scan`
(#561), `policy.allow_paths`, `policy.deny_paths`, `policy.max_changed_*`,
`agent.policy_violation_budget` (#561), `agent.fallback` (#40),
`agent.max_retry_attempts`, `agent.max_timeout_retries` (#577), the
top-level `pr:` / `safety:` blocks (#578), `tracker.statuses` (#786, worker-side
tracker writes are agent-side per SPEC §1 / #76 / #678), and `workspace.hooks`
(#786, use the top-level `hooks:` block). The pre-release compatibility aliases
`tracker.base_url` (#911, use `tracker.endpoint`), `tracker.poll_interval_ms`
(#911, use `polling.interval_ms`), and Gitea `tracker.project_slug` (#911, use
`tracker.endpoint`; Linear still uses `tracker.project_slug` as the project
slug) are also rejected at load.

## Coverage

Every YAML-tagged field in `internal/workflow/config.go` appears above.
Unexported struct fields (`apiKeyEnvVar`, `portSet`, `rootSet`,
`hookFields`, `turnSandboxPolicySet`) carry loader bookkeeping, have no YAML
tag, and are not front-matter keys.

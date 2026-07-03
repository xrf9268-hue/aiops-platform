# Preserve Codex App Environment Inheritance

**Date:** 2026-07-03
**Issue:** [#1049](https://github.com/xrf9268-hue/aiops-platform/issues/1049)
**Status:** Draft for rollback implementation

## Problem

`codex app-server` threads currently receive a runner-owned `config` object that
forces Codex Apps/connectors off:

```json
{
  "apps": {
    "_default": {
      "enabled": false,
      "open_world_enabled": false,
      "destructive_enabled": false
    }
  },
  "features": {
    "apps": false,
    "connectors": false
  }
}
```

That behavior was introduced by #635 to avoid an unattended GitHub run stalling
on an interactive Codex App approval dialog. The fix was broader than the
failure: it disables host-configured Apps/connectors for every local
`codex app-server` thread, even when the operator intentionally enabled them in
the Codex environment that runs the worker.

For direct local binary deployments, this is the wrong boundary. The upstream
Elixir app-server launches `codex.command` from the issue workspace without
injecting a thread config that disables Apps/connectors. It naturally inherits
the environment of the user running the orchestrator. `aiops-platform` should
preserve that same expectation when the worker and `codex app-server` run as the
same user.

## Decision

Rollback the default thread-scoped Apps/connectors disablement. By default, the
runner should omit the `config` field from `thread/start`, allowing Codex to use
its normal effective configuration for Apps/connectors, skills, plugins, and MCP
discovery.

Preserve Codex home inheritance explicitly. `CODEX_HOME` is part of Codex's
documented environment surface and points at config, auth, logs, sessions,
skills, and standalone package metadata; the runner baseline should pass it to
the app-server when the worker has it set.

Preserve Codex shell-tool environment inheritance in real-Codex workflow
templates. The core `codex.command` default remains the SPEC §6.4 value
`codex app-server`, while shipped real-Codex templates should launch app-server
with `--config shell_environment_policy.inherit=all`, matching the upstream
WORKFLOW's local-binary posture while still keeping tracker/repo tokens out of
the app-server environment through aiops-platform's existing deny filter.

The runner still advertises explicit Symphony dynamic tools such as
`linear_graphql`. Those tools are additive and remain the portable contract for
tracker-specific workflow actions.

## Scope

### In scope

- Remove the default `thread/start.config` payload that forces
  `features.apps=false`, `features.connectors=false`, and
  `apps._default.enabled=false`.
- Add `CODEX_HOME` to the agent subprocess baseline environment.
- Update workflow examples and runbooks that explicitly pin
  `command: codex app-server` so real-Codex templates use the upstream-style
  inherited shell environment when real Codex is intended.
- Replace tests that pin disabled Apps/connectors with tests that assert the
  runner does not override the host Codex Apps/connectors configuration by
  default.
- Keep explicit runner-owned `dynamicTools` unchanged.
- Keep approval and elicitation handling unchanged so unattended sessions still
  fail closed when Codex asks the operator for input the worker cannot provide.
- Update #1049 wording so it distinguishes:
  - inherited same-user local Codex environment capability,
  - repo-owned required skill dependencies for portability/diagnostics,
  - open-world/destructive app access policy.

### Out of scope

- Do not change `multiAgentMode: none` in this rollback. That setting was added
  separately by #999 and controls injected multi-agent mode instructions, not the
  Apps/connectors inheritance bug. Follow-up issue #1056 owns the separate audit.
- Do not add a new workflow front-matter key in this rollback.
- Do not add worker-side PR, review, merge, or tracker-write shortcuts.
- Do not disable ordinary user skills or MCP configuration for same-user local
  binary deployments.
- Do not silently enable open-world or destructive app behavior beyond whatever
  the operator's effective Codex configuration already enables.

## Design

`buildThreadStartParams` should build the minimal upstream-aligned
`thread/start` payload:

```go
map[string]any{
    "approvalPolicy": approvalPolicy,
    "sandbox":        in.Workflow.Config.Codex.ThreadSandbox,
    "cwd":            in.Workdir,
    "dynamicTools":   appServerDynamicToolSpecs(in.Workflow.Config),
    "multiAgentMode": "none",
}
```

There should be no `config` key unless a future, explicit workflow/operator
policy requires one. Omitting the key lets Codex resolve its own configuration
from the process environment and Codex config files.

The schema contract test for `ThreadStartParams` remains valuable and should
continue validating the actual payload. The standalone `Config` schema test for
`appServerThreadConfig` should be removed because the runner no longer builds a
thread config by default.

`agentEnv` should include `CODEX_HOME` in its baseline next to `HOME`. Existing
tracker-token deny logic still applies to both baseline and passthrough names, so
this does not expose `GITHUB_TOKEN`, `GITEA_TOKEN`, `LINEAR_API_KEY`, or the
configured tracker token value.

Real-Codex workflow templates should use the upstream-style command:

```text
codex app-server --config shell_environment_policy.inherit=all
```

The core `workflow.DefaultConfig().Codex.Command` and empty-command fallback in
`NewCodexAppServerCommand` stay `codex app-server` to preserve SPEC §6.4. When a
workflow sets the upstream-style command above, the argument order keeps
`app-server` as the direct subcommand so the runner's direct-exec path remains
active; it does not need a shell wrapper.

## Tests

- Update `TestBuildThreadStartParamsDisablesInheritedCodexAppTools` into a
  positive inheritance-preservation test:
  - build the payload,
  - assert `payload["config"]` is absent,
  - assert `dynamicTools` is still present when tracker config requires it.
- Keep `TestBuildThreadStartParamsPinsMultiAgentMode` unchanged for this
  rollback.
- Remove `TestCodexAppServerThreadConfigMatchesSchema`; the config helper should
  no longer exist.
- Add/adjust tests proving `CODEX_HOME` is part of the runner baseline and the
  app-server subprocess environment.
- Keep SPEC-default tests for `codex app-server`; use template/doc checks for
  the explicit real-Codex workflow command posture.
- Run:

```bash
go test ./internal/runner
go test ./internal/workflow
go test ./scripts
git diff --check
```

## Acceptance Criteria

- `thread/start` no longer sends `config.features.apps=false` or
  `config.features.connectors=false` by default.
- `thread/start` no longer sends `apps._default.enabled=false` by default.
- `CODEX_HOME` is inherited by the app-server subprocess without requiring
  `codex.env_passthrough`.
- Shipped real-Codex workflow templates include
  `shell_environment_policy.inherit=all`.
- Explicit `dynamicTools` continue to be advertised.
- `multiAgentMode: none` remains unchanged.
- Existing approval/input-required handling remains unchanged.
- #1049 no longer frames host-local Apps/connectors inheritance as something the
  runner should disable by default.

## Cross-Check Verdict

Treat these as inheritance regressions and handle them in this rollback:

- Default `thread/start.config` disabled Apps/connectors.
- Agent subprocess baseline omitted `CODEX_HOME`.
- Real-Codex workflow templates pinned the narrower `codex app-server` command
  instead of the upstream-style
  `codex app-server --config shell_environment_policy.inherit=all`.

Reviewed but keep separate from this rollback:

- `multiAgentMode: none`: schema says it leaves multi-agent tools available and
  only suppresses injected delegation instructions. Follow-up #1056 owns the
  separate schema/app-server evidence audit.
- `codex.approval_policy` granular all-false: this is the current-schema
  definite fail-closed posture for unattended input/approval requests. #624
  remains the right place to audit approval kinds that slip past granular flags;
  do not "fix" it by disabling entire capability surfaces.
- `linear_graphql.allow_mutations: false`: this is a token-isolated dynamic-tool
  mutation gate, not a host Codex environment inheritance issue. Workflows that
  need agent-owned Linear writes should opt into allowed mutations explicitly.
- Tracker/repo token env deny-list: this preserves the SPEC token boundary and
  should not be relaxed as part of local Codex environment inheritance.
- Optional worker sandbox `env_allowlist`: only applies when the operator opts
  into the external sandbox wrapper; it is not part of the default local binary
  app-server path.

## Follow-up Reflection

#635 solved a real unattended-approval stall by turning off an entire Codex
surface. The narrower failure was approval handling for a specific interactive
tool path. Future hardening should first identify the failed behavior and then
constrain that behavior directly; broad session-level disablement needs an
explicit SPEC/upstream justification before it becomes the default.

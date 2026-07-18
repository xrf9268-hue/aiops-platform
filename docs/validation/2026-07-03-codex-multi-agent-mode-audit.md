# Codex app-server multiAgentMode audit

**Date:** 2026-07-03
**Issue:** #1056

> [!WARNING]
> **Superseded protocol conclusion.** This dated audit is retained as evidence
> for [#1056](https://github.com/xrf9268-hue/aiops-platform/issues/1056) and the
> Codex 0.142.x contract, but its recommendation to send
> `thread/start.multiAgentMode` is no longer current. [#1101](https://github.com/xrf9268-hue/aiops-platform/issues/1101)
> aligned the runner with Codex 0.144.4, where that field is deprecated and
> ignored, so the current payload intentionally omits it. Use the
> [protocol/version pin](../../internal/runner/codex_version.go) and
> [schema contract tests](../../internal/runner/codex_app_server_schema_test.go)
> as the current protocol sources of truth; the
> [payload builder](../../internal/runner/codex_app_server.go) shows the current
> request shape. The material below is historical evidence, not current payload
> guidance.

## Verdict

Keep `thread/start.multiAgentMode: "none"` in the Codex app-server runner.

This is a Codex protocol compatibility pin, not a host-local capability gate.
For the vendored 0.142.0 target schema, `none` preserves the pre-0.142 prompt
contract by avoiding injected multi-agent delegation instructions. It does not
disable multi-agent tools, runner-advertised `dynamicTools`, host MCP discovery,
skills, Apps/connectors, plugin-provided capability, or Codex config inheritance.

No SPEC deviation is needed. Upstream Symphony does not send this field because
it tracks an older Codex protocol shape, but SPEC allows implementations to use
the targeted protocol to advertise client-side tools and set supported runner
policy. The aiops-platform runner keeps advertising `dynamicTools` in the same
`thread/start` payload.

## Evidence

### Vendored target schema: Codex 0.142.0

`internal/runner/testdata/codex_app_server_protocol_v0_142_0.v2.schemas.json`
was generated with `codex app-server generate-json-schema --experimental` and
is the runner's checked-in contract for `CodexProtocolVersion = "0.142.0"`.

Relevant schema facts:

- `MultiAgentMode` says it controls whether the model receives multi-agent
  delegation instructions, and that `none` leaves multi-agent tools available
  without injecting delegation instructions.
- `ThreadStartParams.multiAgentMode` says omitted defaults to
  `explicitRequestOnly`.
- `ThreadStartParams.dynamicTools` is a separate property on the same request
  object.

That is the reason PR #999 pinned `multiAgentMode` to `none`: omitting the field
would have opted aiops-platform into `explicitRequestOnly` instruction injection
after the Codex 0.142 schema bump.

### Current local Codex schema: Codex 0.142.5

Checked locally in this workspace:

```bash
codex --version
tmp=$(mktemp -d)
codex app-server generate-json-schema --out "$tmp" --experimental
```

The local binary reports `codex-cli 0.142.5`. Its generated schema still defines
`MultiAgentMode` as delegation-instruction control and says `none` leaves the
multi-agent tools available. The request property is now described as
deprecated/ignored, while `dynamicTools` remains a separate `ThreadStartParams`
property.

That current schema is compatible with keeping the 0.142.0 pin: at worst the
newer app-server ignores it, and it still is not a capability disablement.

### Runner payload and tests

The runner's `buildThreadStartParams` sends:

- `approvalPolicy`
- `sandbox`
- `cwd`
- `dynamicTools`
- `multiAgentMode: "none"`

It does not send a `config` object. That keeps host-local Codex config discovery
in Codex's own hands and avoids the Apps/connectors disablement class covered by
#1049.

The regression tests now cover the important wiring seam:

- `TestBuildThreadStartParamsPinsMultiAgentModeWithoutSuppressingDynamicTools`
  fails if the field is omitted/changed or if runner dynamic tools stop being
  advertised under `multiAgentMode: "none"`.
- `TestBuildThreadStartParamsPreservesInheritedCodexAppConfig` fails if the
  runner reintroduces a `thread/start.config` override that could suppress
  host-local Apps/connectors or other Codex config.
- `TestCodexAppServerThreadStartPayloadMatchesSchema` validates the actual
  `thread/start` payload against the vendored experimental schema and exercises
  a non-empty `DynamicToolSpec`.

## Upstream and SPEC comparison

Upstream Symphony's Elixir app-server sends `approvalPolicy`, `sandbox`, `cwd`,
and `dynamicTools` in `thread/start`; it does not send `multiAgentMode`.

SPEC says the runner should advertise implemented client-side tools using the
targeted app-server protocol. For aiops-platform's targeted 0.142.0 protocol,
the compatibility pin and explicit `dynamicTools` advertisement are both part
of that request shape. No worker-side tracker write, verification phase, gate,
cache, or artifact is introduced here.

## Non-goals

- Do not change #1049's Apps/connectors, `CODEX_HOME`, shell environment, or
  repo-owned skill dependency work.
- Do not add a workflow config key.
- Do not add a worker/orchestrator phase, gate, artifact, or tracker mutation.

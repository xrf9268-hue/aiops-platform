# Multi-service WORKFLOW routing

`services` is an aiops-platform workflow extension for Linear installations
that want one service-level `WORKFLOW.md` to route issues to several
repositories. Symphony SPEC §5.3 treats unknown top-level workflow keys as
implementation extension points; this page documents the aiops-platform
contract for the top-level `services` key tracked by D25 / issue #143.

## Boundary

`services` only affects read-only candidate selection. The poller reads Linear
issue metadata, chooses the configured service/repository for exactly one match,
and dispatches the agent with that service's repository settings. It does not
write tracker state, comments, pull request links, or labels. Per SPEC §1,
tracker writes remain agent/tool-side through the workflow runtime tools.

The extension is currently Linear-only. Omit `services` for the default
single-service mode that uses the top-level `repo` and `tracker` sections.

## Schema

`services` is an optional top-level array:

```yaml
services:
  - name: api
    repo:
      owner: acme
      name: api
      clone_url: git@github.com:acme/api.git
      default_branch: main
    tracker:
      project_slug: api-platform
      team_key: ENG
      labels: [backend]
      custom_fields:
        Runtime: go
```

Each service object has these fields:

| Field | Type | Required | Default | Meaning |
|---|---|---:|---|---|
| `name` | string | yes | none | Stable service identifier. Names are case-insensitively unique. |
| `repo.owner` | string | no | none | Repository owner metadata used by downstream repo/PR tooling. |
| `repo.name` | string | no | none | Repository name metadata used by downstream repo/PR tooling. |
| `repo.clone_url` | string | yes | none | Git URL cloned for issues routed to this service. Environment-variable indirection such as `$API_REPO_URL` is expanded by the workflow loader. |
| `repo.default_branch` | string | no | `main` | Default branch for the service repository. |
| `tracker.project_slug` | string | conditional | top-level `tracker.project_slug` if omitted | Linear project slug predicate and polling scope for this service. |
| `tracker.team_key` | string | no | none | Linear team key predicate. |
| `tracker.labels` | string array | no | empty | Linear labels that must be present on the issue. |
| `tracker.custom_fields` | string map | no | empty | Linear custom field name/value predicates that must match the issue. |

The top-level `tracker.project_slug` may provide the polling scope for services
that omit `services[].tracker.project_slug`. A service-only Linear workflow may
omit the top-level `repo` block when every dispatchable repository is supplied
under `services`.

## Matching and validation

Validation happens when `WORKFLOW.md` is loaded:

- `services` is optional. An omitted or empty array preserves single-service
  routing through the top-level `repo` block.
- `tracker.kind` must be `linear` for service-routed workflows that omit the
  top-level `repo.clone_url`; non-Linear workflows still require the top-level
  `repo.clone_url`.
- Every service requires `name` and `repo.clone_url`.
- Service names must be unique case-insensitively.
- Every Linear service route must define at least one route predicate among
  `project_slug`, `team_key`, `labels`, or `custom_fields`.
- Every Linear service route must have a project slug either in
  `services[].tracker.project_slug` or top-level `tracker.project_slug`, so the
  poller has an explicit project scope.

At poll time the orchestrator reads Linear candidates for the configured project
scopes and evaluates the service predicates against issue metadata. An issue
that matches one service is dispatched with that service's `repo` settings and a
service-specific workspace key. Unmatched issues are skipped by that poll tick.
Ambiguous matches are configuration errors and fail the poll tick instead of
choosing a repository implicitly.

## Defaults

`services` has no implicit service entries. Field defaults are intentionally
minimal: `repo.default_branch` defaults to `main`; omitted route predicates are
not considered; `tracker.project_slug` may inherit from the top-level tracker
section as described above. Other top-level workflow defaults (`polling`,
`workspace`, `agent`, hooks, sandbox, etc.) continue to apply normally.

## Reload and restart behavior

The worker loads a single service-level `WORKFLOW.md` when the worker process
starts, then the workflow reload loop watches that file and publishes new
runtime snapshots when it changes. The runtime poller rebuilds its Linear project
clients and `services` routing from the current snapshot on subsequent poll
ticks, so service-route changes are intended to apply dynamically without a
process restart. If a reload fails validation, the last valid routing snapshot
continues to drive polling and a `workflow_reload_failed` event is emitted.

Startup reconciliation uses the loaded service routes to preserve active
service-specific workspaces for Linear issues that are still eligible. D11 /
issue #85 continues to track remaining dynamic reload coverage gaps outside the
`services` routing behavior documented here.

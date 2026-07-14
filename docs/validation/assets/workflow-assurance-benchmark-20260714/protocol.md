# Workflow assurance benchmark preregistered protocol

Recorded before repository creation or issue activation on 2026-07-14.

## Goal

Compare assurance, worker-observed token/runtime cost, wall time, and rework for
three existing workflow profiles on one fixed two-issue Go CLI backlog.

## Pinned variables

| Variable | Pinned value |
| --- | --- |
| Worker release | aiops-platform v0.1.15 official darwin/arm64 asset |
| Codex CLI | 0.144.3 |
| Model | `gpt-5.6-sol` |
| Reasoning | high |
| Sandbox | `danger-full-access` |
| Concurrency | one issue and one agent at a time |
| Seed | one unchanged git commit |
| Backlog | add/list, then complete/filter |
| CI | `go test ./...` and `go vet ./...` |
| Holdout | `holdout.sh`, written before activation and hidden from agents |
| Cache | fresh workspace and mirror roots; shared Go module cache |
| Warmup | real worker doctor before activation; no trial issue |

## Profile-only differences

1. Lean: one agent implements, tests, opens a closing PR, waits for CI, and
   squash-merges.
2. Standard: a maker writes a non-closing PR; an independent reviewer reruns
   gates, approves, enables squash auto-merge, confirms merge, and closes.
3. High: standard plus exact-head nested Codex review, GraphQL review-thread
   audit, explicit negative/failure-path checks, and one external
   `@codex review` trigger per reviewed head.

## Operator boundary

After activation, the operator may activate the next issue after the previous
one closes, capture worker/forge state, and abort an invalid run. The operator
must not edit code or PRs, issue a review verdict, merge, repair lifecycle
labels, or manufacture Rework.

## Measurements

Capture fresh process state, handoff and completion state, forge timestamps,
worker `codex_totals`, completed-session usage, wall time, heads/revisions, CI,
reviews, threads, rework, holdout outcome, and interventions. External and
otherwise unobservable review token use must remain explicitly unmeasured.

## Decision rule

Among profiles that merge both issues and pass the same fresh-clone holdout,
prefer the lowest cost unless a higher profile finds a real defect. If the
higher profile cannot complete or the evidence conflicts, report a directional
split recommendation or an inconclusive universal result. Do not generalize one
run statistically.

## Recorded amendments

Amendments were recorded when a setup/prompt defect was detected, before a
replacement valid arm was activated. The invalid attempt was abandoned rather
than repaired in place.

1. Standard v2 used a shortened maker prompt that ended after internal review
   without PR handoff. Two identical no-handoff loops were excluded.
2. Standard v3 used the full maker example but retained an overlapping final
   review skill that terminated before handoff. The valid v4 maker skipped that
   overlapping final reviewer because the independent reviewer already owned
   the final Standards/Spec gate.
3. High v4 omitted `AIOPS_MIRROR_ROOT` and selected the host default cache. It
   was stopped before a PR; later attempts used explicit roots.
4. High v5 invited the maker but did not have that identity accept the
   collaborator invitation. A push failed 403; later attempts verified
   `viewerPermission: WRITE` before activation.
5. High v7 accidentally omitted distinct maker/reviewer mirror roots. The two
   roles contended for one git worktree after review requested rework, so the
   arm was stopped and all of its results were excluded. Valid v8 used a fresh
   repository, workers, workspaces, and distinct explicit mirror roots.
6. The initial lean v2, standard v4, and high v6 round retained state evidence
   as transcript-derived projections instead of exact full API responses. Its
   results were superseded and excluded. Fresh lean v3, standard v5, and high
   v8 arms sampled the full `/api/v1/state` response every two seconds and
   committed selected unmodified lifecycle-boundary responses.

The seed, issue bodies/order, CI, holdout, worker release, Codex version/model,
reasoning, sandbox, identities, and concurrency did not change across valid
arms.

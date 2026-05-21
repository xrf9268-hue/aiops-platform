# symphony-integration.md WORKFLOW.md text fix — design

**Date:** 2026-05-22
**Issue:** [#236](https://github.com/xrf9268-hue/aiops-platform/issues/236)

## Problem

`docs/symphony-integration.md` describes WORKFLOW.md as if D4 (multi-path discovery) and D10 (per-service workflow file) were still open:

- Line 60: "repo-owned `WORKFLOW.md` (discovered at three paths — see Deviations below)" — implies multi-path discovery is the current behavior.
- Lines 79-83 (Deviations paragraph): "There are no current accepted deliberate extensions. In particular, multi-path `WORKFLOW.md` discovery is not an accepted extension: D4 is closed …" — contradicts the immediately-following paragraph that describes the multi-service `services` workflow key (D25) as an accepted extension.

D4 and D10 are both **Closed** in `DEVIATIONS.md:44, 50`. The `internal/workflow/resolver.go` enumerates exactly one path. `README.md:44` already states the single-canonical-path posture.

## Decision

Two surgical edits in `docs/symphony-integration.md`:

1. Line 60 — change the "discovered at three paths" parenthetical to describe single-canonical-path behavior.
2. Lines 79-83 — rewrite the Deviations paragraph to remove the contradiction with the services-routing paragraph below. List closed historical deviations as examples of what's *no longer* in the codebase.

The services-routing paragraph (lines 85-90) is correct and stays.

Audit confirms no other operator-facing doc carries the stale claim (the surviving references are inside historical superpowers plans/specs under `docs/superpowers/{plans,specs}/2026-05-09-workflow-discovery-fallback*` — archival material from when D4 was open, not operator-facing).

## What changes

| File | Change |
| --- | --- |
| `docs/symphony-integration.md:60` | Replace "(discovered at three paths — see Deviations below)" with "in the service/repository root (single canonical path; see `DEVIATIONS.md` D4 closure)". |
| `docs/symphony-integration.md:79-83` | Rewrite to remove the "no current accepted deliberate extensions" contradiction; reference D4/D7/D8 as closed examples. |

## Non-goals

- Don't touch `docs/superpowers/specs/2026-05-09-workflow-discovery-fallback-design.md` or `docs/superpowers/plans/2026-05-09-workflow-discovery-fallback.md` — those are archival design docs from when D4 was open. Editing them would rewrite history.
- Don't touch `AGENTS.md` or `README.md` — both already state the single-canonical-path posture correctly.
- Don't add a new section about closed deviations — DEVIATIONS.md is the canonical ledger; symphony-integration.md should reference it, not duplicate it.

## Acceptance criteria

- [ ] `docs/symphony-integration.md` no longer claims multi-path discovery.
- [ ] References to DEVIATIONS.md reflect current status (D4 closed).
- [ ] `grep -rn "three paths\|\.aiops/WORKFLOW" docs/ README.md AGENTS.md` returns no operator-facing claim that legacy fallback discovery still applies. (Historical superpowers archival plans/specs may still mention them as part of their original design context — that's expected.)

## Refs

- Issue: https://github.com/xrf9268-hue/aiops-platform/issues/236
- DEVIATIONS.md table: D4 closed via #72, D7 closed via #74, D8 closed via #76
- README.md:44 already states the single-canonical-path posture
- `internal/workflow/resolver.go` is the single-path implementation

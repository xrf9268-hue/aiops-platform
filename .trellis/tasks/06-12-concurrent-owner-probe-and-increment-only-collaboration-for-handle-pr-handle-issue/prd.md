# Concurrent-owner probe and increment-only collaboration

## Background (earned by)
PR #768 (2026-06-12): the authoring cloud session and a local /handle-pr session
both responded to the same `@codex review` findings. Three consecutive push
races (403 classification, headerless 403, size-budget split); the local
session's three equivalent implementations were all discarded, only the
increments (per-endpoint test pins, ErrRange saturation, discriminator unit
tests) landed. Wasted rework + reset/cherry-pick coordination risk.

## Requirements (user-approved 2026-06-12)
1. Shared rule lives once in docs/runbooks/pr-review-merge-protocol.md (new §9):
   single-driver rule, active-owner probe signals, autonomous increment-only
   collaboration policy (observe-first window, take-over criterion, never
   force-push over the other session, remote-as-base on rejection).
2. handle-pr SKILL.md: probe at start and before every push; choose mode
   autonomously, inform (not ask) the user.
3. handle-issue SKILL.md: probe at start — existing open PR or active remote
   fix/<n>-* branch means do NOT re-implement; route to handle-pr / adopt branch.
4. Both skills: autonomy-first default — outside protocol §8 hard stops
   (merge permission, destructive ops, scope changes), decide and act, report
   briefly, don't ask.

## Acceptance criteria
- [ ] Protocol §9 added; no renumbering of §1–§8.
- [ ] handle-pr references §9 (no rule duplication beyond a pointer + trigger).
- [ ] handle-issue references §9 with issue-phase probe.
- [ ] Docs-only PR, separate from any code PR, body cites PR #768 as provenance.

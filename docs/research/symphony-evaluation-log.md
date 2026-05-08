# Symphony evaluation log

This log records ongoing learnings from using OpenAI Symphony directly.

The goal is to keep `aiops-platform` honest by feeding back what the upstream
reference model does well, where it falls short for our personal-productivity
use cases, and which gaps deserve to become tracked work in this repo.

It complements the existing direction docs:

- [ADR 0001: Adopt a Symphony-style personal orchestrator](../adr/0001-symphony-style-personal-orchestrator.md)
- [Research: Symphony-style personal productivity](symphony-personal-productivity.md)
- [Symphony integration guide](../symphony-integration.md)

## How to use this log

Append a new entry every time you run a meaningful Symphony session, in
reverse chronological order (newest first). Keep entries small and concrete.
A short entry recorded today is more valuable than a perfect entry written
later.

Each entry should answer four questions:

1. What did I try?
2. What worked?
3. What did not work?
4. What does this mean for `aiops-platform`?

If an entry surfaces a concrete change, link or open a follow-up issue and
note its number under "Issue follow-ups" so the finding is not lost.

## Entry template

Copy this block when adding a new entry.

```markdown
### YYYY-MM-DD - <short title>

- Symphony setup:
  - Issue tracker:
  - Repo or workspace:
  - Agent or runner:
  - Notable settings:
- Task attempted:
- What worked:
- What did not work:
- Diff vs `aiops-platform`:
- Issue follow-ups:
  - [ ] (placeholder) open issue for ...
```

## How to convert findings to issues

When an entry exposes something `aiops-platform` should add, change, or
explicitly decline to support, turn it into an issue so the work can be
scheduled.

1. Identify the smallest standalone change that captures the finding.
2. Decide the rough milestone and priority based on existing labels in this
   repo (`area:*`, `milestone:m*`, `priority:p*`, `type:*`).
3. Open the issue from the CLI, linking back to this log:

   ```bash
   gh issue create \
     --repo xrf9268-hue/aiops-platform \
     --title "<concise change>" \
     --label "<area>,<milestone>,<priority>,<type>" \
     --body "Source: docs/research/symphony-evaluation-log.md entry YYYY-MM-DD.

   Context:
   - <what Symphony did>
   - <gap in aiops-platform>

   Proposal:
   - <smallest change that closes the gap>

   Acceptance criteria:
   - <observable outcome>"
   ```

4. Update the originating log entry under "Issue follow-ups" with the issue
   number so the trail is reversible.

If the finding is a non-goal (Symphony does X, we explicitly will not), still
record it here, but instead of opening an issue, link the relevant ADR or
research note that captures the boundary decision.

## Entries

### YYYY-MM-DD - placeholder, replace with first real session

- Symphony setup:
  - Issue tracker: _to be filled by first real evaluation_
  - Repo or workspace: _to be filled_
  - Agent or runner: _to be filled_
  - Notable settings: _to be filled_
- Task attempted: _to be filled_
- What worked: _to be filled_
- What did not work: _to be filled_
- Diff vs `aiops-platform`: see
  [ADR 0001](../adr/0001-symphony-style-personal-orchestrator.md) for the
  current intentional differences (Gitea-first, Go implementation, local
  policy, draft PR handoff). New diffs discovered during evaluation should
  be recorded here and, when actionable, converted into issues using the
  process above.
- Issue follow-ups:
  - [ ] (placeholder) open follow-up issue once first real entry exists

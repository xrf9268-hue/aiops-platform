# release-please: align repo on Conventional Commits (changelog + version accuracy)

## Goal

The repo's release notes and version bumps are wrong because most commits don't
use Conventional Commit types. release-please (`release-type: go`) only acts on
recognized types; the repo squash-merges PR titles that mostly use freeform
`area:` prefixes (`maintainability:`, `cmd:`, `stateapi:`, `dashboard:`,
`security:`, `hardening:`, `orchestrator:`, `release:`, …), which release-please
**drops entirely** (no changelog entry, no version bump). Meanwhile the config
makes `chore`/`refactor` **visible**, so the user-facing CHANGELOG is polluted
with Trellis bookkeeping while real features are invisible.

Adopt Conventional Commits across the repo (decision A, user-confirmed
2026-06-14), fix the changelog-sections divergence, and enforce CC at the
PR-title layer so the squash subject release-please parses is always valid.

## Source-verified mechanics (research/)

- Bump is decided ONLY by `feat`→minor / breaking→major; `fix`→patch fallthrough; **`perf`/`chore`/`refactor`/unknown types never bump** (`release-please-bump-changelog-mechanics.md`, `versioning-strategies/default.ts:84-104`). No config/footer makes a custom type bump (only `BREAKING CHANGE`/`!` or `Release-As:`).
- A window of only hidden/unknown-type commits renders **empty notes → release-please silently skips the Release PR** (`strategies/base.ts:331-338,525-527`).
- Default visible set = feat/fix/perf/revert; `chore`,`refactor`,`docs`,`style`,`test`,`build`,`ci` hidden. **Unknown types are dropped, not bucketed** (preset `writer-opts.js`).
- Upstream dogfood ships **no** `changelog-sections` (inherits the default that hides chore/refactor). Our visible `chore`+`refactor` is the one real divergence.
- Migration is **safe**: changelog updater is prepend-only (`updaters/changelog.ts`) — past entries (v0.1.0–v0.1.2) are never rewritten; the open 0.1.3 Release PR re-renders under the new sections on the next `push:main` (chore lines drop out); version unaffected.
- Squash setting currently `squash_merge_commit_title=COMMIT_OR_PR_TITLE` → a single-commit PR uses the **commit** subject, not the PR title. Best practice (and required for PR-title enforcement to bind what release-please parses) = `PR_TITLE`.
- No in-tree project version constant (only unrelated `CodexProtocolVersion`) → **no `extra-files`**; ldflags stamping stays the version mechanism.

## Requirements / Plan

1. **`release-please-config.json`**: delete the `changelog-sections` override (inherit the upstream default → hides chore/refactor/docs/etc.) and delete the now-inert `bootstrap-sha` (upstream: "should generally be avoided"). Keep `release-type:go`, `include-component-in-tag:false`, both pre-1.0 bump flags, `initial-version`, `packages`.
2. **New `.github/workflows/pr-title-lint.yml`**: `amannn/action-semantic-pull-request@48f256284bd46cdaab1048c3721360e808335d50 # v6.1.1`, `pull_request_target` [opened, reopened, edited, synchronize], `permissions: pull-requests: read`, `if: github.repository == 'xrf9268-hue/aiops-platform'`, allow-list the canonical 11 types, `requireScope: false`. Job name `Validate PR title (Conventional Commits)`.
3. **`.github/governance/main-ruleset.json`**: add `{ "context": "Validate PR title (Conventional Commits)" }` to `required_status_checks` (only open PR is the release bot, title already valid → safe to require).
4. **Live repo setting**: `gh api -X PATCH repos/… -f squash_merge_commit_title=PR_TITLE` so the squash subject == PR title == what release-please parses + the linter validates. (Flagged; reversible.)
5. **`AGENTS.md`**: add a "Commit / PR-title convention" rule — PR titles MUST be Conventional Commits (the canonical 11 types), because squash-merge makes the PR title the release-please-parsed subject; mistyped titles vanish from the changelog and don't bump. Include the type→when→changelog table.
6. **`docs/engineering-rules-rationale.md`**: provenance entry for the new rule (observed failure: v0.1.3 changelog omits #828/#829/#834 etc. while listing 4 Trellis-archive chores).
7. **`docs/runbooks/ci.md`**: document the PR-title lint, the `PR_TITLE` squash setting, and the changelog-sections fix.

## Acceptance Criteria

- [ ] `release-please-config.json` has no `changelog-sections` and no `bootstrap-sha`; `--print-config`/schema still valid; JSON parses.
- [ ] PR-title lint workflow present, SHA-pinned, fork-safe, types allow-list = canonical 11.
- [ ] Ruleset lists the new required check context (exact job-name match).
- [ ] Live squash setting = `PR_TITLE` (verified via `gh api`).
- [ ] AGENTS.md + rationale + ci.md updated; type list documented.
- [ ] This PR's OWN title is a valid Conventional Commit (`ci: …`).
- [ ] CI green; `@codex review` clean.

## Out of Scope

- Rewriting already-published CHANGELOG entries (v0.1.0–v0.1.2 chore pollution) — prepend-only updater leaves them; cosmetic cleanup is a separate optional commit.
- Retroactively re-typing historical commits.
- Adding `deps` as a type (use `chore(deps):`/`build(deps):` scope instead).
- Changing the bump flags or release.yml handoff (both already correct).

## Follow-ups (tracked as GitHub issues — handle after this PR)

- **#840 — import the updated ruleset (post-merge, REQUIRED for the gate to bind).** The committed `main-ruleset.json` is not auto-applied; after #838 merges and the workflow is on `main`, re-import via `gh api PUT …/rulesets/17166171`. Must come *after* merge or it deadlocks open PRs.
- **#839 — read-only ruleset drift-check (separate PR, user-confirmed).** Add a CI job that GETs the live ruleset and diffs it against the committed JSON, failing on drift; **no** auto-apply (privilege/least-privilege — research in `research/ruleset-as-code-best-practices.md`).
- **#841 — (low) strip chore/refactor noise from published CHANGELOG v0.1.1/v0.1.2.** Cosmetic; prepend-only updater won't fix history. Close-as-wontfix acceptable.

## Mid-implementation additions (codex review on #838)

- **Dependabot prefix fix (P2):** `.github/dependabot.yml` emitted `deps:` titles (not an allowed type) → would brick every Dependabot PR under the new required lint. Changed all three ecosystems to `chore(deps)`; guarded by `TestDependabotPrefixesAreConventionalCommitTypes` + `TestIsConventionalCommitPrefix` (mutation-verified).
- **Trigger fix (P1):** the lint workflow used `pull_request_target`, whose check reports on the base SHA → a required context would sit "pending" forever. Switched to `pull_request` (attaches to head SHA; matches `pr-metadata.yml` and the "no pull_request_target" invariant in ci.md).

## Decision (ADR-lite)

**Context:** custom commit prefixes are silently dropped by release-please; the config surfaces the wrong types. **Decision (A):** adopt Conventional Commits, hide chore/refactor (inherit upstream default), enforce at the PR-title layer (the squash subject), and set `squash_merge_commit_title=PR_TITLE`. **Rejected B** (extend changelog-sections to keep custom prefixes): source-proven it gives changelog presence but never severity-accurate bumps — custom types can't bump. **Consequences:** all contributors (incl. the agent) must title PRs as Conventional Commits going forward; a new required gate enforces it.

## Technical Notes / References

- `research/release-please-bump-changelog-mechanics.md` — source-cited bump/changelog/parse behavior (v17.9.0).
- `research/release-please-best-practices.md` — config comparison, recommended sections, the pinned PR-title-lint workflow, type list, migration safety.
- Upstream clone (temporary): `/tmp/release-please-src` (v17.9.0).
- Seams: `release-please-config.json`, `.github/workflows/pr-title-lint.yml` (new), `.github/governance/main-ruleset.json`, `AGENTS.md`, `docs/engineering-rules-rationale.md`, `docs/runbooks/ci.md`.

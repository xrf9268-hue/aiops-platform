# Research: release-please bump + changelog mechanics (source-level, v17.9.0)

- **Query**: Prove from release-please v17.9.0 source exactly how commit types map to version bumps and changelog visibility, so we can fix aiops-platform's non-Conventional-Commit subjects (`maintainability:`, `cmd:`, etc.) correctly.
- **Scope**: external (source-level investigation of cloned release-please + its preset dependency)
- **Date**: 2026-06-14
- **Sources**:
  - `release-please` v17.9.0, cloned at `/tmp/release-please-src` (declared in `package.json:2` `"version": "17.9.0"`).
  - Default changelog preset `conventional-changelog-conventionalcommits` v6.1.0 (release-please declares `^6.0.0` in `package.json`; 6.1.0 is the resolved upstream), cloned at `/tmp/ccc-preset/packages/conventional-changelog-conventionalcommits` (`package.json` `"version": "6.1.0"`). This is an EXTERNAL dependency, not vendored in the release-please clone — line numbers below are from the upstream tag `conventional-changelog-conventionalcommits-v6.1.0`.
  - Commit parser is `@conventional-commits/parser` `^0.4.1` (imported at `src/commit.ts:23`), NOT the older `conventional-commits-parser`. It is an AST/unist-tree parser per the conventionalcommits.org grammar.

> **Bottom line (the verdict this settles):** A commit whose subject type is NOT one of release-please's recognized Conventional-Commit types (`feat`/`feature`, `fix`, `perf`, `revert`, `docs`, `style`, `chore`, `refactor`, `test`, `build`, `ci`, plus `deps` only if added via `changelog-sections`) is treated as an **unknown type**: it contributes **nothing** to the version bump AND is **dropped from the changelog entirely** (not bucketed). All of aiops-platform's custom prefixes (`maintainability:`, `cmd:`, `stateapi:`, `dashboard:`, `security:`, `hardening:`, `orchestrator:`, `release:`, `workflow:`, `workspace:`, `tests:`, `deps:`) are unknown types under the default config — except `deps:` is registered in the default heading map but is still hidden-by-absence in the writer's default `types` list (see Q4). There is NO config flag, footer, or plugin that makes an arbitrary custom type bump a version. The only escape hatches are (a) renaming to a real CC type, (b) adding the type to `changelog-sections` (changelog visibility ONLY, still no bump), or (c) a per-commit `Release-As:` footer / global `release-as` (forces an explicit version, type-independent).

---

## Findings

### Files Found

| File Path | Description |
|---|---|
| `/tmp/release-please-src/src/commit.ts` | Conventional-commit parsing: AST walk, type/scope/breaking extraction, squash splitting, `Release-As` footer note |
| `/tmp/release-please-src/src/versioning-strategy.ts` | `Major/Minor/Patch/CustomVersionUpdate` updaters + `VersioningStrategy` interface |
| `/tmp/release-please-src/src/versioning-strategies/default.ts` | `DefaultVersioningStrategy.determineReleaseType` — the only-feat/fix/breaking bump logic + pre-major flags + `Release-As` |
| `/tmp/release-please-src/src/versioning-strategies/always-bump-patch.ts` | `AlwaysBumpPatch extends DefaultVersioningStrategy`, forces patch (still inherits `Release-As`) |
| `/tmp/release-please-src/src/strategies/base.ts` | `buildReleasePullRequest` — the "should we release / skip" decision; `changelogEmpty`; `buildNewVersion`; default wiring of versioning + changelog notes |
| `/tmp/release-please-src/src/strategies/go.ts` | `Go extends BaseStrategy`; does NOT override `versioningStrategy` or `changelogNotes` → uses defaults |
| `/tmp/release-please-src/src/changelog-notes/default.ts` | `DefaultChangelogNotes.buildNotes` — feeds commits to the `conventionalcommits` preset writer |
| `/tmp/release-please-src/src/changelog-notes.ts` | `ChangelogSection` interface (`{type, section, hidden?}`); `DEFAULT_HEADINGS` map; `buildChangelogSections` |
| `/tmp/release-please-src/src/factories/versioning-strategy-factory.ts` | Maps `versioning` config → strategy class (`default`, `always-bump-patch`, …) |
| `/tmp/release-please-src/src/manifest.ts` | Wires `bump-minor-pre-major` / `bump-patch-for-minor-pre-major` config keys into the strategy options |
| `/tmp/ccc-preset/packages/conventional-changelog-conventionalcommits/writer-opts.js` | **External** preset: the default `types` array (which types are hidden) + the `transform` that DROPS unknown/hidden commits |

---

## Q1 — Commit parsing (`src/commit.ts`)

**Parser library.** `import * as parser from '@conventional-commits/parser'` (`src/commit.ts:23`). This is an AST parser (unist tree); release-please walks the tree with `unist-util-visit` / `unist-util-visit-parents` (`src/commit.ts:16-18`) and converts the AST into the legacy "conventional-changelog" object shape via `toConventionalChangelogFormat()` (`src/commit.ts:81-311`).

**Type & scope extraction.** Walking the `summary` (header) node (`src/commit.ts:102-122`):
- `case 'type'` → `headerCommit.type = node.value` (`:104-106`)
- `case 'scope'` → `headerCommit.scope = node.value` (`:108-110`)
- `case 'breaking-change'` (the `!` marker in the header) → appends `'!'` to the rebuilt header (`:112-114`)
- `case 'text'` → `headerCommit.subject = node.value` (`:115-117`)

So `type`/`scope` come from the grammar's structural nodes, not a hand-rolled regex. The grammar enforces the canonical `<type>(<scope>)!: <subject>` header shape; a header that does not match (e.g. no `:` separator, or a malformed prefix) yields an empty/odd parse. **There is no allow-list of types at parse time** — `maintainability: foo` parses with `type = "maintainability"`. The type allow-list is applied later (bump logic Q2, changelog Q4).

**Bang (`type!:`) detection.** The header `!` is a `breaking-change` node inside `summary`. When the parser also re-parses the rebuilt footer/notes, the `!` is resolved to a BREAKING CHANGE note (`src/commit.ts:167-175` handles the `token` parent case; the comment at `:168-169` states "If the '!' breaking change marker is used, the breaking change will be identified when the footer is parsed as a commit"). The final `breaking` boolean is computed in `parseConventionalCommits` as `parsedCommit.notes.filter(note => note.title === 'BREAKING CHANGE').length > 0` (`src/commit.ts:426-428`).

**`BREAKING CHANGE:` / `BREAKING-CHANGE:` footers.** Detected in three places:
1. AST walk of `breaking-change` nodes across summary/body/footer (`src/commit.ts:137-178`).
2. An explicit regex fallback on the body string: `bodyString.match(/BREAKING-CHANGE:\s*(.*)/)` (`src/commit.ts:183`).
3. A second regex fallback in `postProcessCommits`: `commit.body?.match(/BREAKING-CHANGE:\s*(.*)/)` (`src/commit.ts:337`).
Note the hyphenated `BREAKING-CHANGE:` is regex-matched; the spaced `BREAKING CHANGE:` form is handled by the grammar's `breaking-change` node. Both end up as a note titled `'BREAKING CHANGE'` (`src/commit.ts:133-135, 345-348`).

**Squash-merge: is the SUBJECT parsed as the header, and is the body scanned?** Yes to both.
- The whole commit message string is parsed (`parseCommits(message)` → `parser.parser(message)`, `src/commit.ts:373`). For a squash-merged PR, GitHub's squash commit message is `"<PR title>\n\n<PR body>"`, so the PR-title-derived subject line is the conventional header, and the body is parsed too (body text, BREAKING CHANGE footers, `Release-As`, references).
- `preprocessCommitMessage` (`src/commit.ts:456-469`) additionally lets a PR override the parsed message: if the PR **body** contains a `BEGIN_COMMIT_OVERRIDE` … `END_COMMIT_OVERRIDE` block, that block REPLACES the commit message for parsing (`:458-466`). This is how a non-conventional squash title can be overridden into a conventional message without changing the title.

**Multiple conventional commits out of ONE squashed body — yes, two mechanisms:**
1. **`splitMessages`** (`src/commit.ts:388-403`): splits the message on blank lines that are immediately followed by a conventional header, using regex
   `/\r?\n\r?\n(?=(?:feat|fix|docs|style|refactor|perf|test|build|ci|chore|revert)(?:\(.*?\))?: )/` (`src/commit.ts:399`).
   **Critical:** this split regex is a HARD-CODED list of the standard CC types. A blank-line-separated block starting with `maintainability:` is **NOT** a split point — it stays glued to the preceding block and is not promoted to its own commit. Custom types get no squash-splitting.
   `splitMessages` also extracts `BEGIN_NESTED_COMMIT` … `END_NESTED_COMMIT` blocks (`src/commit.ts:389-396`).
2. **Footer-as-commit** (`src/commit.ts:251-308`): footers that themselves look like commits (e.g. a `chore(...)` line in the trailer) are recursively re-parsed and pushed as additional commits (`:298-302`).

**`Release-As:` footer (parse side).** A footer whose token lowercases to `release-as` (`src/commit.ts:262`) is recorded as a note `{title: 'RELEASE AS', text: <version>}` (`src/commit.ts:289-293`) and also written into `headerCommit.footer` (`:294-295`). Consumed by the bump logic and changelog (Q2).

---

## Q2 — Version bump logic (`src/versioning-strategies/default.ts`)

`Go` (`release-type: go`) extends `BaseStrategy` without overriding the strategy, and `base.ts` defaults to `new DefaultVersioningStrategy(...)` (`src/strategies/base.ts:138-140`). So aiops-platform runs `DefaultVersioningStrategy`.

The decision is `determineReleaseType(version, commits)` (`src/versioning-strategies/default.ts:66-105`). Walking the commits:

```ts
// default.ts:73-89
for (const commit of commits) {
  const releaseAs = commit.notes.find(note => note.title === 'RELEASE AS');
  if (releaseAs) { ... return new CustomVersionUpdate(...); }   // :74-83
  if (commit.breaking) { breaking++; }                          // :84-85
  else if (commit.type === 'feat' || commit.type === 'feature') { features++; }  // :86-88
}
```

Then (`default.ts:91-104`):

```ts
if (breaking > 0) {
  if (version.isPreMajor && this.bumpMinorPreMajor) return new MinorVersionUpdate();   // :92-93
  else return new MajorVersionUpdate();                                                // :94-95
} else if (features > 0) {
  if (version.isPreMajor && this.bumpPatchForMinorPreMajor) return new PatchVersionUpdate();  // :98-99
  else return new MinorVersionUpdate();                                                       // :100-102
}
return new PatchVersionUpdate();   // :104  <-- fallthrough
```

**Exact type→bump mapping (DefaultVersioningStrategy):**
- **breaking** (`commit.breaking === true`, from a `!` or `BREAKING CHANGE` note, on ANY type) → major (or minor if pre-1.0 and `bump-minor-pre-major`).
- **`feat` / `feature`** (and only these literal strings) → minor (or patch if pre-1.0 and `bump-patch-for-minor-pre-major`).
- **everything else** — `fix`, `perf`, `docs`, `chore`, `refactor`, `deps`, `revert`, AND every unknown type like `maintainability` — contributes nothing to `breaking`/`features`. It falls through to the **`return new PatchVersionUpdate()` at `default.ts:104`**.

**Important nuance about `fix` / `perf` / "patch":** Note `determineReleaseType` does NOT count `fix` or `perf` explicitly. It counts only breaking + feat. Any commit list that reaches `buildNewVersion` (i.e. is non-empty after filtering — see Q3) and has neither breaking nor feat returns a **patch bump by fallthrough** regardless of whether the commit was `fix`, `chore`, or `maintainability`. **`perf` does NOT get special bump treatment in release-please's bump code** — it patches by the same fallthrough as everything else. The thing that prevents a pure-`chore`/`maintainability` window from releasing is NOT the bump code (it would happily return patch); it's the **changelog-empty gate** in Q3.

**Is there ANY way for a custom/unknown type to trigger a bump?**
- Not via type name: only the literals `feat`/`feature` (minor) and the `breaking` flag (major) are matched (`default.ts:86, 84`). No config flag adds custom bump types to `DefaultVersioningStrategy`.
- Not via `changelog-sections`: that only feeds the changelog writer (Q4); it is never read by `determineReleaseType`.
- **A breaking footer DOES work on any type.** `maintainability!: x` or a `BREAKING CHANGE:` footer sets `commit.breaking`, so the commit bumps major/minor even though `maintainability` is unknown. This is the one type-independent way to force a real bump tied to content.
- **`Release-As:` footer / `release-as` config — yes, explicit override.**
  - Per-commit footer `Release-As: 1.2.3` → note `RELEASE AS` → `CustomVersionUpdate` returns exactly that version (`default.ts:74-83`); also short-circuited earlier in `buildNewVersion` (`src/strategies/base.ts:554-564`). Type-independent.
  - Global manifest `release-as` config → `buildNewVersion` returns `Version.parse(this.releaseAs)` first (`src/strategies/base.ts:547-552`).
  - `AlwaysBumpPatch` (`versioning: always-bump-patch`) still extends `DefaultVersioningStrategy` and only overrides `determineReleaseType` to always patch (`src/versioning-strategies/always-bump-patch.ts:24-31`); `buildNewVersion`'s `Release-As` short-circuit still applies.

**Config-flag wiring (pre-major).** `bump-minor-pre-major` / `bump-patch-for-minor-pre-major` config keys (`src/manifest.ts:164-165`) → `bumpMinorPreMajor` / `bumpPatchForMinorPreMajor` options (`src/manifest.ts:1393-1394`, `:1746-1750`) → `DefaultVersioningStrategyOptions` (`src/versioning-strategies/default.ts:27-31, 50-53`). `version.isPreMajor` is the `< 1.0.0` predicate used at `default.ts:92, 98`.

For aiops-platform (pre-1.0, both flags on): **breaking → minor**, **feat → patch**, **everything else (that survives the Q3 gate) → patch**.

---

## Q3 — Does an unknown-type-only window even open a Release PR?

The gate is in `BaseStrategy.buildReleasePullRequest` (`src/strategies/base.ts:277-375`). Two skip points:

1. **Empty commit list** (`:286-289`): if there are no conventional commits at all and no `bumpOnlyOptions`, return `undefined` (skip). Unknown-type commits are NOT removed here — they still parse into `ConventionalCommit` objects (Q1), so the list is non-empty.

2. **Changelog-empty gate** (`:331-338`) — THIS is the real decider for a pure unknown/chore window:

```ts
// base.ts:324-338
const releaseNotesBody = await this.buildReleaseNotes(conventionalCommits, ...);
if (!bumpOnlyOptions && this.changelogEmpty(releaseNotesBody)) {
  this.logger.info(`No user facing commits found since ... - skipping`);
  return undefined;
}
```

`changelogEmpty` (`src/strategies/base.ts:525-527`):

```ts
protected changelogEmpty(changelogEntry: string): boolean {
  return changelogEntry.split('\n').length <= 1;
}
```

So the release notes are rendered FIRST (via the `conventionalcommits` writer, Q4), and **if the rendered notes are essentially empty (≤1 line), release-please skips and opens no PR** — logging "No user facing commits found … - skipping". Because the writer DROPS hidden+unknown commits (Q4), a window containing only `chore:` (hidden) and `maintainability:`/`cmd:`/etc. (unknown) produces empty notes → **no Release PR is opened, no bump happens.** The bump fallthrough-to-patch in Q2 never fires for these because this gate returns first.

(`bumpOnlyOptions` is only set by the workspace/dependency plugin path — `Strategy` interface doc `src/strategy.ts:46-49` — irrelevant to a normal Go single-package release.)

**Consequence for aiops-platform:** any release window whose commits are all custom-prefixed and/or `chore`/`docs`/`refactor`/etc. (all hidden or unknown) will be SILENTLY SKIPPED — no version bump, no changelog, no Release PR — even though work shipped. Only `feat:` / `fix:` / breaking (and a few non-hidden CC types) produce visible notes and therefore a release.

---

## Q4 — Changelog sections & default visible/hidden set

**How notes are built.** `DefaultChangelogNotes.buildNotes` (`src/changelog-notes/default.ts:51-117`) constructs the preset via `presetFactory(config)` (`:69`) where `config.types = options.changelogSections` ONLY if `changelog-sections` is set (`:65-68`). It then maps each `ConventionalCommit` to a writer record carrying `type: commit.type` (`:98`) and calls `conventionalChangelogWriter.parseArray(changelogCommits, context, preset.writerOpts)` (`:114-115`). The preset is `conventional-changelog-conventionalcommits` (`:25`).

**Default `types` array (when `changelog-sections` is UNSET).** Comes from the preset's `defaultConfig` (external dep, `/tmp/ccc-preset/.../writer-opts.js:176-191`):

```js
config.types = config.types || [
  { type: 'feat',     section: 'Features' },                          // visible
  { type: 'feature',  section: 'Features' },                          // visible
  { type: 'fix',      section: 'Bug Fixes' },                         // visible
  { type: 'perf',     section: 'Performance Improvements' },          // visible
  { type: 'revert',   section: 'Reverts' },                           // visible
  { type: 'docs',     section: 'Documentation',          hidden: true },
  { type: 'style',    section: 'Styles',                 hidden: true },
  { type: 'chore',    section: 'Miscellaneous Chores',   hidden: true },  // <-- chore hidden by default (confirmed)
  { type: 'refactor', section: 'Code Refactoring',       hidden: true },
  { type: 'test',     section: 'Tests',                  hidden: true },
  { type: 'build',    section: 'Build System',           hidden: true },
  { type: 'ci',       section: 'Continuous Integration', hidden: true },
];
```

So **by default: visible = `feat`/`feature`, `fix`, `perf`, `revert`; hidden = `docs`, `style`, `chore`, `refactor`, `test`, `build`, `ci`.** `chore` IS hidden by default (confirmed). Note `deps` is NOT in this writer default list (it appears only in release-please's `DEFAULT_HEADINGS` map at `src/changelog-notes.ts:43-56`, which is used by `buildChangelogSections` to label a section IF you list `deps` in `changelog-sections` — it does not make `deps` visible on its own).

**What happens to a commit whose type is NOT in `types` (unknown type like `maintainability`).** Decided by the preset's `transform` (`/tmp/ccc-preset/.../writer-opts.js:79-160`):

```js
transform: (commit, context) => {
  let discard = true
  const entry = findTypeEntry(config.types, commit)   // :82
  ...
  // Release-As footer keeps it:
  if (... releaseAsRe.test(commit.footer/body)) discard = false   // :91-94
  // BREAKING CHANGE notes keep it:
  commit.notes.forEach(note => { note.title = 'BREAKING CHANGES'; discard = false })  // :96-99
  // the drop:
  if (discard && (entry === undefined || entry.hidden)) return    // :102-103  -> returns undefined => DROPPED
  if (entry) commit.type = entry.section   // :105  relabel type to its section heading
  ...
  return commit
}
```

`findTypeEntry` (`/tmp/ccc-preset/.../writer-opts.js:60-71`) does an exact `entry.type === typeKey` match (lowercased). For `maintainability` there is no entry → `entry === undefined` → the `transform` **returns `undefined`, which conventional-changelog-writer treats as "discard this commit."**

**Therefore: an unknown type is DROPPED from the changelog entirely — NOT bucketed under a "raw type" heading and NOT shown under an "Other"/"Miscellaneous" group.** The only way an unknown-type commit survives into the changelog is if it ALSO carries a `Release-As` footer (`:91-94`) or a `BREAKING CHANGE` note (`:96-99`), in which case `discard` is flipped to `false` and it is rendered (and, for breaking, grouped under BREAKING CHANGES).

`hidden: true` entries (e.g. `chore`) take the same `return` at `:102-103` (`entry.hidden` true) → dropped unless breaking/Release-As.

Note line `:105` `if (entry) commit.type = entry.section`: only commits with a matching, non-hidden entry get relabeled to their human heading and grouped (`groupBy: 'type'`, `:161`). Unknown types never reach this line.

**How `changelog-sections` overrides.** Setting `changelog-sections` replaces the entire default `types` array (`src/changelog-notes/default.ts:65-68` → preset `config.types`). You can:
- Make a normally-hidden type visible: e.g. `{type: 'chore', section: 'Miscellaneous'}` (omit `hidden`).
- Register a CUSTOM type for the CHANGELOG: e.g. `{type: 'maintainability', section: 'Maintainability'}` makes `maintainability:` commits appear under a "Maintainability" heading.
- **But replacing the array means you must re-list EVERY type you want visible** (feat/fix/etc.), or they fall back to being unknown/hidden relative to your new list. (The preset only applies the built-in defaults when `config.types` is falsy — `writer-opts.js:178`.)

---

## Q5 — Can a repo keep custom prefixes AND get them into changelog/bump? (rule option B in/out)

**Changelog: YES (visibility only).** `changelog-sections` (`src/changelog-notes/default.ts:65-68`, preset `transform`/`findTypeEntry`) lets you register custom types so `maintainability:`, `cmd:`, etc. render under custom headings. This makes custom prefixes VISIBLE in CHANGELOG.md.

**Version bump from a custom type: NO.** `changelog-sections` is consumed ONLY by the changelog writer; `DefaultVersioningStrategy.determineReleaseType` (`src/versioning-strategies/default.ts:66-105`) hard-codes `feat`/`feature` (minor) and the `breaking` flag (major) and never reads `changelog-sections` or any custom-type list. There is **no config that maps a custom type → a bump level.** A repo with `changelog-sections` registering `maintainability` would show those commits in the changelog but they would STILL contribute zero to the bump (patch-by-fallthrough only if some OTHER releasable signal already triggered a non-skip — and per Q3 the changelog being non-empty is exactly what un-skips it).

This is the subtle but important interaction for "option B" (keep custom prefixes + register them in `changelog-sections`):
- It WOULD un-skip the release: registered custom types now render to the changelog (`transform` no longer discards them), so `changelogEmpty` is false (Q3) → a Release PR opens.
- The resulting bump would be a **patch** (Q2 fallthrough at `default.ts:104`) for any window lacking `feat`/breaking — even if the custom commit is semantically a feature. You cannot say "`cmd:` should bump minor."
- So option B gives you changelog visibility and "something releases," but it CANNOT express feature-vs-fix-vs-breaking severity for custom types; severity is only ever derived from the literal `feat`/`feature` type and the `breaking` flag/footer.

**The only release-please features that influence bump are:**
1. The commit `type` literal `feat`/`feature` (minor) — `src/versioning-strategies/default.ts:86`.
2. The `breaking` flag from `!` or `BREAKING CHANGE`/`BREAKING-CHANGE` (major/minor), usable on ANY type — `src/commit.ts:426-428`, `src/versioning-strategies/default.ts:84`.
3. `Release-As:` footer / global `release-as` config (explicit exact version, type-independent) — `src/versioning-strategies/default.ts:74-83`, `src/strategies/base.ts:547-564`.
4. The `versioning` strategy choice (`default` vs `always-bump-patch` vs `always-bump-minor`/`-major`) — `src/factories/versioning-strategy-factory.ts:41-42` — these change the FORMULA, not which commit types are "releasable."

No plugin/footer/flag promotes an arbitrary custom type to a specific bump severity.

---

## Caveats / Not Found

- **Preset version pin.** release-please declares `conventional-changelog-conventionalcommits: ^6.0.0` (release-please `package.json`); I verified against the upstream `6.1.0` source (`/tmp/ccc-preset`). The default `types` array has been stable across 6.x; if the lockfile in aiops-platform's release-please action pins a different 6.x patch, re-confirm `writer-opts.js:178-191`, but the visible/hidden split is unlikely to differ. (`node_modules` was not present in the clone, so the exact resolved patch inside a given CI run could not be read directly — the upstream `6.1.0` source is the citation.)
- **`@conventional-commits/parser` internals not deep-read.** I confirmed release-please's USE of it (`src/commit.ts:23`, AST walks) but did not read the parser package's grammar source (also absent from the clone). The behaviors that matter (type/scope/bang/footer extraction) are all re-derived by release-please's own AST walk, which I cited directly.
- **GitHub squash message shape is assumed**, not read from release-please: GitHub composes a squash commit as `"<PR title>\n\n<PR body>"`. release-please parses whatever the merged commit message is (`src/commit.ts:373, 420-425`) plus the optional PR-body `BEGIN_COMMIT_OVERRIDE` block (`:456-469`). The mapping "squash subject == PR title" is GitHub behavior, not release-please code.
- All line numbers for `writer-opts.js` are from the EXTERNAL preset repo at tag `conventional-changelog-conventionalcommits-v6.1.0`, not from `/tmp/release-please-src`.

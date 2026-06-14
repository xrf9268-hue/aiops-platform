# Research: GitHub ruleset / branch-protection as code — authoritative best practice

- **Query**: Should we add a CI mechanism (auto-sync or drift-detection) for `.github/governance/main-ruleset.json`? What do authoritative sources actually do?
- **Scope**: external (web/official docs) + internal (repo governance files)
- **Date**: 2026-06-14

## TL;DR verdict (verdict-first)

**For THIS repo (single-maintainer, AI-agent-driven, SHA-pinned actions, minimal/earned config), the best-practice-aligned recommendation is: keep the current manual-apply + documented model. Optionally add a *read-only* drift-detection check (compare live ruleset to committed JSON, warn/annotate, never mutate). Do NOT add an auto-apply / auto-sync workflow.**

Rationale, in one line each:
- Auto-apply requires an **`Administration: write`** credential (PAT or GitHub App) that the workflow's built-in `GITHUB_TOKEN` **cannot** carry — that is privilege concentration / self-modifying governance, which GitHub's own hardening guidance argues against (least privilege). [GH-RULES-PERM][GH-TOKEN-KEYS][GH-LEASTPRIV]
- The recognized read-only equivalent (`terraform plan -detailed-exitcode`, exit 2 = drift) is endorsed and runs with **read-only** scope — same posture our manual model already has, just automated alerting. [TF-PLAN]
- Full GitOps tools (Terraform / safe-settings / repository-settings) are real, maintained, and correct — but they are **org-scale policy-as-code**; adopting one for a single repo's single ruleset is over-design relative to the toil it removes.

---

## Findings

### 1. Established ways to manage rulesets / branch protection as code

| Approach | Auto-apply or detect-only? | Credential / privilege needed | Maturity / maintenance |
|---|---|---|---|
| **Terraform GitHub provider** (`github_repository_ruleset`, `github_branch_protection`) | **Auto-apply** on `apply` (create/update/destroy); **detect drift** on `plan`/`plan -detailed-exitcode` | PAT or GitHub App with repo **admin / `Administration: write`**; auth chain = `token`/`GITHUB_TOKEN` → App `app_auth` → `gh auth token` → anonymous read-only | v6.x, `v6.12.1` (2026-04-28), ~1.1k★, commits within last day. Actively maintained. [TF-RULESET][TF-BP][TF-INDEX][TF-PLAN] |
| **github/safe-settings** (GitHub's own Probot app) | **Auto-apply AND auto-revert drift** — on `branch_protection_rule` / `repository_ruleset` webhooks it *syncs to prevent unauthorized changes*; scheduled cron re-sync for drift; PR events run in `dry-run`/`nop` to validate. Rulesets supported as `org`-targeted settings. | Self-hosted **GitHub App** with org-wide write incl. branch-protection/administration; central `admin` repo holds config | `2.1.21` (2026-05-12), ~879★, commits within last day. Actively maintained by GitHub. [SS-README] |
| **repository-settings/app** (`.github/settings.yml` Probot app) | **Auto-apply / sync** settings (incl. branch protection) from `.github/settings.yml` to GitHub | Hosted `github.com/apps/settings` GitHub App (or self-hosted) with write to repo settings/branch protection | `v5.0.11` (2026-06-04), ~1.0k★, commits within last day. Maintained. **Carries an explicit security caveat** (see §3). [RS-README] |
| **Hand-rolled `gh api` workflow** | **Either** — `--method GET` (detect) or `--method PUT/POST` (apply). Our repo's README documents the manual `PUT` form. | For **apply** of a repository ruleset: **`Administration: write`** (PAT/App) — *not* grantable to the workflow `GITHUB_TOKEN`; for **read**: `Metadata: read` only | N/A (DIY). Our `.github/governance/README.md` already documents the manual `gh api ... --method PUT ... --input main-ruleset.json` flow. [GH-RULES-PERM][REPO-README] |
| **Read-only drift-detection check in CI** | **Detect only** — `gh api .../rulesets/<id>` (GET) vs committed JSON, normalize, `diff`, warn/annotate, **never mutate** | **Read-only**: `Metadata: read` (rulesets GET works with metadata-read). Can run on the built-in `GITHUB_TOKEN` with no admin scope. | Recognized pattern; the Terraform-native form is `plan -detailed-exitcode` (exit 2 = drift). [GH-RULES-PERM][TF-PLAN] |

**Key permission fact (load-bearing for the whole decision):** the GitHub REST docs state that *Create / Update a repository ruleset* requires `"Administration" repository permissions (write)`, while *Get* a ruleset needs only `"Metadata" repository permissions (read)`. [GH-RULES-PERM] The workflow `GITHUB_TOKEN`'s grantable `permissions:` keys are: `actions, attestations, checks, contents, deployments, id-token, issues, models, discussions, packages, pages, pull-requests, security-events, statuses, vulnerability-alerts` — **`administration` is not in that set.** [GH-TOKEN-KEYS] So any auto-apply workflow must import a separate admin-scoped PAT/App secret; a read-only drift check does not.

### 2. Is read-only drift detection a recognized / endorsed pattern?

Yes. It is the standard "plan, don't apply" CI posture:

- **Terraform's own CLI** ships `terraform plan -detailed-exitcode` specifically for this: it changes exit codes to `0 = empty diff (no changes)`, `1 = error`, `2 = non-empty diff (changes present)`. A CI job that fails/warns on exit 2 is the canonical read-only drift-detection check for any Terraform-managed resource, including `github_repository_ruleset` / `github_branch_protection`. The HashiCorp docs frame drift as "drift and confusion about how the true state of resources relates to configuration," with `plan` as the detection tool. [TF-PLAN]
- **safe-settings** runs its PR-time validation in `dry-run`/`nop` mode — i.e. it *computes and reports the diff without applying* on non-default branches — which is the same detect-don't-mutate idea applied at review time. [SS-README]

Detect-only vs full GitOps auto-apply is a deliberate trade, not a deficiency: detect-only keeps the apply action gated behind a human with admin rights (matching our current manual model), while still catching out-of-band UI edits. Full auto-apply removes the human but concentrates an admin credential in CI (§3).

### 3. Security view — admin/ruleset-write scope in CI

GitHub's own guidance is least-privilege-first:

- **"It's good security practice to set the default permission for the `GITHUB_TOKEN` to read access only for repository contents. The permissions can then be increased, as required, for individual jobs."** — GitHub Actions secure-use reference. [GH-LEASTPRIV] The same page leads with "**Principle of least privilege** … ensure that the credentials being used within workflows have the least privileges required," and warns that any user with write access to the repo can read its secrets — so a stored **admin PAT** is exposed to everyone with repo write. [GH-LEASTPRIV]
- The built-in `GITHUB_TOKEN` **cannot** be granted `administration` (not in the grantable keys [GH-TOKEN-KEYS]; not in the default scope table [GH-TOKEN-DEFAULT]). An auto-apply ruleset workflow therefore *must* inject an admin-scoped PAT or GitHub App private key as a secret. That is precisely the privilege-concentration / self-modifying-governance pattern hardening guides discourage: the automation that enforces branch protection also has the power to rewrite or remove it, and that power now lives in a CI secret rather than behind a human admin's interactive session.
- **repository-settings/app documents this risk explicitly**: *"this app inherently escalates anyone with `push` permissions to the **admin** role, since they can push config settings to the default branch, which will be synced. Use caution when merging PRs and adding collaborators."* Its mitigation is CODEOWNERS + required code-owner review on the settings file. [RS-README] The same escalation logic applies to any auto-sync of a governance file: whoever can land a change to the file effectively gains admin-over-branch-protection.

Net: an auto-apply workflow that can modify branch protection **is** a recognized risky pattern (privilege concentration + a self-modifying control). It is justified at org scale where the toil of manual application across many repos is large; it is hard to justify for one repo with one ruleset.

### 4. Proportionality verdict for THIS repo

Context: single-maintainer / agent-driven, SHA-pinned actions, `required_approving_review_count: 0`, values minimal/earned config and small attack surface, ruleset is **already** committed as source-of-truth and applied manually via documented `gh api ... PUT`. [REPO-README][REPO-JSON]

**Recommendation: keep manual-apply + documented (current state). Optionally add a read-only drift check. Do not auto-apply.**

Trade-offs across the three options:

- **Leave manual + documented (current):** zero new privilege, zero new attack surface, zero CI complexity. Residual risk = silent drift if someone edits the ruleset in the UI and forgets to re-export, or edits the JSON and forgets to re-import. For a single maintainer who is the only admin, that drift window is small and self-inflicted — low real risk. **Most proportional.**
- **Add read-only drift check (recommended if you want automation):** runs on the default `GITHUB_TOKEN` with **read-only** scope (`Metadata: read` is enough to GET the ruleset [GH-RULES-PERM]); no admin secret; cannot mutate anything. Cost = one small workflow + JSON normalization. Benefit = closes the "edited one side, forgot the other" gap by annotating/failing a scheduled or PR check. Aligned with `terraform plan -detailed-exitcode` semantics. **Proportional and cheap; the only option that adds value without adding privilege.**
- **Adopt Terraform / safe-settings / repository-settings full GitOps:** correct and maintained, but each requires an **admin-scoped credential reachable from automation** (Administration: write), pulls in a provider/state file or a hosted/self-hosted GitHub App, and is designed for *many* repos/rulesets. For one repo + one ruleset this is over-design: high privilege + high complexity to remove a tiny amount of toil. **Not proportional here.** (Matches AGENTS.md harness principle 1 "name the behavior" / principle 5 "few sharp tools" — full GitOps's nameable behavior is multi-repo policy enforcement, which this repo does not have.)

#### Sketch of the recommended read-only drift check (if adopted)

Behavior: on a schedule (and/or on PRs touching `.github/governance/**`), GET the live ruleset, normalize both sides, diff, and **fail/annotate on divergence — never PUT**.

- **Permissions:** `permissions: { contents: read }` — the built-in `GITHUB_TOKEN`. Rulesets GET works with `Metadata: read`; no PAT, no `administration`, no secret. [GH-RULES-PERM][GH-TOKEN-KEYS]
- **Fetch live:** `gh api repos/$REPO/rulesets/<id>` (look up `<id>` once via `gh api repos/$REPO/rulesets --jq '.[]|select(.name=="main merge governance").id'`).
- **Normalize before compare** (the live payload carries server-managed fields the committed file omits): drop `id`, `node_id`, `created_at`, `updated_at`, `_links`, `current_user_can_bypass`, and `source`/`source_type`; sort object keys and any arrays (`rules`, `required_status_checks`, `bypass_actors`) deterministically; compare only the fields the committed JSON declares (`name`, `target`, `enforcement`, `conditions`, `bypass_actors`, `rules`). Use `jq -S` (sort keys) + a canonical projection, e.g. `jq -S '{name,target,enforcement,conditions,bypass_actors,rules}'` on both sides, then `diff`.
- **Failure mode:** non-zero exit (or `::warning::`) with the diff printed, mirroring `terraform plan -detailed-exitcode` exit 2 = "changes present." [TF-PLAN]
- **What it deliberately does NOT do:** it never calls `--method PUT/POST`, so it needs no admin scope and cannot itself become a governance-rewrite vector.

### Internal context (repo files inspected)

| File | Relevance |
|---|---|
| `.github/governance/main-ruleset.json` | The committed source-of-truth ruleset (`main merge governance`): squash-only, 4 required checks, review-thread resolution, no force-push/deletion, `required_approving_review_count: 0`. [REPO-JSON] |
| `.github/governance/README.md` | Documents that the JSON is **not** auto-applied; admin imports/re-imports via `gh api --method POST/PUT ... --input`. Already states "A committed JSON file is **not** auto-applied — GitHub stores rulesets server-side." [REPO-README] |

---

## Caveats / Not Found

- I could not run a general web search engine (WebSearch and the exa MCP tools were unavailable in this environment). All external facts were pulled **directly from authoritative source repos/docs** via `gh api` / `curl` against GitHub Docs source (`github/docs`), the Terraform provider repo (`integrations/terraform-provider-github`), HashiCorp's CLI docs, and the canonical app READMEs (`github/safe-settings`, `repository-settings/app`). This is *more* authoritative than blog aggregation, but I did not survey practitioner blog write-ups by name; the "who does drift-detection-only" claim rests on the first-party Terraform `-detailed-exitcode` mechanism and safe-settings' dry-run mode rather than a named practitioner essay.
- CIS GitHub Benchmark / OpenSSF Scorecard: I confirmed GitHub's own least-privilege guidance and that Scorecard checks "token permissions" and flags over-broad workflow tokens [GH-LEASTPRIV], but I did not fetch the CIS benchmark PDF directly (not openly retrievable via the tools here). The least-privilege conclusion does not depend on it.
- The exact numeric ruleset `<id>` for this repo is not in the committed JSON (it is server-assigned); a drift check must look it up by name at runtime.

## Sources

- [GH-RULES-PERM] GitHub REST API — Repository rules: *Create/Update a repository ruleset* requires `"Administration" repository permissions (write)`; *Get* requires `"Metadata" repository permissions (read)`. https://docs.github.com/en/rest/repos/rules?apiVersion=2022-11-28
- [GH-TOKEN-KEYS] GitHub Docs source — grantable `GITHUB_TOKEN` `permissions:` keys (no `administration`). `github/docs` → `data/reusables/actions/github-token-available-permissions.md`.
- [GH-TOKEN-DEFAULT] GitHub Docs — Automatic token authentication (default `GITHUB_TOKEN` scope table; `administration` absent). https://docs.github.com/en/actions/security-for-github-actions/security-guides/automatic-token-authentication
- [GH-LEASTPRIV] GitHub Docs — Security hardening / secure-use reference: "Principle of least privilege"; "good security practice to set the default permission for the `GITHUB_TOKEN` to read access only"; users with write access can read repo secrets; Scorecard flags token permissions. `github/docs` → `content/actions/reference/security/secure-use.md`.
- [TF-RULESET] Terraform GitHub provider — `github_repository_ruleset` resource ("When applied, a new ruleset will be created. When destroyed, that ruleset will be removed."). `integrations/terraform-provider-github` → `docs/resources/repository_ruleset.md`.
- [TF-BP] Terraform GitHub provider — `github_branch_protection` resource. `integrations/terraform-provider-github` → `docs/resources/branch_protection.md`.
- [TF-INDEX] Terraform GitHub provider — auth chain (token/`GITHUB_TOKEN` → App `app_auth` → `gh auth token` → anonymous read-only). `integrations/terraform-provider-github` → `docs/index.md`.
- [TF-PLAN] HashiCorp — `terraform plan` `-detailed-exitcode`: `0` empty diff / `1` error / `2` changes present; drift detection framing. https://developer.hashicorp.com/terraform/cli/commands/plan
- [SS-README] `github/safe-settings` README — "Policy-as-Code for GitHub Organizations"; `repository_ruleset` / `branch_protection_rule` webhooks "sync … to prevent any unauthorized changes"; scheduled sync for drift prevention; PR `dry-run`/`nop` validation; rulesets as `org`-targeted settings. `v2.1.21` (2026-05-12), maintained.
- [RS-README] `repository-settings/app` README — syncs `.github/settings.yml` to GitHub; Security Implications: "this app inherently escalates anyone with `push` permissions to the **admin** role … Use caution"; mitigation = CODEOWNERS + required code-owner review. `v5.0.11` (2026-06-04), maintained.
- [REPO-README] In-repo: `.github/governance/README.md` — manual import/re-import via `gh api --method POST/PUT … --input`; "not auto-applied."
- [REPO-JSON] In-repo: `.github/governance/main-ruleset.json` — committed source-of-truth ruleset.

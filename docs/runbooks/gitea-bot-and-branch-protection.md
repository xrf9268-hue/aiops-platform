# Gitea bot and branch protection runbook

This runbook documents the minimum safe Git settings for running aiops-platform against a Gitea instance. Follow this before enabling the worker for company use.

## Threat model summary

Per SPEC §1, the **agent** writes code, pushes branches, and opens pull
requests; the **worker** is the scheduler/runner and tracker *reader* — it
polls Gitea, prepares the deterministic per-issue workspace, and runs the agent
in it (#76 removed all worker-side push/PR/merge code). Two trust boundaries
matter:

- A Gitea API token (`GITEA_TOKEN`) is held by the **worker process** for
  tracker polling and the orchestrator-owned `gitea_issue_labels` state-label
  tool, never as an environment variable inside the agent subprocess (the
  `linear_graphql`-style token isolation from #76). The agent's branch push
  **and** PR creation use a separate credential embedded in `repo.clone_url`
  (see below).
- Reviewers must remain the only path that lands code on `main`.

The settings below keep both boundaries enforced even if the worker, the agent,
its host, or its tokens are compromised.

## Bot account

Create a dedicated Gitea user for the worker. Do not reuse a human account.

Recommended account properties:

- Username: e.g. `aiops-bot`.
- Email: a dedicated mailbox the team controls.
- Two-factor authentication: enabled.
- No organization owner role. The bot should be a regular member.
- Repository access: collaborator on each target repository, with **Write** access only. Do not grant **Admin**.

Reasoning: the bot identity backs branch pushes and PR creation (both performed
by the agent, not the worker), plus tracker polling and label writes. Write is
sufficient. Admin would let a compromised token change branch protection,
webhooks, or repository settings.

## Bot credential scope

Two distinct Gitea credentials back the bot, and they are isolated from each
other on purpose:

1. **`GITEA_TOKEN` — the orchestrator-held tracker/API token.** The worker
   process exports it and uses it for Gitea issue polling (reading issues and
   labels) and for the one orchestrator-owned Gitea write tool currently
   advertised to the agent: `gitea_issue_labels` (the aiops/* state-label
   tool wired in `internal/runner/tools.go`). When the agent calls that tool
   the **worker process** attaches the token server-side and makes the
   authenticated call; the raw token is **never** copied into the agent
   subprocess environment, tool metadata, or prompt text —
   `internal/workflow/agent_env_policy.go` explicitly denies `GITEA_TOKEN`
   (and `GITEA_API_TOKEN`, `GITHUB_TOKEN`, …) passthrough. This is the same
   token-isolation boundary `linear_graphql` enforces for Linear (#76).
   `GITEA_TOKEN` is **not** the credential the agent uses to open a PR — there
   is no orchestrator-owned Gitea PR-creation proxy (the transitional
   `internal/gitea.CreatePullRequest`/`FindOpenPullRequest` helpers never
   gained a production call path after #76 and were deleted under #771; only
   `gitea_issue_labels` is advertised).
2. **`repo.clone_url` basic-auth — the agent's push + PR credential.** The
   workspace sets `origin` to `repo.clone_url`, whose embedded basic-auth
   userinfo authorizes the agent's `git push origin <work-branch>` inside its
   workspace without `GITEA_TOKEN` ever being present as an env var. The same
   credential is what the agent uses to open the pull request the `WORKFLOW.md`
   prompt instructs it to create (e.g. via the Gitea `POST /api/v1/repos/.../pulls`
   API), since the agent has no other Gitea credential. Configure it
   independently of `GITEA_TOKEN`; because it must both push branches and open
   PRs it needs repository **write** (a read-only deploy token is not enough).
   `workflow.MaskCloneURL` scrubs this credential from the diagnostics that
   route through it, and the workspace-mirror clone/fetch paths now mask it too
   (#595): `internal/workspace/mirror.go` wraps a failed bare clone with
   `MaskCloneURL`, and clone/fetch git output is forwarded through
   `runGitRedacted`, which strips embedded `user:token@` userinfo from git's
   stderr. As with any credential, still prefer a clone-URL credential you can
   rotate easily.

Recommended `GITEA_TOKEN` scopes (Gitea 1.20+ scoped token model):

- `write:issue` on the specific repositories the bot is allowed to act on —
  **required** whenever the `gitea_issue_labels` state tool is enabled (the
  normal Gitea path). The tool moves an issue's aiops/* state via
  `POST`/`DELETE /repos/{owner}/{repo}/issues/{index}/labels`, which Gitea
  classifies under the `issue` scope, not `repository`. With only
  `read`-level issue scope the agent's first state transition fails
  authorization. `write:issue` implies `read:issue`.
- `read:issue` covers issue/label polling and is implied by `write:issue`.
- `GITEA_TOKEN` does **not** need `write:repository`: it neither pushes
  branches nor opens PRs (those use the separate `repo.clone_url` credential
  below). Granting it only widens blast radius for no functional gain.

The `repo.clone_url` credential is separate and needs repository **write**
(`write:repository`) on the target repositories: it authorizes both the agent's
branch push and the PR creation (`POST /repos/{owner}/{repo}/pulls`, which Gitea
classifies under the `repository` scope).

Recommended token scopes to **not** grant:

- `admin:*` of any kind.
- `write:organization`, `write:user`, `write:admin`.
- `delete:repository`.
- `write:package`, unless package publishing is an explicit feature.

If the deployed Gitea version only exposes the legacy `repo` scope, prefer that over a global token, and rely on per-repository collaborator membership to limit blast radius.

## Token storage and rotation

- Inject `GITEA_TOKEN` only as an environment variable on the worker process. Do not commit it. Do not add it to `codex.env_passthrough` / `claude.env_passthrough` — it is denied passthrough into the agent subprocess by design.
- Keep the `repo.clone_url` push + PR credential in the same secret manager. `workflow.MaskCloneURL` scrubs it from worker output, including the workspace-mirror clone/fetch error and stderr paths (#595).
- Store the source of truth in a secret manager.
- Rotate both credentials on a schedule (recommended: every 90 days) and immediately if a worker host or backup is suspected compromised.
- Revoke a credential in Gitea before deleting it from the secret store, so any in-flight call fails closed.
- **Rotating the `repo.clone_url` credential also requires invalidating the cached bare mirror.** The mirror is keyed by host/path, not by the URL userinfo, and the reuse path only runs `git fetch` — it does not rewrite `remote.origin.url` (`internal/workspace/mirror.go` → `ensureMirrorLocked`). So a mirror cloned with the old credential keeps using it until reset: after rotating, delete the repo's mirror under `$AIOPS_MIRROR_ROOT/` (or rewrite its `remote.origin.url`) so the next prepare re-clones with the new credential. See [`workspace-cache.md`](workspace-cache.md) for the cache layout and reset. Skipping this makes the worker keep authenticating with the just-revoked credential and then fail closed on the next fetch/push.

## Branch protection on `main`

Enable Gitea branch protection for `main` (Settings -> Branches -> Protected Branches) on every repository the bot can push to.

Required settings:

- **Disable direct push**: no user, including the bot, may push commits directly to `main`.
- **Enable "Require pull request reviews before merging"**: at least one approving review from a human reviewer.
- **Require status checks to pass**: select the GitHub Actions / Gitea Actions checks defined in `docs/runbooks/ci.md` (build, tests, gofmt, go mod tidy).
- **Dismiss stale approvals on new commits**: on. Forces re-review when the agent pushes new commits to the PR branch.
- **Restrict who can push to matching branches**: leave empty, or restrict to release tooling only. The bot must not be in this list.
- **Require signed commits**: optional but recommended once signing is configured for human contributors.
- **Block force push**: on.
- **Block deletions**: on.

If your Gitea version supports it, also enable "Require linear history" so merges are squash or rebase only.

## Worker / agent behavior contract

Code never lands on `main` by the worker or the agent on its own. The contract
is:

- The **agent** pushes its work to a feature branch named after the task (for
  example `ai/<task-id>-<slug>`) and opens the pull request from that branch
  into `main`, using its workflow/tool surface. The **worker** only prepares
  the workspace and runs the agent — it has **no** `git push` or
  `POST /api/v1/repos/.../pulls` code path (#76 removed them; `cmd/worker` and
  `internal/orchestrator` carry only a `-github-issue` push *preflight* flag,
  not a push).
- Whether a PR is opened as a draft is set via the `WORKFLOW.md` prompt (the `pr.draft` front-matter key was removed in #578 and is now rejected at load). Gitea's `POST /repos/{owner}/{repo}/pulls` API has no `draft` request field (verified against `release/v1.26` `modules/structs/pull.go`); draft state is derived purely from a Work-In-Progress title prefix matched against `setting.Repository.PullRequest.WorkInProgressPrefixes` (default `WIP:` and `[WIP]`), which the agent sets on the PR title. Reviewers will see PR titles like `WIP: chore(ai): ...` for drafts. Draft state is a workflow-level signal only — do not rely on it as a human gate; reviewers must still treat every agent-authored PR as unverified until a human review is complete.
- Neither the worker nor any orchestrator-held tool calls a merge endpoint. There is no merge code path in `cmd/worker/main.go` or `internal/gitea/`. If you add one in the future, gate it behind explicit configuration and do not enable it by default.
- The **worker** has no code path that deletes branches or changes repository settings, webhooks, or branch protection — that is a hard guarantee of the worker binary (#76). The **agent** is instructed only to push its work branch and open a PR; it is not asked to delete branches or touch repository settings. But the agent holds a repository-**write** credential (`repo.clone_url`), so "the agent does not delete branches" is a behavioral expectation, **not** an enforced boundary: a misbehaving or compromised agent can delete or force-update any branch that branch protection does not cover. Per the threat model, the enforced guarantee comes from Gitea protection, so block deletions / force-push on **every** branch that must survive (release branches, shared feature branches), not just `main` — see "Branch protection on `main`" above and apply the same `Block deletions` / `Block force push` settings to those branch patterns.

If a future change introduces auto-merge, it must be opt-in per repository, must require all status checks green, and must still require at least one human review according to branch protection.

## Pull request review expectations

Every agent-authored pull request must be reviewed by a human before merge. Reviewers are expected to:

1. **Read the run summary**: confirm the linked task ID, the source event (Gitea, GitHub, or Linear issue), the model used, and the workflow that produced the change.
2. **Verify diff scope** against the human reviewer's understanding of the issue. Scope and path constraints now live in the operator's `WORKFLOW.md` prompt (SPEC §3.2) as advisory pre-push instructions — neither sandbox layer nor the worker enforces repository-subpath policy (the `deny_paths` / `max_changed_*` gate was removed in #561). So review scope as a human and rely on repository permissions, branch protection, required checks, and review for enforced landing controls:
   - Confirm only files relevant to the task were changed, and flag any out-of-scope edits the prompt was supposed to keep the agent away from.
   - Treat an unexpectedly large or wide-ranging diff as a signal to request a split, not a config violation.
3. **Read every changed line**. AI-authored diffs may look reasonable while changing semantics. Do not skim.
4. **Run the verify commands locally** if the change touches anything you are not certain about. The verify commands are listed in `WORKFLOW.md` and `docs/runbooks/ci.md`.
5. **Check secrets and credentials**: confirm no tokens, keys, or environment values were added to the repository.
6. **Resolve all review threads** before approval.
7. **Approve and merge as a human**. The bot account must not approve and must not merge. The cautious profile opens PRs as drafts (draft intent is set in the `WORKFLOW.md` prompt), but draft state only signals intent — reviewers are the first and only human gate before merge, regardless of whether the PR was opened as draft or ready-for-review. Mark the PR ready for review only after the verification steps above pass.

If the reviewer is uncertain, the expected action is to request changes or close the PR. Do not merge to "see what happens".

## Poller security

Gitea issue discovery is poll-based. Do not configure repository webhooks for
aiops-platform; the retired trigger API and HMAC webhook secret are not part of
the SPEC-aligned runtime. Operational requirements:

- Scope `GITEA_TOKEN` to the repositories the worker and agent tools actually need.
- Keep tracker state in `aiops/*` labels so the worker can select active issues without receiving webhooks.
- Treat issue titles, bodies, labels, and comments as user-authored content; avoid logging more than the minimal diagnostics needed to identify a task.

## Incident response checklist

If the bot account or its token may be compromised:

1. Revoke both bot credentials in Gitea immediately: `GITEA_TOKEN` and the `repo.clone_url` push + PR credential.
2. Stop the worker process.
3. Delete the affected repos' cached bare mirrors under `$AIOPS_MIRROR_ROOT/` (see [`workspace-cache.md`](workspace-cache.md)) — a mirror cloned with the now-revoked `repo.clone_url` credential retains it in `remote.origin.url` until reset, so it must not survive into the post-incident worker.
4. Audit recent pushes by the bot account (`git log --author=aiops-bot` on each repo, plus Gitea audit log).
5. Force-close any open PRs the agent opened under the bot identity during the suspect window.
6. Confirm `main` is unaffected (branch protection should guarantee this, but verify).
7. Issue new credentials with the recommended scopes above and resume the worker.

## Pre-production checklist

Before enabling the worker against company repositories, confirm:

- [ ] Dedicated `aiops-bot` Gitea account exists with 2FA.
- [ ] Bot has Write (not Admin) access on each target repository.
- [ ] `GITEA_TOKEN` is scoped to `write:issue` (not `write:repository`) on the allowed repositories only, and is denied passthrough into the agent subprocess (orchestrator-tools-only).
- [ ] The `repo.clone_url` push + PR credential has repository write on the allowed repositories and is configured independently of `GITEA_TOKEN`.
- [ ] Branch protection on `main` blocks direct push, requires PR review, requires status checks, blocks force push, blocks deletion.
- [ ] Any other branch that must not be deleted or force-updated (release branches, shared feature branches) carries `Block deletions` / `Block force push` protection — the agent's `repo.clone_url` write credential can otherwise delete or rewrite unprotected branches.
- [ ] The bot is not in any push-allowlist on protected branches.
- [ ] Worker host stores the token only in environment variables sourced from a secret manager.
- [ ] Gitea issues use the configured `aiops/*` state labels for poller discovery.
- [ ] Reviewer expectations above are documented for the team that owns the repositories.

# Gitea bot and branch protection runbook

This runbook documents the minimum safe Git settings for running aiops-platform against a Gitea instance. Follow this before enabling the worker for company use.

## Threat model summary

Per SPEC §1, the **agent** writes code, pushes branches, and opens pull
requests; the **worker** is the scheduler/runner and tracker *reader* — it
polls Gitea, prepares the deterministic per-issue workspace, and runs the agent
in it (#76 removed all worker-side push/PR/merge code). Two trust boundaries
matter:

- A Gitea API token (`GITEA_TOKEN`) is held by the **worker process** for
  tracker polling and is exposed to the agent only through orchestrator-owned
  tool proxies, never as an environment variable inside the agent subprocess
  (the `linear_graphql`-style token isolation from #76). The agent's branch
  push uses a separate credential embedded in `repo.clone_url` (see below).
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
   labels). It is also the token behind the agent's orchestrator-owned tool
   proxies: when the agent calls a Gitea tool (the `gitea_issue_labels` state
   tool wired in `internal/runner/tools.go`, and the Gitea PR helper in
   `internal/gitea/client.go`), the **worker process** attaches the token
   server-side and makes the authenticated call. The raw token is **never**
   copied into the agent subprocess environment, tool metadata, or prompt text
   — `internal/workflow/agent_env_policy.go` explicitly denies `GITEA_TOKEN`
   (and `GITEA_API_TOKEN`, `GITHUB_TOKEN`, …) passthrough. This is the same
   token-isolation boundary `linear_graphql` enforces for Linear (#76).
2. **`repo.clone_url` basic-auth — the branch-push credential.** The workspace
   sets `origin` to `repo.clone_url`, whose embedded basic-auth userinfo
   carries the push credential, so the agent's `git push origin <work-branch>`
   succeeds inside its workspace without `GITEA_TOKEN` ever being present as an
   env var. Configure this credential independently of `GITEA_TOKEN` (it can be
   a narrowly scoped deploy token). It is masked from logs and error strings by
   `workflow.MaskCloneURL`.

Recommended `GITEA_TOKEN` scopes (Gitea 1.20+ scoped token model):

- `write:repository` on the specific repositories the bot is allowed to act on
  (needed for the agent's PR-creation tool and label writes routed through the
  worker process).
- `read:repository` is implied (covers issue/label polling).
- `write:issue` only if the workflow exposes an issue-comment tool. The state
  tool uses label writes covered by `write:repository`; leave `write:issue` off
  until a workflow needs it.

Recommended token scopes to **not** grant:

- `admin:*` of any kind.
- `write:organization`, `write:user`, `write:admin`.
- `delete:repository`.
- `write:package`, unless package publishing is an explicit feature.

If the deployed Gitea version only exposes the legacy `repo` scope, prefer that over a global token, and rely on per-repository collaborator membership to limit blast radius.

## Token storage and rotation

- Inject `GITEA_TOKEN` only as an environment variable on the worker process. Do not commit it. Do not add it to `codex.env_passthrough` / `claude.env_passthrough` — it is denied passthrough into the agent subprocess by design.
- Keep the `repo.clone_url` push credential in the same secret manager and out of logs (`workflow.MaskCloneURL` scrubs it from worker output).
- Store the source of truth in a secret manager.
- Rotate both credentials on a schedule (recommended: every 90 days) and immediately if a worker host or backup is suspected compromised.
- Revoke a credential in Gitea before deleting it from the secret store, so any in-flight call fails closed.

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
- Whether a PR is opened as a draft is set via the `WORKFLOW.md` prompt (the `pr.draft` front-matter key was removed in #578 and is now rejected at load). Gitea's `POST /repos/{owner}/{repo}/pulls` API has no `draft` request field (verified against `release/v1.26` `modules/structs/pull.go`); draft state is derived purely from a Work-In-Progress title prefix matched against `setting.Repository.PullRequest.WorkInProgressPrefixes` (default `WIP:` and `[WIP]`), which the PR tool sets. Reviewers will see PR titles like `WIP: chore(ai): ...` for drafts. Draft state is a workflow-level signal only — do not rely on it as a human gate; reviewers must still treat every agent-authored PR as unverified until a human review is complete.
- Neither the worker nor any orchestrator-held tool calls a merge endpoint. There is no merge code path in `cmd/worker/main.go` or `internal/gitea/client.go`. If you add one in the future, gate it behind explicit configuration and do not enable it by default.
- The agent does not delete branches.
- Neither the worker nor the agent changes repository settings, webhooks, or branch protection.

If a future change introduces auto-merge, it must be opt-in per repository, must require all status checks green, and must still require at least one human review according to branch protection.

## Pull request review expectations

Every agent-authored pull request must be reviewed by a human before merge. Reviewers are expected to:

1. **Read the run summary**: confirm the linked task ID, the source event (Gitea, GitHub, or Linear issue), the model used, and the workflow that produced the change.
2. **Verify diff scope** against the human reviewer's understanding of the issue. Scope and path constraints now live in the operator's `WORKFLOW.md` prompt (SPEC §3.2), enforced preventively by the agent before push — there is no worker-side path/diffstat gate (the `deny_paths` / `max_changed_*` policy caps were removed in #561). So review scope as a human:
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

1. Revoke both bot credentials in Gitea immediately: `GITEA_TOKEN` and the `repo.clone_url` push credential.
2. Stop the worker process.
3. Audit recent pushes by the bot account (`git log --author=aiops-bot` on each repo, plus Gitea audit log).
4. Force-close any open PRs the agent opened under the bot identity during the suspect window.
5. Confirm `main` is unaffected (branch protection should guarantee this, but verify).
6. Issue new credentials with the recommended scopes above and resume the worker.

## Pre-production checklist

Before enabling the worker against company repositories, confirm:

- [ ] Dedicated `aiops-bot` Gitea account exists with 2FA.
- [ ] Bot has Write (not Admin) access on each target repository.
- [ ] `GITEA_TOKEN` is scoped to `write:repository` on the allowed repositories only, and is denied passthrough into the agent subprocess (orchestrator-tools-only).
- [ ] The `repo.clone_url` push credential is scoped to the allowed repositories and configured independently of `GITEA_TOKEN`.
- [ ] Branch protection on `main` blocks direct push, requires PR review, requires status checks, blocks force push, blocks deletion.
- [ ] The bot is not in any push-allowlist on protected branches.
- [ ] Worker host stores the token only in environment variables sourced from a secret manager.
- [ ] Gitea issues use the configured `aiops/*` state labels for poller discovery.
- [ ] Reviewer expectations above are documented for the team that owns the repositories.

# Gitea bot and branch protection runbook

This runbook documents the minimum safe Git settings for running aiops-platform against a Gitea instance. Follow this before enabling the worker for company use.

## Threat model summary

The worker writes code on behalf of humans and pushes it back to Gitea. Two trust boundaries matter:

- The worker holds a Gitea API token and can push branches and open pull requests.
- Reviewers must remain the only path that lands code on `main`.

The settings below keep both boundaries enforced even if the worker, its host, or its token are compromised.

## Bot account

Create a dedicated Gitea user for the worker. Do not reuse a human account.

Recommended account properties:

- Username: e.g. `aiops-bot`.
- Email: a dedicated mailbox the team controls.
- Two-factor authentication: enabled.
- No organization owner role. The bot should be a regular member.
- Repository access: collaborator on each target repository, with **Write** access only. Do not grant **Admin**.

Reasoning: the worker only needs to push branches and open pull requests. Write is sufficient. Admin would let a compromised token change branch protection, webhooks, or repository settings.

## Bot token scope

The worker uses a single Gitea personal access token, exported as `GITEA_TOKEN`. From the code paths exercised today (`internal/gitea/client.go` and `internal/workspace/manager.go`) the bot performs:

- `git push origin <work-branch>` over HTTPS using the token.
- `POST /api/v1/repos/{owner}/{repo}/pulls` to open a pull request.
- Webhook delivery is verified with an HMAC secret and does not require a token.

Recommended token scopes (Gitea 1.20+ scoped token model):

- `write:repository` on the specific repositories the worker is allowed to act on.
- `read:repository` is implied.
- `write:issue` only if the worker is expected to comment on issues. The current worker does not require it; leave it off until needed.

Recommended token scopes to **not** grant:

- `admin:*` of any kind.
- `write:organization`, `write:user`, `write:admin`.
- `delete:repository`.
- `write:package`, unless package publishing is an explicit feature.

If the deployed Gitea version only exposes the legacy `repo` scope, prefer that over a global token, and rely on per-repository collaborator membership to limit blast radius.

## Token storage and rotation

- Inject `GITEA_TOKEN` only as an environment variable on the worker process. Do not commit it.
- Store the source of truth in a secret manager.
- Rotate the token on a schedule (recommended: every 90 days) and immediately if a worker host or backup is suspected compromised.
- Revoke the token in Gitea before deleting it from the secret store, so any in-flight worker call fails closed.

## Branch protection on `main`

Enable Gitea branch protection for `main` (Settings -> Branches -> Protected Branches) on every repository the worker can push to.

Required settings:

- **Disable direct push**: no user, including the bot, may push commits directly to `main`.
- **Enable "Require pull request reviews before merging"**: at least one approving review from a human reviewer.
- **Require status checks to pass**: select the GitHub Actions / Gitea Actions checks defined in `docs/runbooks/ci.md` (build, tests, gofmt, go mod tidy).
- **Dismiss stale approvals on new commits**: on. Forces re-review when the worker pushes new commits to the PR branch.
- **Restrict who can push to matching branches**: leave empty, or restrict to release tooling only. The bot must not be in this list.
- **Require signed commits**: optional but recommended once signing is configured for human contributors.
- **Block force push**: on.
- **Block deletions**: on.

If your Gitea version supports it, also enable "Require linear history" so merges are squash or rebase only.

## Worker behavior contract

The worker is designed to never land code on `main` by itself. The contract is:

- The worker pushes to a feature branch named after the task (for example `ai/<task-id>-<slug>`).
- The worker opens a pull request from that branch into `main`.
- Whether the PR is opened as a draft is controlled by the `pr.draft` field in `WORKFLOW.md`. The cautious-company template (`docs/workflows/company-cautious-WORKFLOW.md`) sets `pr.draft: true`, so the worker opens drafts under the cautious profile. Workflows that omit the field or set it to `false` get regular ready-for-review PRs. Gitea's `POST /repos/{owner}/{repo}/pulls` API has no `draft` request field (verified against `release/v1.26` `modules/structs/pull.go`); draft state is derived purely from a Work-In-Progress title prefix matched against `setting.Repository.PullRequest.WorkInProgressPrefixes` (default `WIP:` and `[WIP]`). When `pr.draft: true` is set, `internal/gitea/client.go` prepends `WIP: ` to the PR title before sending the create request. Reviewers will see PR titles like `WIP: chore(ai): ...` for drafts. Draft state is a workflow-level signal only — do not rely on it as a human gate; reviewers must still treat every worker-authored PR as unverified until a human review is complete.
- The worker does **not** call any merge endpoint. There is no merge code path in `cmd/worker/main.go` or `internal/gitea/client.go`. If you add one in the future, gate it behind explicit configuration and do not enable it by default.
- The worker does not delete branches.
- The worker does not change repository settings, webhooks, or branch protection.

If a future change introduces auto-merge, it must be opt-in per repository, must require all status checks green, and must still require at least one human review according to branch protection.

## Pull request review expectations

Every worker-authored pull request must be reviewed by a human before merge. Reviewers are expected to:

1. **Read the run summary**: confirm the linked task ID, the source event (Gitea issue, Linear issue, or manual enqueue), the model used, and the workflow that produced the change.
2. **Verify diff scope**:
   - Confirm only files relevant to the task were changed.
   - Confirm no denied paths from `WORKFLOW.md` policy were modified.
   - Confirm the changed file count is within the policy limit.
3. **Read every changed line**. AI-authored diffs may look reasonable while changing semantics. Do not skim.
4. **Run the verify commands locally** if the change touches anything you are not certain about. The verify commands are listed in `WORKFLOW.md` and `docs/runbooks/ci.md`.
5. **Check secrets and credentials**: confirm no tokens, keys, or environment values were added to the repository.
6. **Resolve all review threads** before approval.
7. **Approve and merge as a human**. The bot account must not approve and must not merge. The cautious profile opens worker PRs as drafts via `pr.draft: true`, but draft state only signals intent — reviewers are the first and only human gate before merge, regardless of whether the PR was opened as draft or ready-for-review. Mark the PR ready for review only after the verification steps above pass.

If the reviewer is uncertain, the expected action is to request changes or close the PR. Do not merge to "see what happens".

## Webhook security

The worker validates incoming Gitea webhooks with HMAC SHA-256 (`internal/gitea/webhook.go`). Operational requirements:

- Configure a strong, random `GITEA_WEBHOOK_SECRET` per repository.
- Restrict webhook events to `issue_comment` (and any others the worker explicitly handles).
- Restrict the source IP ranges of the webhook receiver if the network allows it.
- Do not log the raw webhook body; the payload may contain user-authored content.

## Incident response checklist

If the bot account or its token may be compromised:

1. Revoke `GITEA_TOKEN` in Gitea immediately.
2. Stop the worker process.
3. Audit recent pushes by the bot account (`git log --author=aiops-bot` on each repo, plus Gitea audit log).
4. Force-close any open PRs the bot opened during the suspect window.
5. Confirm `main` is unaffected (branch protection should guarantee this, but verify).
6. Rotate the webhook secret.
7. Issue a new token with the recommended scopes above and resume the worker.

## Pre-production checklist

Before enabling the worker against company repositories, confirm:

- [ ] Dedicated `aiops-bot` Gitea account exists with 2FA.
- [ ] Bot has Write (not Admin) access on each target repository.
- [ ] `GITEA_TOKEN` is scoped to `write:repository` on the allowed repositories only.
- [ ] Branch protection on `main` blocks direct push, requires PR review, requires status checks, blocks force push, blocks deletion.
- [ ] The bot is not in any push-allowlist on protected branches.
- [ ] Worker host stores the token only in environment variables sourced from a secret manager.
- [ ] Webhook secret is configured and verified by the worker.
- [ ] Reviewer expectations above are documented for the team that owns the repositories.

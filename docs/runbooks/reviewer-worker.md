# Reviewer worker — a fresh-context checker on `Human Review`

How to run a second worker process whose agent reviews handed-off work
against a rubric and issues the terminal verdict, so the model that wrote
the code is not the one that declares it done.

This is the deployment pattern behind the maker/checker split that already
exists in the tracker state machine: under this pattern's prompt contract
the maker agent moves an issue no further than `Human Review` (a
*pending-verification* state), and `Done` is issued by the checker — a
human today, a fresh-context reviewer agent with this runbook. (The split
is a prompt contract, not a platform enforcement: `gitea_issue_labels`
accepts any `aiops/*` state label, so a maker prompt that allowed
`aiops/done` would bypass the checker.) It is pure configuration + prompt: **zero platform code**, no
new config keys, consistent with D25's "one process per service" stance
(service routing was removed in #573) and the single-workflow-source rule
(#72). The rejected alternative — a worker-side verifier gate at the
`Finishing` phase — is documented in #744's closing verdict (worker does not
own PR handoff per #76; post-turn gates race reconcile-cancel per #557/D33;
upstream has no verifier equivalent).

Advisory background (per the AGENTS.md practitioner-accounts clause): both
loop-engineering articles this pattern answers gate the *stop condition*
with an independent-context grader — "a fresh model decides if the loop is
done instead of the one that did the work" (Osmani), "grading is done in an
independent context window" (Martin). The reviewer worker is that shape
expressed inside the SPEC state machine.

## Topology

Two worker processes, one tracker project:

| | maker worker | reviewer worker |
|---|---|---|
| `WORKFLOW.md` | implementation prompt | rubric/review prompt ([example](../../examples/reviewer-WORKFLOW.md)) |
| `tracker.active_states` | `[Todo, Rework]` | `[Human Review]` |
| `tracker.inactive_states` | `[Human Review, Merging]` | `[Todo, In Progress, Merging, Rework]` |
| `workspace.root` | e.g. `~/aiops-workspaces/maker` | **a different root — hard requirement, see below** |
| `AIOPS_MIRROR_ROOT` env | e.g. `~/aiops-mirrors/maker` | **a different mirror root — hard requirement, see below** |
| state writes (agent-side) | flips to `aiops/human-review` | flips to `aiops/done` / `aiops/rework` |

Both point at the same Gitea repo/project. Optionally narrow the reviewer
further with `tracker.required_labels`.

State flow:

```
Todo ──maker──▶ Human Review ──reviewer──▶ Done        (rubric passed)
  ▲                                  └────▶ Rework      (rubric failed)
  └──────────── maker re-dispatch on Rework ◀──────────┘
```

The `Rework` return path is the platform's existing re-dispatch cycle — no
new mechanics. (Exercised live in the v0.1.0 lifecycle test: 12/12 issues
merged, with three `Rework` round-trips.)

## Hard requirement: separate `workspace.root` per worker

The two workers MUST NOT share a workspace root. The collision is
deterministic, not theoretical:

- `PathFor` is `Root/<owner>/<repo>/<sourceType>/<sourceEventID>` (falling
  back to `Root/<owner>/<repo>/<task.ID>` when source type/event id are
  unset; `internal/workspace/manager.go`) — either way the *same issue*
  resolves to the *same directory* in both processes when the roots match.
- Workspace reuse is destructive: `reuseWorktree` runs
  `git reset --quiet HEAD -- .` followed by
  `git checkout --force --no-track -B <branch> <startRef>`, force-discarding
  tracked modifications.
- The processes overlap in time. After the maker flips the handoff label,
  the maker's run keeps streaming until its reconcile pass observes the
  inactive state — up to one full poll interval (the tail window measured in
  #557). The reviewer can dispatch inside that window and force-reset a
  worktree the maker's agent is still using. There is no cross-process lock
  to save you.

Give each worker its own `workspace.root` and the collision is impossible
(different path prefixes).

### Bare mirror cache: also a hard requirement — set `AIOPS_MIRROR_ROOT` per worker

`workspace.root` isolation does NOT isolate the bare mirror cache. Each
worker's mirror root resolves (in order) from the `AIOPS_MIRROR_ROOT` env
var, else `<user-cache-dir>/aiops-platform/mirrors`, else
`<tmp>/aiops-platform/mirrors` (`internal/workspace/mirror.go`). Two workers
on the same host and user therefore share one bare mirror per clone URL —
and in THIS pattern that is not merely a contention risk, it is broken by
design:

- Both workers dispatch the *same issue* on the *same work branch* name,
  `ai/<issue.ID>` (`internal/orchestrator/poller.go`).
- Worktrees attach to the shared bare mirror
  (`git worktree add --no-track -B <branch>`), and git refuses to check out
  a branch that another worktree of the same repository already has checked
  out. While the maker's worktree holds `ai/<issue.ID>`, the reviewer's
  workspace preparation for that issue fails outright.

Set a distinct `AIOPS_MIRROR_ROOT` for each worker process. It costs one
extra bare clone on disk and removes both the branch-namespace collision
and the flock/fetch contention class (mirror preparation is serialized
across processes by the per-mirror OS-level advisory flock in
`acquireMirrorLock`; an acquisition error fails closed and retries on the
normal backoff).

## Reviewer `WORKFLOW.md`

Start from [`examples/reviewer-WORKFLOW.md`](../../examples/reviewer-WORKFLOW.md).
Verify any edited front matter against the real loader before deploying —
the snippet must pass, with the same environment the reviewer process will
launch under (the mirror root comes from the environment, not the front
matter — omitting it falls back to the shared per-user cache and hits the
branch collision documented above):

```bash
export AIOPS_MIRROR_ROOT=~/aiops-mirrors/reviewer   # hard requirement, see above
export GITEA_TOKEN=...           # worker-held tracker token
export REVIEWER_CLONE_URL=...    # bot basic-auth clone URL
cp examples/reviewer-WORKFLOW.md /path/to/reviewer-workdir/WORKFLOW.md
go run ./cmd/worker --print-config /path/to/reviewer-workdir
```

Launch the reviewer worker with those same exports in place.

Two properties of the example are load-bearing:

1. **"Review only, do not write code" lives in the prompt body.** Do not
   reach for `policy.mode: analysis_only` to enforce it: that directive is
   the plan-artifact contract (`internal/worker/runtask.go`,
   `AppendAnalysisOnlyDirective`) — it asks the agent to produce
   `.aiops/PLAN.md` and to refrain from posting tracker comments, which is
   the wrong job description for a reviewer whose deliverables are exactly
   a findings comment plus a verdict label flip. (It is a prompt directive,
   not a worker-enforced gate — the post-turn analysis gate class was
   removed in #561/D33 — but pointing it at a reviewer buys you nothing
   and contradicts the prompt you actually want.)
2. **The verdict path does not depend on the sandbox.** The label flip goes
   through `gitea_issue_labels`, an orchestrator-proxied dynamic tool: the
   request is executed by the worker process holding the token, not by the
   agent's sandboxed filesystem/network. Even the strictest sandbox tier
   still delivers verdicts.

### Sandbox: two tiers (both need explicit network access)

The typed sandbox policies derive `networkAccess: false` by default
(`internal/workflow/codex_schema.go`), and everything in "finding the diff"
below — `git fetch`, Gitea API calls — needs the network. Set
`codex.turn_sandbox_policy` explicitly in either tier (the v0.1.0 lifecycle
test hit exactly this: the maker could not push or open PRs until
`networkAccess: true` was set):

- **Default — review can run build/test:**
  `thread_sandbox: workspace-write` plus an explicit `workspaceWrite` turn
  policy (the same shape the maker runs; note the loader requires the full
  field set for this type — `writableRoots`, `networkAccess`,
  `excludeTmpdirEnvVar`, `excludeSlashTmp` — see the example for the exact
  YAML). The reviewer can
  `git fetch` the head branch, diff locally, and run `go build` /
  `go test`. Keep the "do not commit, do not push" constraint in the
  prompt.
- **Strict — judge the diff text only:**
  `thread_sandbox: read-only` +
  `turn_sandbox_policy: {type: readOnly, networkAccess: true}`. The
  read-only filesystem blocks `git fetch` too (fetch writes `.git`), so the
  diff must come from the Gitea API as text
  (`GET /repos/{owner}/{repo}/pulls/{number}.diff`) and build/test rubric
  items are off the table. Strongest containment.

## Finding the diff under review

Nothing hands the reviewer a PR link: the rendered prompt contains only the
SPEC §4.1.1 issue snapshot (`internal/worker/runtask.go`,
`issueRenderVarsForTask`), and the reviewer's worktree is freshly created
from the base ref — **the maker's changes are not in the reviewer's local
checkout**.

**Credentials reality check.** The tracker API token never reaches the
agent: `GITEA_TOKEN` and friends are on the env-passthrough deny list
(`internal/workflow/agent_env_policy.go`), and the only Gitea dynamic tool
is the label flip. What the agent CAN use is the same surface the maker
uses to push and open PRs: the workspace's git remote. Give `repo.clone_url`
an HTTP(S) basic-auth token (`http://<bot>:<token>@gitea.local/...`) — the
agent reads it from `git remote get-url origin` and uses it both for
`git fetch` and for Gitea API calls (read issue comments, fetch the PR
diff, post the review-findings comment). Use a low-privilege bot account
for that token per the repo safety posture; with an SSH `clone_url` the
agent can fetch but has no API credential, which limits the reviewer to
fetch-and-diff plus the label flip (no comments).

Adopt one of these two discovery conventions and write it into BOTH prompts
(the example uses the first):

1. **PR URL in an issue comment (default).** The maker's prompt requires it
   to comment the PR URL on the issue immediately after opening the PR. The
   reviewer reads the issue comments, takes the newest PR URL, and obtains
   the diff via `git fetch origin <head-branch>` or the API.
2. **Head-branch lookup.** The platform dispatches every run on work branch
   `ai/<issue.ID>` (`internal/orchestrator/poller.go`). Note that for the
   Gitea tracker `issue.ID` is Gitea's *internal* issue id, not the
   human-visible issue number — discover the actual branch/PR by listing
   open PRs whose head matches `ai/*` and whose body/title references the
   issue, rather than reconstructing the name from the issue number.

## Single-process alternative: in-run grader sub-agent (verified)

If you cannot run a second worker, the maker's own `WORKFLOW.md` can demand
an independent-context grade *before* the handoff label flip — the
preventive placement both articles actually describe.

Verified live (codex CLI 0.137.0, `codex app-server`, experimental API on —
the platform's standard session shape):

- The session exposes a multi-agent tool family —
  `multi_agent_v1.spawn_agent` / `send_input` / `wait_agent` /
  `resume_agent` / `close_agent` — discoverable through the session's
  `tool_search` tool (it is not in the initially visible tool list). An
  end-to-end probe (spawn a grader with inline instructions, wait, read its
  verdict) succeeded; the wire stream reports it as `collabAgentToolCall`
  items.
- **Named `.codex/agents/*.toml` definitions are NOT exposed** in this mode
  (probed: a `.codex/agents/grader.toml` in the workspace was not available
  as a named agent). Define the grader inline in the `spawn_agent`
  instructions instead of relying on repo-level agent files.

Prompt shape that works:

```
Before flipping the handoff label: use tool_search to find your
multi-agent tools, spawn one sub-agent whose instructions are the rubric
below plus the diff you produced, wait for its verdict, and only proceed
to the handoff if it answers PASS. On FAIL, fix the findings and re-grade.
```

Caveats: the grader burns extra tokens per run (spend it where a second
opinion pays — Osmani's caveat); the tool family is experimental surface
and should be re-verified after a codex CLI upgrade
(`CodexProtocolVersion` bumps, see `internal/runner/codex_version.go`).

## Alignment statement

- Zero platform changes: two processes, two `WORKFLOW.md` files, one
  tracker. Matches D25 ("Replaceable by running one process per service")
  and #72 (one workflow source *per process*).
- The reviewer is an ordinary SPEC worker; its "verdict" is an agent-side
  tracker write through the existing tool surface (SPEC §1, #76).
- No `DEVIATIONS.md` entry needed; no SPEC-sensitive paths touched.

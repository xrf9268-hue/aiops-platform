# First run: Docker + Linear + Codex

This is the supported first-run path for a new operator who wants to validate
aiops-platform with Docker, Linear, and `codex app-server`.

## Support matrix

| Area | Stable path | Fallback | Not supported yet / blocker |
| --- | --- | --- | --- |
| macOS host development | Run `go run ./cmd/worker --doctor --mode=mock` and `agent.default: mock` from source. | Use host Codex CLI for local `codex` / `codex-app-server` workflows after `codex --login`. | Host binary mounts into Linux containers are not supported; install Codex in the image. |
| Linux amd64 Docker worker | Build `Dockerfile` target `codex-worker`; Codex CLI `0.133.0` is installed from a pinned release artifact and checksum. | Use base `worker` target for mock-only validation. | Online installer is not the promoted Docker path until Linux ARM64 checksum behavior is reliable. |
| Linux arm64 Docker worker | Build `codex-worker`; uses the pinned `aarch64-unknown-linux-musl` release artifact and checksum. | Same as above. | `curl https://chatgpt.com/codex/install.sh \| sh` failed on 2026-05-26 because the installer could not find the ARM64 package checksum. |
| Codex auth | Mount a restricted writable `CODEX_HOME` directory into `/home/aiops/.codex` for Codex CLI 0.133; verify with `worker --doctor --mode=real`. | Local host development can use the normal host `~/.codex`; read-only copies are only suitable for archival inspection or future no-write smoke modes. | Passing raw bearer tokens on command lines or in logs is not supported. |
| GitHub agent auth | File-backed `gh` auth from a Docker secret, or a dedicated SSH deploy key, visible to the `aiops` user and verified with `worker --doctor --mode=real --github-issue <n>`. | Read-only issue-only tokens are enough for analysis-only smoke tests; branch push validation needs write-capable repo credentials. | `GH_TOKEN`/`GITHUB_TOKEN` in the worker environment are stripped from agent subprocesses and are not a valid dogfood credential contract. |
| Linear auth | Personal API key in a Docker Compose secret file. Linear expects `Authorization: <API_KEY>` for personal keys. | Local development may use `LINEAR_API_KEY` in `.env`; do not use this for production-style examples. | OAuth app-actor is documented by Linear and is the intended future service-account path, but aiops-platform still accepts a single `tracker.api_key` string today. |
| Sandbox | Mock mode works under the base hardened container. Real Codex Docker validation uses a Docker-isolated profile and explicit `codex.thread_sandbox: danger-full-access` only inside that container boundary. | Enable kernel/user namespace support and keep Codex `workspace-write` if your container profile permits it. | Do not copy `danger-full-access` to a shared host run. |

Official references checked for this path:

- OpenAI Codex CLI getting started: npm install / upgrade, supported local CLI
  behavior, approvals, and sandbox expectations.
- OpenAI Codex CLI ChatGPT sign-in: `codex --login`, local credential storage,
  and revocation behavior.
- OpenAI Codex repository install docs: macOS/Linux/WSL requirements, DotSlash,
  source builds, and release artifacts.
- OpenAI Codex app-server README: prefer token/auth files over raw bearer
  tokens on command lines for manual app-server startup.
- Docker Compose secrets docs: service-scoped secrets are mounted under
  `/run/secrets/<name>` and should be preferred for passwords/API keys.
- Linear GraphQL docs: personal API keys use raw `Authorization: <API_KEY>`;
  OAuth access tokens use `Authorization: Bearer <ACCESS_TOKEN>`.
- Linear OAuth actor authorization docs: `actor=app` is the future
  app/service-account style path for agent integrations.

## 1. Prepare files

```bash
mkdir -p .aiops/secrets .aiops/codex-home
cp examples/WORKFLOW.md .aiops/WORKFLOW.md
# Scope umask 077 so every secret lands as 0600; co-tenant users/processes
# must never get a readable window on these credential files.
( umask 077
  printf '%s' 'replace-with-linear-personal-key' > .aiops/secrets/linear_api_key
  printf '%s' 'replace-with-least-privilege-github-token' > .aiops/secrets/github_token
  openssl rand -hex 24 > .aiops/secrets/state_api_token
)
```

Run `codex --login` on the host, then copy or provision the Codex home you
want the container to read:

```bash
cp ~/.codex/auth.json .aiops/codex-home/auth.json
cp ~/.codex/config.toml .aiops/codex-home/config.toml
chmod 600 .aiops/secrets/* .aiops/codex-home/*
```

Edit `.aiops/WORKFLOW.md`:

- set `repo.clone_url` to a disposable fixture repository for smoke tests;
- set `tracker.project_slug` to the Linear project slug;
- keep `agent.default: mock` for the first smoke;
- switch to `agent.default: codex-app-server` only for the real Codex smoke;
- in Docker real mode, set `codex.command: codex app-server` and make the
  sandbox choice explicit.

## 2. Configure Compose

Use `.env` only for non-secret paths and build settings. Write absolute paths:
the documented command merges Compose files from `deploy/`, and absolute
bind/secret paths avoid project-directory ambiguity.

```bash
cat > .env <<EOF
AIOPS_WORKFLOW_PATH=$PWD/.aiops/WORKFLOW.md
AIOPS_CODEX_HOME_PATH=$PWD/.aiops/codex-home
LINEAR_API_KEY_FILE=$PWD/.aiops/secrets/linear_api_key
GITHUB_TOKEN_FILE=$PWD/.aiops/secrets/github_token
# Optional: only if this deployment still needs a Gitea API token.
# GITEA_TOKEN_FILE=$PWD/.aiops/secrets/gitea_token
AIOPS_STATE_API_TOKEN_FILE=$PWD/.aiops/secrets/state_api_token
AIOPS_UID=$(id -u)
AIOPS_GID=$(id -g)
EOF
```

The Codex overlay reads Linear, GitHub, optional Gitea, and dashboard tokens
from Docker secret files. The entrypoint converts the GitHub secret into
`/home/aiops/.config/gh/hosts.yml` with restrictive permissions and clears
plain `GH_TOKEN`/`GITHUB_TOKEN` environment variables so the preflight matches
the agent shell. The Codex home is a writable bind because Codex CLI 0.133
writes while loading configuration and may persist refreshed auth state. Keep
these directories restricted to the worker UID/GID and do not point them at a
shared host home unless that is your intended trust boundary.

## 3. Run preflight

Host mock preflight:

```bash
go run ./cmd/worker --doctor --mode=mock .aiops/WORKFLOW.md
```

Docker real preflight:

```bash
docker compose --env-file .env \
  -f deploy/docker-compose.yml \
  -f deploy/docker-compose.codex.yml \
  run --rm worker --doctor --mode=real --github-issue 451 \
    --github-repo xrf9268-hue/aiops-platform
```

Expected output is action-oriented:

```text
PASS Linear auth: API key authenticated and project is visible
FAIL Codex auth: ...
     Fix: Run codex --login in the same CODEX_HOME/container user context.
WARN Dashboard state API: not checked; no dashboard URL supplied
     Fix: Pass --dashboard-url while the worker is running to verify state API auth.
```

`--mode=mock` fails on missing workflow, missing `repo.clone_url`, missing
Linear key, and missing required host binaries. It warns, but does not fail, on
Docker/Codex paths that are not needed by the mock runner. On the host,
`--mode=real` also checks Docker Compose. Inside the Docker worker it skips the
host Docker CLI check and validates live Linear auth/project visibility, Codex
CLI version, Codex login status, and a minimal `codex app-server` JSON-RPC
probe. When `--github-issue` is set, it also validates `gh issue view` and
`git push --dry-run` from the exact Codex agent environment. Omit
`--github-repo` only for single-repo workflows; multi-service workflows must
name the target repo so the preflight cannot validate the wrong route.

## 4. Start the long-lived worker

Start production-style workers with the dashboard overlay from the first
long-lived `up`. The base Compose service binds the worker HTTP listener to
container loopback, and Docker cannot add a new published port to an already
running container without recreating it. The overlay binds all interfaces inside
the worker container, publishes only to host loopback, and keeps the state API
token loaded from the secret file configured above.

```bash
docker compose --env-file .env \
  -f deploy/docker-compose.yml \
  -f deploy/docker-compose.codex.yml \
  -f deploy/docker-compose.dashboard.yml \
  up -d --build worker
```

After the worker is healthy, smoke-check host reachability without printing the
state API token:

```bash
docker compose --env-file .env \
  -f deploy/docker-compose.yml \
  -f deploy/docker-compose.codex.yml \
  -f deploy/docker-compose.dashboard.yml \
  ps worker

curl_cfg="$(mktemp)"
chmod 600 "$curl_cfg"
printf 'header = "Authorization: Bearer %s"\n' \
  "$(cat .aiops/secrets/state_api_token)" > "$curl_cfg"
curl --fail --silent --show-error --config "$curl_cfg" \
  http://127.0.0.1:4000/ >/tmp/aiops-dashboard.html
curl --fail --silent --show-error --config "$curl_cfg" \
  http://127.0.0.1:4000/api/v1/state >/tmp/aiops-state.json
rm -f "$curl_cfg"

timeout 15s env AIOPS_STATE_API_TOKEN="$(cat .aiops/secrets/state_api_token)" \
  go run ./cmd/tui --url http://127.0.0.1:4000/ --raw >/tmp/aiops-tui.txt \
  || test "$?" -eq 124
```

`cmd/tui` keeps polling until interrupted; the `timeout` command is expected to
stop it after a frame has been written.

Open `http://127.0.0.1:4000/` in a browser and let the Basic-auth prompt ask
for credentials. Use username `aiops` and the contents of
`.aiops/secrets/state_api_token` as the password. Do not put credentials in the
URL.

If a worker is already running without the dashboard overlay and cannot be
recreated yet, use a host-local tunnel as a low-downtime bridge until the next
planned restart. This keeps the host listener on loopback and preserves the
worker's existing state API authentication; it requires `socat` on the host and
`bash` in the worker image.

```bash
worker_container="$(docker compose --env-file .env \
  -f deploy/docker-compose.yml \
  -f deploy/docker-compose.codex.yml \
  ps -q worker)"

socat TCP-LISTEN:4000,bind=127.0.0.1,reuseaddr,fork \
  EXEC:"docker exec -i $worker_container bash -lc 'exec 3<>/dev/tcp/127.0.0.1/4000; cat <&0 >&3 & cat <&3 >&1; wait'",nofork
```

Run the same `curl` and `cmd/tui --raw` smoke checks through the tunnel. Treat
the tunnel as temporary operational access; keep the dashboard overlay in the
normal long-lived Compose command so future restarts do not need it.

## 5. Run a todo smoke

Prepare or select one disposable Linear issue in an active state. For the first
pass, keep `agent.default: mock`:

```bash
go build -o /tmp/aiops-worker ./cmd/worker
AIOPS_SMOKE_WORKER_BIN=/tmp/aiops-worker \
  scripts/aiops-todo-smoke.sh \
  --mode mock \
  --workflow .aiops/WORKFLOW.md \
  --issue AIS-123
```

The smoke script:

- runs `worker --doctor`;
- starts a worker on `127.0.0.1:4010`;
- triggers `/api/v1/refresh`;
- waits for one lifecycle to increment `completed_total` or `failed_total`;
- writes a timestamped report under `docs/validation/smoke/`;
- records the state snapshot and worker log paths without printing secrets.

For real Codex mode, switch the workflow to `agent.default: codex-app-server`,
allow only the Linear mutations you need (`issueUpdate`, `commentCreate`), and
run:

```bash
AIOPS_SMOKE_WORKER_BIN=/tmp/aiops-worker \
  scripts/aiops-todo-smoke.sh \
  --mode real \
  --workflow .aiops/WORKFLOW.md \
  --issue AIS-124
```

Verify the report, Linear comment, final Linear state, and workspace cleanup
before considering the install validated.

When validating writeback behavior, the disposable issue must exercise the real
agent-side tracker tool path. A validation-only prompt can prove that
`codex app-server` accepts the schema, but it does not prove the full Symphony
contract where the agent performs tracker writes through advertised tools.

For a production-style writeback check, use a fresh disposable issue and require
the agent to:

- write `.aiops/PLAN.md` and `.aiops/RUN_SUMMARY.md`;
- call the advertised `linear_graphql` tool for `commentCreate`;
- call the advertised `linear_graphql` tool for `issueUpdate` to move the issue
  to a terminal state;
- avoid shell `curl`, source edits, pushes, and pull requests.

After the run, verify Linear independently with the API or UI: the expected
comment must exist, the issue must be in the expected terminal state, the worker
must report `runner_end ok:true` and `verify_end status:ok`, and the issue
workspace should be cleaned up on the next reconciliation. If you rerun the same
disposable issue id, clean the previous temporary worktree first; workspace
branch names are derived from issue ids and can collide.

## 6. Run a GitHub issue-to-PR smoke

Use this only with a disposable GitHub issue and a disposable Linear mirror.
The goal is to validate the dogfood path that this repo's batch workflow needs:
the agent reads a GitHub issue with `gh`, implements in an isolated workspace,
pushes a branch, and opens a draft PR that closes the GitHub issue.

Prepare one throwaway GitHub issue in the target repository:

```bash
gh_issue_url="$(gh issue create \
  --repo xrf9268-hue/aiops-platform \
  --title "Docker dogfood PR smoke $(date -u +%Y%m%dT%H%M%SZ)" \
  --body "Disposable validation issue. The agent should make a tiny docs-only change, open a draft PR, and leave this issue to be closed by the PR.")"
gh_issue="${gh_issue_url##*/}"
```

Mirror that issue into a fresh Linear issue in the configured project. The
Linear issue title/body must include the GitHub issue number and URL, and the
workflow prompt must instruct the agent to read the GitHub issue with
explicit fields, then read comments through the REST comments endpoint:

```bash
gh issue view "$gh_issue" \
  --repo xrf9268-hue/aiops-platform \
  --json number,title,state,labels,body,url

gh api --paginate \
  "repos/xrf9268-hue/aiops-platform/issues/${gh_issue}/comments?per_page=100"
```

Do not use `gh issue view --comments` in the dogfood workflow. Older GitHub CLI
versions query the deprecated Projects Classic `projectCards` field for that
shape, and GitHub returns a GraphQL deprecation error before the agent reads the
issue. Treat that exact `repository.issue.projectCards` failure as a GitHub CLI
query-shape issue, not an authentication failure. Also avoid unsupported
`gh issue view --json` fields such as `closedBy`; use `closed` and `closedAt`
only if the installed `gh issue view --json` help lists them.

Run the smoke script against the Linear identifier for that mirror:

```bash
AIOPS_SMOKE_WORKER_BIN=/tmp/aiops-worker \
  scripts/aiops-todo-smoke.sh \
  --mode real \
  --workflow WORKFLOW.md \
  --issue AIS-125 \
  --github-repo xrf9268-hue/aiops-platform \
  --github-issue "$gh_issue" \
  --expect-draft-pr
```

The script first runs `worker --doctor --mode=real --github-issue` so `gh issue
view` and `git push --dry-run` are checked from the sanitized Codex agent
environment. After the selected Linear issue completes, it verifies that an
open draft PR in the target GitHub repo has a `closingIssuesReferences` link to
the disposable GitHub issue. If no draft PR exists, the smoke fails and writes
the state snapshot plus worker log paths into `docs/validation/smoke/`.
For slow GitHub PR visibility or long-running handoff paths, tune
`AIOPS_SMOKE_PR_POLL_ATTEMPTS` and `AIOPS_SMOKE_PR_POLL_INTERVAL_SECONDS`.

After a successful smoke, close or merge the disposable draft PR according to
the normal PR follow-through gate, then close any intentionally disposable
GitHub/Linear test issue that is not closed by the PR.

## Troubleshooting

| Symptom | Next action |
| --- | --- |
| `FAIL Linear API key` | Make sure the workflow uses `api_key: $LINEAR_API_KEY`; for Docker, set `LINEAR_API_KEY_FILE` and merge `deploy/docker-compose.codex.yml`. |
| `FAIL Linear auth` | Personal keys must be sent raw, not as `Bearer`; confirm the token can see `tracker.project_slug`. |
| `FAIL Codex CLI` | Build the `codex-worker` target or install Codex on the host. |
| `FAIL Codex auth` | Run `codex --login` for the same `CODEX_HOME` and container user context. |
| `FAIL GitHub agent gh auth` | Set `GITHUB_TOKEN_FILE` to a least-privilege token file or mount a deploy key; do not rely on `GH_TOKEN`/`GITHUB_TOKEN` in the worker environment. |
| `FAIL GitHub agent git push` | Run the documented doctor command from the container and fix `gh auth setup-git` or deploy-key write access for the target repo. |
| `FAIL no new open draft PR ... closes GitHub issue` | Confirm the mirrored Linear issue tells the agent to open a draft PR with `Closes #<n>`, and that the GitHub credential has repo write access. |
| `WARN Codex sandbox` | Either use the documented Docker-isolated profile for real smoke validation or enable the kernel/user namespace support required by Codex `workspace-write`. |
| smoke timeout | Confirm a disposable issue is in an active state, `/readyz` is healthy, and `tracker.active_states` matches the Linear board. |

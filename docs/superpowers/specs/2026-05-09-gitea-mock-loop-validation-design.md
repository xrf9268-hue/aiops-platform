# Gitea `/ai-run` mock loop end-to-end validation — design

**Issue:** [#5](https://github.com/xrf9268-hue/aiops-platform/issues/5) — `[M1][P0] Validate Gitea /ai-run mock loop end to end`
**Date:** 2026-05-09
**Status:** approved (awaiting implementation plan)

## Goal

Prove that the full Gitea path works against a real Gitea API surface, in a form
that re-runs on every PR and catches regressions automatically. The proof must
exercise the production code path from webhook receipt through Gitea PR
creation; it is not a unit test.

The current state: the Day 1 runbook (`docs/day1-runbook.md`) describes the
loop, but no automated check has ever exercised it. Every milestone after M1
builds on the assumption that this loop is sound.

## Approach summary

Write a Go integration test suite that:

- Spins up Postgres and Gitea via `testcontainers-go`.
- Runs `trigger-api` and `worker` **in-process** in the test binary.
- Drives Gitea through its REST API to create the bot user, repo, webhook,
  issue, and `/ai-run` comment.
- Asserts task state in Postgres and PR state through the Gitea API.

Gated by `//go:build e2e`; runs in a dedicated CI job parallel to the existing
`go` job.

## Out of scope

- `docker-compose.yml` changes (e2e lives only inside testcontainers).
- E2E coverage of `verify.timeout`, retry, or runner-timeout paths (already
  covered by unit tests; e2e cost-to-value too low).
- Adding `e2e` to required PR checks — that is a repo-settings follow-up
  tracked separately, not part of this spec.

---

## §1 — Architecture

### Production code refactors

Two new internal packages, one helper export, no behavior changes:

- **`internal/triggerapi/`** — receives the body of `cmd/trigger-api/main.go`.
  - `server.go`: `Server` struct, `NewServer(store Store, secret string) *Server`, exported `Store` interface (rename of unexported `taskStore`).
  - `routes.go`: `Routes(s *Server) http.Handler`.
  - `handlers.go`: existing `handle*` methods.
  - `cmd/trigger-api/main.go` shrinks to env-parse + `triggerapi.NewServer` + `http.ListenAndServe(addr, triggerapi.Routes(s))`.

- **`internal/worker/`** — receives the body of `cmd/worker/main.go`,
  `print_config.go`, and the existing `cmd/worker/*_test.go` files.
  - `run.go` exposes:

    ```go
    type Config struct {
        DSN           string
        WorkspaceRoot string
        MirrorRoot    string
        GiteaBaseURL  string
        GiteaToken    string
        PollInterval  time.Duration // default 3s; tests use 200ms
        IdleSleep     time.Duration // default 3s; tests use 200ms
    }

    func LoadConfigFromEnv() Config
    func Run(ctx context.Context, pool *pgxpool.Pool, cfg Config)
    func PrintConfig(workdir string, stdout, stderr io.Writer) int
    ```

  - `Run` is the existing `for { Claim → runTask → Complete/Fail }` loop with
    one change: bare `time.Sleep` becomes a `select` on `ctx.Done()` so the
    loop returns cleanly on cancel. No return value: the pool is owned by the
    caller, claim/run errors are logged in-loop (matches today's behavior),
    and the only exit is `ctx.Done()`.
  - `cmd/worker/main.go` shrinks to env-parse + `signal.NotifyContext` +
    `worker.Run(ctx, pool, cfg)`. The signal handling is a side-benefit
    refactor — the production binary currently does not respond to SIGTERM.
    Init errors (e.g., `pgxpool.New` failure) stay in `main` and `log.Fatal`,
    so `Run` itself has no fatal-init path.

- **`internal/gitea/webhook.go`** — add `Sign(secret string, body []byte) string`,
  the inverse of the existing `VerifySignature`. Round-trip pinned in
  `webhook_test.go`. Tests use this to craft validly signed bodies.

### Test layout

```
test/e2e/
├── e2e_test.go      # 4 Test* functions, build-tagged
├── setup.go         # TestMain, testbed lifecycle
├── gitea.go         # giteaEnv: container + REST helpers
├── services.go      # in-process trigger-api + worker
├── sign.go          # thin wrappers over gitea.Sign
├── poll.go          # pollUntil generic helper
└── fixtures/
    ├── mock-happy.md
    └── mock-allow-fail.md
```

### Container topology (test process only)

```
testcontainers-go
├── postgres:16                      ← migrations/001_init.sql injected via /docker-entrypoint-initdb.d
└── gitea/gitea:1.21.11-rootless     ← admin user + token created via REST after readiness
                                       host.docker.internal:host-gateway extra host
                                       so webhooks can reach the test process
trigger-api                          ← httptest.NewServer(triggerapi.Routes(...))
worker                               ← go func() { worker.Run(ctx, pool, cfg) }()
```

Both `trigger-api` and `worker` run in the test process. `httptest.Server`
exposes a real port that the Gitea container reaches via
`host.docker.internal` (see §6 for why this is non-negotiable on Linux CI).

---

## §2 — Test scenarios

> Note: `source_event_id` on the Gitea webhook path is derived from the
> `X-Gitea-Delivery` header (per-delivery UUID), **not** from comment ID. So
> idempotency triggers on Gitea webhook redelivery, not on a user posting two
> distinct `/ai-run` comments on the same issue. The dedup test below reflects
> this reality.

### 1. `TestGiteaMockLoop_HappyPath`

**Setup**

- Bootstrap a fresh repo `aiops-bot/demo-happy`.
- Create `WORKFLOW.md` from `fixtures/mock-happy.md` via the Gitea contents API.
- Create an `issue_comment` webhook pointing at the `httptest.Server` URL,
  using a per-test HMAC secret.
- Create an issue.

**Act**

- POST `/ai-run` as a comment via Gitea API.
- `pollUntil` Postgres `tasks.status='succeeded'` (timeout 60s).

**Assert**

- `tasks.work_branch` matches `^ai/tsk_`.
- `task_events` contains at least: `workflow_resolved`, `runner_start`,
  `pr_opened`.
- Gitea: `GET /repos/aiops-bot/demo-happy/branches/<work_branch>` → 200.
- Gitea: `GET /repos/aiops-bot/demo-happy/pulls?state=open` → 1 PR, `draft=true`,
  body contains `.aiops/` (no byte-exact match — see §6).

### 2. `TestWebhookBadSignature`

No Gitea repo or webhook needed; calls trigger-api directly.

**Act**

- POST a forged-signature `issue_comment` payload to `/v1/events/gitea`.

**Assert**

- HTTP 401.
- Postgres `tasks` row count for this test's `created_at` window is 0.

### 3. `TestWebhookRedelivery_Deduped`

Direct POST to trigger-api with a single `X-Gitea-Delivery` UUID, replayed twice
(simulates Gitea's automatic retry on 5xx/network failure).

**Act**

- Build a valid `issue_comment` body, sign with `gitea.Sign`, POST twice with
  identical headers and body.

**Assert**

- First response: `{accepted:true, task_id:T1, deduped:false}`.
- Second response: `{accepted:true, task_id:T1, deduped:true}` (same task ID).
- Postgres: exactly one `tasks` row with `source_event_id=<delivery uuid>`.

### 4. `TestVerifyAllowFailure`

**Setup**

- Repo `aiops-bot/demo-allow-fail`, `WORKFLOW.md` from
  `fixtures/mock-allow-fail.md` (verify command `false`,
  `verify.allow_failure: true`, `pr.draft: false`).

**Act**

- Same as test 1: webhook + issue + `/ai-run`.

**Assert**

- `tasks.status='succeeded'`.
- `task_events` contains a `verify_end` event with payload
  `status: failed_allowed`.
- Gitea PR: `draft=true` (the allow_failure path forces draft regardless of
  workflow), body contains a degraded-mode banner referencing
  `.aiops/VERIFICATION.txt`.

---

## §3 — Fixtures, Gitea bootstrap, signing

### WORKFLOW.md fixtures

Both fixtures intentionally omit the `tracker` block — the Gitea path does not
require Linear configuration, and the workflow loader accepts the absence.

**`fixtures/mock-happy.md`**

```yaml
---
agent:
  default: mock
  timeout: 5m
policy:
  mode: draft_pr
  max_changed_files: 12
  max_changed_loc: 300
verify:
  commands: []
pr:
  draft: true
  labels: [ai-generated, needs-review]
---
Run mock task {{ task.id }} for {{ repo.owner }}/{{ repo.name }}.
```

**`fixtures/mock-allow-fail.md`** — same prompt body, replace verify and pr blocks:

```yaml
verify:
  commands: ["false"]
  allow_failure: true
pr:
  draft: false   # exercise the allow_failure path that forces draft
  labels: [ai-generated, needs-review]
```

### `giteaEnv` REST helpers

```go
type giteaEnv struct {
    baseURL  string  // testcontainer mapped port
    botUser  string  // "aiops-bot"
    botToken string
}

func startGitea(ctx context.Context) (*giteaEnv, func(), error)

func (g *giteaEnv) createRepo(ctx context.Context, name string) (cloneURL string, err error)
func (g *giteaEnv) putFile(ctx context.Context, owner, repo, path string, content []byte, msg string) error
func (g *giteaEnv) createWebhook(ctx context.Context, owner, repo, url, secret string) error
func (g *giteaEnv) createIssue(ctx context.Context, owner, repo, title, body string) (int, error)
func (g *giteaEnv) commentIssue(ctx context.Context, owner, repo string, issue int, body string) (int64, error)
func (g *giteaEnv) listOpenPRs(ctx context.Context, owner, repo string) ([]gitea.PullRequest, error)
func (g *giteaEnv) getBranch(ctx context.Context, owner, repo, branch string) (bool, error)
```

### Gitea container configuration

- Image: `gitea/gitea:1.21.11-rootless`.
- Env: `INSTALL_LOCK=true`, `SECRET_KEY=<random>`, `DB_TYPE=sqlite3`,
  `DISABLE_SSH=true`, `RUN_MODE=prod`.
- Bot creation: after readiness probe (`GET /api/v1/version` 200), POST to
  `/api/v1/users/signup` to create `aiops-bot`, then exchange basic auth for an
  access token via `POST /api/v1/users/aiops-bot/tokens`.
- Files are seeded via the contents API
  (`POST /api/v1/repos/{owner}/{repo}/contents/{path}`); no `git push` from
  tests, no SSH keys to manage.

### HMAC signing

`internal/gitea.Sign(secret string, body []byte) string` returns
`"sha256=<hex>"`. Test code:

```go
body := mustJSON(payload)
req.Header.Set("X-Gitea-Signature", gitea.Sign(secret, body))
req.Header.Set("X-Gitea-Delivery", deliveryUUID)
req.Header.Set("X-Gitea-Event", "issue_comment")
```

`webhook_test.go` gets a sign/verify round-trip pin so both functions stay in
lockstep.

### `services.go`

The testbed is a single instance owned by `TestMain` (not a per-test value, to
amortize ~30s container startup). Tests reach it via package-global. Cleanup
between tests happens through `t.Cleanup` truncating Postgres tables.

```go
type testbed struct {
    pg         *pgxpool.Pool
    gitea      *giteaEnv
    triggerSrv *httptest.Server
    secret     string
    workerStop func() // ctx cancel + WaitGroup wait, 5s deadline
}

// setupTestbed builds the testbed once for the whole package run.
// Errors are returned (not t.Fatal) because TestMain has no *testing.T.
func setupTestbed(ctx context.Context) (*testbed, error)

// resetState is called via t.Cleanup in each test; truncates tasks and
// task_events without tearing down containers.
func (b *testbed) resetState(t *testing.T)
```

`TestMain`:

```go
func TestMain(m *testing.M) {
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()
    var err error
    bed, err = setupTestbed(ctx)
    if err != nil { log.Fatal(err) }
    code := m.Run()
    bed.workerStop()
    os.Exit(code)
}
```

`triggerSrv = httptest.NewServer(triggerapi.Routes(triggerapi.NewServer(store, secret)))`.

Worker is launched as `go func() { defer wg.Done(); _ = worker.Run(ctx, pool, cfg) }()`
with `cfg.WorkspaceRoot = t.TempDir()`, `cfg.MirrorRoot = t.TempDir()`,
`cfg.PollInterval = 200ms`, `cfg.IdleSleep = 200ms`.

### Polling

```go
// pollUntil waits for fn to return (true, nil) or fails the test on timeout.
func pollUntil(t *testing.T, timeout, interval time.Duration, fn func(context.Context) (bool, error))
```

Used for "task succeeded" and "PR exists" waits. Default timeout 60s; the
underlying loop should resolve in 5–15s, leaving ample margin.

---

## §4 — Refactor risks and mitigations

| Concern | Severity | Mitigation |
|---|---|---|
| Import cycle from new packages | Low | `internal/worker` and `internal/triggerapi` only import existing `internal/*` and `task`; they do not import each other |
| Behavior drift in `runTask` during package move | Med | "Move only, no edits" rule for the refactor commit; PR review checks every hunk is pure relocation |
| `signal.NotifyContext` causes early exit | Low | Already a normal cancel path; covered by tests |
| `--print-config` regression | Low | `print_config_test.go` migrates with the package and runs in the existing `go` CI job |
| External imports of `cmd/*` | None | `package main` is unimportable; only `cmd/*/*_test.go` files are affected, and they migrate too |

---

## §5 — CI integration

### New `e2e` job

`.github/workflows/ci.yml` gains a sibling job, parallel to `go` (no `needs:`):

```yaml
e2e:
  name: E2E Gitea mock loop
  runs-on: ubuntu-latest
  timeout-minutes: 20
  steps:
    - uses: actions/checkout@v6
      with:
        persist-credentials: false
    - uses: actions/setup-go@v6
      with:
        go-version-file: go.mod
        cache: true
        cache-dependency-path: |
          go.mod
          go.sum
    - name: Pre-pull container images
      run: |
        docker pull postgres:16
        docker pull gitea/gitea:1.21.11-rootless
    - name: Run e2e tests
      run: go test -tags e2e -race -timeout 15m ./test/e2e/...
```

Pre-pulling separates image-pull failures from test failures, and amortizes the
slowest setup step before `go test` starts the testcontainer wait loops.

### Gating

Adding `e2e` to the `main` branch protection's required checks is a follow-up
repo-settings change, not part of this spec's code PR.

### Local development

- `go test ./...` continues to ignore e2e (build tag isolation).
- E2E run: `go test -tags e2e -race ./test/e2e/...`. Requires local Docker.
- `docs/runbooks/local-dev.md` gains a short section: command, expected runtime
  (cold ~3min, warm ~45s), and "Docker isn't running" troubleshooting.

---

## §6 — Risks and stability tactics

| Risk | Trigger | Mitigation |
|---|---|---|
| testcontainers cold-pull timeout | First CI run after image bump | §5 pre-pull step; 15min test timeout headroom |
| Gitea API not ready when first call hits | Container up but Gitea init not done | `startGitea` polls `/api/v1/version` for up to 60s |
| Webhook delivery latency | Gitea fires webhooks asynchronously | `pollUntil` with 30s budget |
| Postgres readiness race | Container up but `pg_isready` not yet | `testcontainers-go/modules/postgres.Run` with `WaitForLog("ready to accept connections")` |
| Cross-test row contamination | Tests share Postgres | TestMain owns the testbed; per-test repos use distinct names; `t.Cleanup` truncates `tasks` and `task_events` after each test; assertions filter on `created_at >= test_start` for defense in depth |
| Worker goroutine leak | ctx canceled but loop blocked on Claim | `WaitGroup` wait with 5s deadline in `workerStop`; deadline exceeded becomes `t.Errorf` not a hang |
| Port conflicts under parallel CI | Multiple jobs on one runner | testcontainers-mapped host ports; `httptest.NewServer` random port; nothing hardcoded |
| Mock runner output drift | Future internal change | PR-body assertions check substring presence (`.aiops/`), not byte-exact content |
| `--print-config` regression from package move | Refactor diff | Existing tests migrate with package; covered in `go` CI job, not duplicated in e2e |

### Critical: webhook callback addressing

The single sharpest gotcha. The Gitea container needs to reach the
`httptest.Server` running in the test process. On Linux GitHub runners,
`127.0.0.1` from inside the container does not resolve to the test process.

```go
giteaContainer := testcontainers.GenericContainer(ctx, ContainerRequest{
    Image:      "gitea/gitea:1.21.11-rootless",
    ExtraHosts: []string{"host.docker.internal:host-gateway"},
    // ...
})

// Webhook URL fed to Gitea:
webhookURL := strings.Replace(triggerSrv.URL, "127.0.0.1", "host.docker.internal", 1)
```

Without `host-gateway`, webhooks return 401/timeout on Linux CI even though the
test passes locally on macOS. Pin this in code with a comment block referencing
this section of the spec.

---

## Implementation staging

The implementation plan should split into two PRs to keep review tractable:

1. **Refactor PR** — move `cmd/trigger-api` and `cmd/worker` bodies to
   `internal/triggerapi` and `internal/worker`; add `gitea.Sign` and its
   round-trip test; thin `cmd/*/main.go`. **No new tests, no e2e infra.**
   Reviewer's job is "every hunk is pure relocation" plus signature parity.
   Existing CI must stay green.

2. **E2E PR** — add `test/e2e/`, fixtures, the four tests, the new CI job,
   and the local-dev runbook section. Builds on a clean refactor base.

Splitting this way means a refactor regression cannot hide inside e2e churn,
and a flaky e2e cannot block a clean refactor merge.

## Decision log

- **Build tag over t.Skip**: gating via `//go:build e2e` prevents regular
  `go test ./...` from doing any container work; opt-in is explicit.
- **In-process services over subprocess**: cheaper, easier to assert, and
  the production binaries are already thin wrappers over `Run`/`Routes`.
- **REST API over `git push`**: contents API removes SSH key plumbing and
  keeps fixtures simple.
- **Shared testbed in TestMain**: amortizes ~30s startup; per-test isolation
  via distinct repo names plus truncate-on-cleanup. Per-test testbed instances
  would multiply CI time without proportional value.
- **Pin Gitea to 1.21.11-rootless**: matches what production runs; image bumps
  are deliberate, not silent.
- **Don't add `e2e` to required checks in this PR**: repo-settings change is
  out of scope for the code PR; tracked as a follow-up.

## Acceptance criteria mapping (from issue #5)

| AC | Covered by |
|---|---|
| Create issue in demo repo | `giteaEnv.createIssue` in tests 1, 4 |
| Comment `/ai-run` | `giteaEnv.commentIssue` in tests 1, 4 |
| Trigger API enqueues task | All four tests; tests 1/4 via webhook, 2/3 via direct POST |
| Worker claims task | Tests 1, 4 (assert `tasks.status` and events) |
| Worker creates `ai/tsk_...` branch | Tests 1, 4 (Gitea API branch check) |
| Worker creates Gitea PR | Tests 1, 4 (Gitea API PR list check) |
| Task ends as `succeeded` | Tests 1, 4 (`tasks.status` poll) |

## Follow-ups (out of scope here)

- Add `e2e` to `main` branch protection required checks (repo settings).
- Track a separate issue for retry-path and runner-timeout e2e if production
  ever loses confidence in the unit-test coverage of those paths.

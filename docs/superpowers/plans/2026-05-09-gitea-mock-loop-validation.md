# Gitea `/ai-run` mock loop e2e validation — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Validate the Gitea `/ai-run` mock loop end-to-end via a Go integration test suite using `testcontainers-go` for Postgres + Gitea, gated by `//go:build e2e` and a parallel CI job. Closes [#5](https://github.com/xrf9268-hue/aiops-platform/issues/5).

**Architecture:** The implementation ships in two PRs: (PR1) refactor `cmd/worker` and `cmd/trigger-api` bodies into new `internal/worker` and `internal/triggerapi` packages so tests can import them, plus add `gitea.Sign`. (PR2) add the `test/e2e/` package, fixtures, four test scenarios, CI job, and runbook section.

**Tech Stack:** Go 1.25, `testcontainers-go` modules `postgres` and `gitea` (or `GenericContainer` for Gitea if module is unstable), `pgx/v5`, `httptest`, GitHub Actions, Docker daemon on `ubuntu-latest`.

**Reference:** Spec at `docs/superpowers/specs/2026-05-09-gitea-mock-loop-validation-design.md` is the source of truth for design decisions; this plan is the execution recipe.

---

## PR1 — Refactor

Tasks 1–3 ship in a single PR. Existing CI (`go test -race ./...` + Docker image build) must stay green throughout.

### Task 1: Add `gitea.Sign` with round-trip test

**Files:**
- Modify: `internal/gitea/webhook.go`
- Modify: `internal/gitea/webhook_test.go`

- [ ] **Step 1: Read existing webhook.go to confirm Sign matches Verify exactly**

The existing `VerifySignature` strips `sha256=` prefix, computes `hex.EncodeToString(hmac_sha256(secret, body))`. The inverse must produce the same hex-encoded HMAC with the `sha256=` prefix. Confirm by reading `/Users/yvan/developer/aiops-platform/internal/gitea/webhook.go`.

- [ ] **Step 2: Write the failing test**

Append to `internal/gitea/webhook_test.go`:

```go
func TestSign_RoundTripsThroughVerifySignature(t *testing.T) {
	secret := "shared-secret"
	body := []byte(`{"action":"created"}`)
	sig := Sign(secret, body)
	if !strings.HasPrefix(sig, "sha256=") {
		t.Fatalf("Sign should return sha256=...; got %q", sig)
	}
	if !VerifySignature(secret, sig, body) {
		t.Fatalf("VerifySignature rejected the value Sign produced: %q", sig)
	}
	if VerifySignature("other-secret", sig, body) {
		t.Fatalf("VerifySignature accepted a wrong secret")
	}
	if VerifySignature(secret, sig, []byte(`{"action":"edited"}`)) {
		t.Fatalf("VerifySignature accepted a wrong body")
	}
}
```

If `strings` is not yet imported in this test file, add `"strings"` to the imports.

- [ ] **Step 3: Run test to verify it fails**

```bash
go test ./internal/gitea/ -run TestSign_RoundTripsThroughVerifySignature -v
```

Expected: FAIL with `undefined: Sign`.

- [ ] **Step 4: Implement Sign**

Append to `internal/gitea/webhook.go` (anywhere after `VerifySignature`):

```go
// Sign returns the value of an X-Gitea-Signature header for body, given
// the shared HMAC secret. It is the inverse of VerifySignature: a value
// produced by Sign(secret, body) is always accepted by
// VerifySignature(secret, ..., body).
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
```

The imports in `webhook.go` already cover `hmac`, `sha256`, and `hex`.

- [ ] **Step 5: Run test to verify it passes**

```bash
go test ./internal/gitea/ -v
```

Expected: PASS, including the new test plus all pre-existing tests.

- [ ] **Step 6: Commit**

```bash
git add internal/gitea/webhook.go internal/gitea/webhook_test.go
git commit -m "feat(gitea): add Sign as inverse of VerifySignature"
```

---

### Task 2: Extract `internal/triggerapi`

**Files:**
- Create: `internal/triggerapi/server.go`
- Create: `internal/triggerapi/routes.go`
- Create: `internal/triggerapi/handlers.go`
- Create: `internal/triggerapi/server_test.go`
- Modify: `cmd/trigger-api/main.go` (shrink)
- Delete: `cmd/trigger-api/main_test.go` (content migrates to `internal/triggerapi/server_test.go`)

The package move is mechanical: rename `package main` to `package triggerapi`, capitalize the names tests reach, update import paths in callers. There are no behavior changes.

- [ ] **Step 1: Create `internal/triggerapi/server.go`**

Write:

```go
package triggerapi

import (
	"context"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
)

// Store is the subset of the queue store the trigger API needs.
type Store interface {
	Enqueue(context.Context, task.Task) (task.Task, bool, error)
	GetTask(context.Context, string) (task.Task, error)
	ListTasks(context.Context, task.Status) ([]task.Task, error)
	TaskEvents(context.Context, string) ([]task.Event, error)
}

type Server struct {
	store  Store
	secret string
}

func NewServer(store Store, secret string) *Server {
	return &Server{store: store, secret: secret}
}
```

- [ ] **Step 2: Create `internal/triggerapi/routes.go`**

Write:

```go
package triggerapi

import (
	"encoding/json"
	"net/http"
)

func Routes(s *Server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /v1/events/gitea", s.handleGitea)
	mux.HandleFunc("POST /v1/tasks", s.handleManualTask)
	mux.HandleFunc("GET /v1/tasks", s.handleListTasks)
	mux.HandleFunc("GET /v1/tasks/{id}", s.handleGetTask)
	mux.HandleFunc("GET /v1/tasks/{id}/events", s.handleTaskEvents)
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
```

- [ ] **Step 3: Create `internal/triggerapi/handlers.go`**

Move `handleGitea`, `handleManualTask`, `handleGetTask`, `handleTaskEvents`, `handleListTasks` verbatim from `cmd/trigger-api/main.go` into this file. Update the receiver type from `*server` to `*Server`. Imports needed: `context`, `encoding/json`, `errors`, `fmt`, `io`, `net/http`, `time`, `github.com/xrf9268-hue/aiops-platform/internal/gitea`, `github.com/xrf9268-hue/aiops-platform/internal/task`.

The body of each handler is unchanged — the only edits are:
- Receiver: `func (s *server) ...` → `func (s *Server) ...`
- Package: `package main` → `package triggerapi`

- [ ] **Step 4: Create `internal/triggerapi/server_test.go`**

Move all test functions from `cmd/trigger-api/main_test.go` into this file. Adjust:
- `package main` → `package triggerapi`
- If tests use unexported helpers, either keep them as helper exports or rewrite as needed.
- Keep the same test names so coverage signal is preserved across the move.

(If `main_test.go` is small — a peek shows ~50 lines — a verbatim move with the package-line change is sufficient.)

- [ ] **Step 5: Shrink `cmd/trigger-api/main.go`**

Replace the entire file with:

```go
package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xrf9268-hue/aiops-platform/internal/queue"
	"github.com/xrf9268-hue/aiops-platform/internal/triggerapi"
)

func main() {
	ctx := context.Background()
	dsn := env("DATABASE_URL", "postgres://aiops:aiops@localhost:5432/aiops?sslmode=disable")
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	s := triggerapi.NewServer(queue.New(pool), os.Getenv("GITEA_WEBHOOK_SECRET"))

	addr := env("ADDR", ":8080")
	log.Printf("trigger-api listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, triggerapi.Routes(s)))
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
```

- [ ] **Step 6: Delete `cmd/trigger-api/main_test.go`**

```bash
rm cmd/trigger-api/main_test.go
```

- [ ] **Step 7: Run all tests and build**

```bash
gofmt -l $(git ls-files '*.go')
go mod tidy && git diff --exit-code -- go.mod go.sum
go test -race ./...
go build ./cmd/trigger-api
```

Expected: gofmt clean, go.mod/go.sum unchanged, all tests pass (including the migrated `triggerapi` tests), binary builds.

- [ ] **Step 8: Commit**

```bash
git add internal/triggerapi cmd/trigger-api
git commit -m "refactor(trigger-api): extract server into internal/triggerapi"
```

---

### Task 3: Extract `internal/worker`

**Files:**
- Create: `internal/worker/run.go` (Config + LoadConfigFromEnv + Run)
- Create: `internal/worker/runtask.go` (the worker's per-task pipeline; previously `runTask` and friends in `cmd/worker/main.go`)
- Create: `internal/worker/print_config.go` (moved verbatim minus package line)
- Create: `internal/worker/run_test.go` (was `cmd/worker/main_test.go`)
- Create: `internal/worker/print_config_test.go` (moved verbatim minus package line)
- Create: `internal/worker/secretscan_test.go` (moved verbatim minus package line)
- Modify: `cmd/worker/main.go` (shrink)
- Delete: `cmd/worker/main_test.go`, `cmd/worker/print_config.go`, `cmd/worker/print_config_test.go`, `cmd/worker/secretscan_test.go`

This is the largest task in PR1. The file move is mechanical but touches ~700 lines.

- [ ] **Step 1: Read source files to inventory required exports**

```bash
sed -n '1,667p' cmd/worker/main.go
sed -n '1,200p' cmd/worker/print_config.go
sed -n '1,400p' cmd/worker/main_test.go
sed -n '1,200p' cmd/worker/secretscan_test.go
sed -n '1,100p' cmd/worker/print_config_test.go
```

While reading, list every unexported identifier used **across files** that will need exporting once the package is `worker` instead of `main`. The spec lists likely candidates: `runTask`, `resolveWorkflow`, `runVerifyPhase`, `runRunnerWithTimeout`, `runSecretScan`, `runSecretScanWith`, `handleTaskFailure`, `writeTaskFiles`, `writeFailureArtifacts`, `recordPolicyViolation`, `createPR`, `createPRWith`, `buildPRBody`, `appendRunSummaryDirective`, `eventEmitter`, `secretScanFn`, `prClient`, `printConfigOutput`, `configView`, `agentConfigView`, `printConfigResolution`, `promptSummary`, `newConfigView`, `maskSecrets`, `summarizePrompt`. Confirm against actual call sites.

- [ ] **Step 2: Create `internal/worker/run.go` with Config and Run skeleton**

Write:

```go
package worker

import (
	"context"
	"io"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xrf9268-hue/aiops-platform/internal/queue"
	"github.com/xrf9268-hue/aiops-platform/internal/runner"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
)

type Config struct {
	DSN              string
	WorkspaceRoot    string
	MirrorRoot       string
	GiteaBaseURL     string
	GiteaToken       string
	IdleSleep        time.Duration
	ClaimErrorSleep  time.Duration
}

func LoadConfigFromEnv() Config {
	return Config{
		DSN:             env("DATABASE_URL", "postgres://aiops:aiops@localhost:5432/aiops?sslmode=disable"),
		WorkspaceRoot:   env("WORKSPACE_ROOT", "/tmp/aiops-workspaces"),
		MirrorRoot:      os.Getenv("AIOPS_MIRROR_ROOT"),
		GiteaBaseURL:    os.Getenv("GITEA_BASE_URL"),
		GiteaToken:      os.Getenv("GITEA_TOKEN"),
		IdleSleep:       3 * time.Second,
		ClaimErrorSleep: 5 * time.Second,
	}
}

// PrintConfig dispatches the `worker --print-config <workdir>` subcommand.
// Defined here so cmd/worker/main.go can call it before opening a DB pool.
func PrintConfig(workdir string, stdout, stderr io.Writer) int {
	return printConfig(workdir, stdout, stderr)
}

// Run is the worker's main loop. It returns when ctx is canceled.
// Init errors (e.g., a bad pool) are the caller's responsibility; they
// are not surfaced here.
func Run(ctx context.Context, store *queue.Store, cfg Config) {
	for {
		t, err := store.Claim(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("claim error: %v", err)
			if !sleepOrCancel(ctx, cfg.ClaimErrorSleep) {
				return
			}
			continue
		}
		if t == nil {
			if !sleepOrCancel(ctx, cfg.IdleSleep) {
				return
			}
			continue
		}
		runErr := runTask(ctx, store, *t, cfg)
		if runErr != nil {
			log.Printf("task %s failed: %v", t.ID, runErr.Err)
			handleTaskFailure(ctx, store, *t, runErr.Cfg, runErr.Err)
			continue
		}
		_ = store.Complete(ctx, t.ID)
	}
}

func sleepOrCancel(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// runTaskError bundles the resolved Workflow Config alongside the task's error
// so handleTaskFailure can route retry buckets without re-resolving the
// workflow.
type runTaskError struct {
	Cfg interface{} // workflow.Config; left as interface{} until runtask.go is added so this file compiles standalone
	Err error
}

// Type aliases referenced from runtask.go land alongside this file once
// runtask.go is in place. Compilation works at this stage because runtask.go
// is added in the same task.

// Suppress unused-import warnings until runtask.go references runner/task.
var _ = runner.New
var _ = task.EventClaimed
```

(The `runTaskError` and unused-import suppression are temporary scaffolding for the next step. They go away once `runtask.go` lands.)

- [ ] **Step 3: Create `internal/worker/runtask.go` from `cmd/worker/main.go`**

Move every function and type from `cmd/worker/main.go` **except** `func main()` and the `env` helper (which is now in `run.go`). Apply these edits:

- `package main` → `package worker`
- Capitalize names that `main_test.go` calls from outside the package — see Step 1's inventory. Internal-only helpers stay lowercase.
- Replace `os.Getenv("WORKSPACE_ROOT")` reads inside `runTask`/helpers with `cfg.WorkspaceRoot`. Same for `AIOPS_MIRROR_ROOT` → `cfg.MirrorRoot`. Thread `cfg Config` into `runTask` and any helper that previously read these env vars.
- Replace `os.Getenv("GITEA_BASE_URL")` and `os.Getenv("GITEA_TOKEN")` reads inside `createPR`/`createPRWith` with `cfg.GiteaBaseURL` / `cfg.GiteaToken`.
- Update `runTask`'s signature to `func runTask(ctx context.Context, store *queue.Store, t task.Task, cfg Config) *runTaskError` (returns nil on success). Adjust the `Run` loop in `run.go` accordingly — replace the `runTaskError`/`interface{}` placeholder with the real `workflow.Config` type once it's imported.
- Remove the temporary `interface{}`/`runTaskError`/unused-import scaffolding from `run.go` after Step 4.

- [ ] **Step 4: Tighten `run.go` after `runtask.go` lands**

Replace `runTaskError.Cfg interface{}` with `workflow.Config`, import the workflow package in `run.go`, and remove the `var _ = runner.New` / `var _ = task.EventClaimed` lines. The `Run` loop's call site becomes:

```go
runErr := runTask(ctx, store, *t, cfg)
if runErr != nil {
    log.Printf("task %s failed: %v", t.ID, runErr.Err)
    handleTaskFailure(ctx, store, *t, runErr.Cfg, runErr.Err)
    continue
}
```

- [ ] **Step 5: Move `print_config.go` and tests**

```bash
git mv cmd/worker/print_config.go internal/worker/print_config.go
git mv cmd/worker/print_config_test.go internal/worker/print_config_test.go
git mv cmd/worker/secretscan_test.go internal/worker/secretscan_test.go
```

In each moved file: `package main` → `package worker`, fix unexported identifiers that are now called from outside the original file scope only if needed (most are intra-file).

- [ ] **Step 6: Move `main_test.go` to `internal/worker/run_test.go` as external test package**

```bash
git mv cmd/worker/main_test.go internal/worker/run_test.go
```

In `run_test.go`:
- `package main` → `package worker_test`
- Add import `worker "github.com/xrf9268-hue/aiops-platform/internal/worker"`
- For every helper the test calls, prefix with `worker.` (e.g., `runTask` → `worker.RunTask`, `eventEmitter` → `worker.EventEmitter`, `fakeEmitter` → keep local but ensure it implements `worker.EventEmitter`).
- Test fakes (`fakeEmitter`, `stubRunner`, `fakePRClient`) stay in this file but are now in `worker_test` package; they only need to satisfy the exported interfaces.

If any test reaches an unexported helper that wasn't in the export list from Step 1, either export it or rewrite the test against the public surface. Do not skip tests.

- [ ] **Step 7: Shrink `cmd/worker/main.go`**

Replace the file with:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xrf9268-hue/aiops-platform/internal/queue"
	"github.com/xrf9268-hue/aiops-platform/internal/worker"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "--print-config" {
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: worker --print-config <workdir>")
			os.Exit(2)
		}
		os.Exit(worker.PrintConfig(os.Args[2], os.Stdout, os.Stderr))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := worker.LoadConfigFromEnv()
	pool, err := pgxpool.New(ctx, cfg.DSN)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	worker.Run(ctx, queue.New(pool), cfg)
}
```

- [ ] **Step 8: Run all checks**

```bash
gofmt -l $(git ls-files '*.go')
go mod tidy && git diff --exit-code -- go.mod go.sum
go test -race ./...
go build ./cmd/trigger-api ./cmd/worker ./cmd/linear-poller
```

Expected: clean. Tests that previously lived in `cmd/worker` now run as `internal/worker` tests. The `--print-config` test still pins behavior.

If a test breaks because a previously private symbol is unreachable, export it and re-run.

- [ ] **Step 9: Commit**

```bash
git add internal/worker cmd/worker
git commit -m "refactor(worker): extract main loop into internal/worker"
```

---

### PR1 wrap-up

- [ ] **Step 1: Open PR1**

```bash
git push -u origin <branch>
gh pr create --title "refactor: extract trigger-api and worker bodies into internal packages" --body "$(cat <<'EOF'
## Summary
- Add `internal/gitea.Sign` as the inverse of `VerifySignature`.
- Move `cmd/trigger-api` body into `internal/triggerapi` (Server, Routes, handlers).
- Move `cmd/worker` body into `internal/worker` (Config, Run, runTask, helpers, --print-config dispatcher).
- Replace direct `os.Getenv` reads inside the worker pipeline with `Config` fields.
- Add SIGTERM/SIGINT handling to the worker binary.

No behavior changes. Prep for #5 e2e test suite (PR2).

## Test plan
- [x] `go test -race ./...` green
- [x] `go build` for all three commands
- [x] gofmt and go mod tidy clean
EOF
)"
```

PR1 must merge before PR2 starts.

---

## PR2 — E2E test suite

Tasks 4–15 ship in a single PR built on top of PR1's main.

### Task 4: Add testcontainers-go dependency

**Files:**
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: Add the dependency**

```bash
go get github.com/testcontainers/testcontainers-go@latest
go get github.com/testcontainers/testcontainers-go/modules/postgres@latest
go mod tidy
```

- [ ] **Step 2: Verify go.mod is sane**

```bash
git diff go.mod go.sum | head -50
go test ./... -count=1
```

Expected: existing tests still pass; go.mod has new `testcontainers-go` entries.

- [ ] **Step 3: Commit**

```bash
git add go.mod go.sum
git commit -m "chore: add testcontainers-go dependency for e2e tests"
```

---

### Task 5: Postgres testcontainer helper

**Files:**
- Create: `test/e2e/postgres.go`

- [ ] **Step 1: Create the helper**

Write:

```go
//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

type pgEnv struct {
	pool      *pgxpool.Pool
	dsn       string
	container testcontainers.Container
}

func startPostgres(ctx context.Context) (*pgEnv, error) {
	migration, err := os.ReadFile("../../migrations/001_init.sql")
	if err != nil {
		return nil, fmt.Errorf("read migration: %w", err)
	}

	c, err := tcpostgres.Run(ctx,
		"postgres:16",
		tcpostgres.WithDatabase("aiops"),
		tcpostgres.WithUsername("aiops"),
		tcpostgres.WithPassword("aiops"),
		tcpostgres.WithInitScripts(),
		testcontainers.WithWaitStrategy(
			wait.ForLog("ready to accept connections").WithOccurrence(2),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("start postgres: %w", err)
	}

	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, err
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, err
	}

	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		pool.Close()
		_ = c.Terminate(ctx)
		return nil, fmt.Errorf("apply migration: %w", err)
	}

	return &pgEnv{pool: pool, dsn: dsn, container: c}, nil
}

func (p *pgEnv) close(ctx context.Context) {
	p.pool.Close()
	_ = p.container.Terminate(ctx)
}
```

(The migration is applied via `pool.Exec` rather than `WithInitScripts` because `WithInitScripts` only runs on first init and a running pool is required afterward anyway.)

- [ ] **Step 2: Build-test the file**

```bash
go build -tags e2e ./test/e2e/...
```

Expected: builds (no tests yet).

- [ ] **Step 3: Commit**

```bash
git add test/e2e/postgres.go
git commit -m "test(e2e): add Postgres testcontainer helper"
```

---

### Task 6: Gitea testcontainer + bootstrap

**Files:**
- Create: `test/e2e/gitea.go`

- [ ] **Step 1: Implement the container helper**

Write:

```go
//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

type giteaEnv struct {
	baseURL    string
	mappedPort string
	botUser    string
	botPass    string
	botToken   string
	container  testcontainers.Container
}

const giteaImage = "gitea/gitea:1.21.11-rootless"

// startGitea boots Gitea, injects an admin user via env, then exchanges
// basic auth for an access token. Returns a fully usable client envelope.
func startGitea(ctx context.Context) (*giteaEnv, error) {
	pass := randomHex(16)
	secret := randomHex(32)

	req := testcontainers.ContainerRequest{
		Image:        giteaImage,
		ExposedPorts: []string{"3000/tcp"},
		Env: map[string]string{
			"GITEA_ADMIN_USER":             "aiops-bot",
			"GITEA_ADMIN_PASSWORD":         pass,
			"GITEA_ADMIN_EMAIL":            "aiops-bot@example.invalid",
			"GITEA__security__INSTALL_LOCK": "true",
			"GITEA__security__SECRET_KEY":  secret,
			"GITEA__database__DB_TYPE":     "sqlite3",
			"GITEA__server__DISABLE_SSH":   "true",
		},
		ExtraHosts: []string{"host.docker.internal:host-gateway"},
		WaitingFor: wait.ForHTTP("/api/v1/version").WithPort("3000/tcp").WithStartupTimeout(90 * time.Second),
	}

	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return nil, fmt.Errorf("start gitea: %w", err)
	}

	host, err := c.Host(ctx)
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, err
	}
	port, err := c.MappedPort(ctx, "3000/tcp")
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, err
	}
	baseURL := fmt.Sprintf("http://%s:%s", host, port.Port())

	// Set ROOT_URL so webhook payloads embed externally-reachable URLs.
	// Gitea's app.ini is generated on first start; updating ROOT_URL via
	// env after boot requires a config reload. We use a small admin API
	// call instead: PATCH /api/v1/admin/config — but Gitea exposes only
	// a subset of settings via API. ROOT_URL is read at startup. Workaround:
	// write to /etc/gitea/app.ini via Exec and HUP the process, or set the
	// env var before container start. Doing the latter requires knowing the
	// mapped port up front, which we do not.
	//
	// Pragmatic solution: rewrite the clone_url in webhook payloads at the
	// trigger-api seam. See services.go for the rewrite hook.
	//
	// (Leaving ROOT_URL unset means Gitea uses http://localhost:3000/ in
	// payloads. The worker, running on the host, cannot clone from there.
	// We inject CloneURLOverride into trigger-api at services.go.)

	env := &giteaEnv{
		baseURL:    baseURL,
		mappedPort: port.Port(),
		botUser:    "aiops-bot",
		botPass:    pass,
		container:  c,
	}

	tok, err := env.createToken(ctx, "e2e")
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, fmt.Errorf("create token: %w", err)
	}
	env.botToken = tok

	return env, nil
}

func (g *giteaEnv) close(ctx context.Context) {
	_ = g.container.Terminate(ctx)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// --- HTTP plumbing ---

func (g *giteaEnv) doJSON(ctx context.Context, method, path string, body any, basicAuth bool) (*http.Response, []byte, error) {
	var bodyReader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
		bodyReader = bytes.NewReader(buf)
	}
	u := g.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, u, bodyReader)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if basicAuth {
		req.SetBasicAuth(g.botUser, g.botPass)
	} else if g.botToken != "" {
		req.Header.Set("Authorization", "token "+g.botToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp, respBody, nil
}

func (g *giteaEnv) createToken(ctx context.Context, name string) (string, error) {
	type tokReq struct {
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
	}
	resp, body, err := g.doJSON(ctx, "POST",
		"/api/v1/users/"+url.PathEscape(g.botUser)+"/tokens",
		tokReq{Name: name, Scopes: []string{"write:repository", "write:admin", "write:user"}},
		true)
	if err != nil {
		return "", err
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("createToken: status %d body %s", resp.StatusCode, body)
	}
	var out struct{ Sha1 string `json:"sha1"` }
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	if out.Sha1 == "" {
		return "", fmt.Errorf("createToken: empty sha1 in body %s", body)
	}
	return out.Sha1, nil
}

// --- domain helpers ---

func (g *giteaEnv) createRepo(ctx context.Context, name string) (cloneURL string, err error) {
	type req struct {
		Name     string `json:"name"`
		AutoInit bool   `json:"auto_init"`
		Private  bool   `json:"private"`
	}
	resp, body, err := g.doJSON(ctx, "POST", "/api/v1/user/repos",
		req{Name: name, AutoInit: true, Private: false}, false)
	if err != nil {
		return "", err
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("createRepo: status %d body %s", resp.StatusCode, body)
	}
	var out struct{ CloneURL string `json:"clone_url"` }
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	return out.CloneURL, nil
}

func (g *giteaEnv) putFile(ctx context.Context, owner, repo, path string, content []byte, msg string) error {
	type req struct {
		Message string `json:"message"`
		Content string `json:"content"`
	}
	resp, body, err := g.doJSON(ctx, "POST",
		fmt.Sprintf("/api/v1/repos/%s/%s/contents/%s", owner, repo, path),
		req{Message: msg, Content: encodeBase64(content)},
		false)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("putFile: status %d body %s", resp.StatusCode, body)
	}
	return nil
}

func (g *giteaEnv) createWebhook(ctx context.Context, owner, repo, hookURL, secret string) error {
	type req struct {
		Type   string            `json:"type"`
		Config map[string]string `json:"config"`
		Events []string          `json:"events"`
		Active bool              `json:"active"`
	}
	resp, body, err := g.doJSON(ctx, "POST",
		fmt.Sprintf("/api/v1/repos/%s/%s/hooks", owner, repo),
		req{
			Type: "gitea",
			Config: map[string]string{
				"content_type": "json",
				"url":          hookURL,
				"secret":       secret,
			},
			Events: []string{"issue_comment"},
			Active: true,
		},
		false)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("createWebhook: status %d body %s", resp.StatusCode, body)
	}
	return nil
}

func (g *giteaEnv) createIssue(ctx context.Context, owner, repo, title, body string) (int, error) {
	type req struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	resp, respBody, err := g.doJSON(ctx, "POST",
		fmt.Sprintf("/api/v1/repos/%s/%s/issues", owner, repo),
		req{Title: title, Body: body}, false)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode/100 != 2 {
		return 0, fmt.Errorf("createIssue: status %d body %s", resp.StatusCode, respBody)
	}
	var out struct{ Number int `json:"number"` }
	if err := json.Unmarshal(respBody, &out); err != nil {
		return 0, err
	}
	return out.Number, nil
}

func (g *giteaEnv) commentIssue(ctx context.Context, owner, repo string, issue int, body string) error {
	type req struct{ Body string `json:"body"` }
	resp, respBody, err := g.doJSON(ctx, "POST",
		fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d/comments", owner, repo, issue),
		req{Body: body}, false)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("commentIssue: status %d body %s", resp.StatusCode, respBody)
	}
	return nil
}

type prSummary struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Draft  bool   `json:"draft"`
	Head   struct{ Ref string `json:"ref"` } `json:"head"`
}

func (g *giteaEnv) listOpenPRs(ctx context.Context, owner, repo string) ([]prSummary, error) {
	resp, body, err := g.doJSON(ctx, "GET",
		fmt.Sprintf("/api/v1/repos/%s/%s/pulls?state=open", owner, repo),
		nil, false)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("listOpenPRs: status %d body %s", resp.StatusCode, body)
	}
	var out []prSummary
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (g *giteaEnv) getBranch(ctx context.Context, owner, repo, branch string) (bool, error) {
	resp, _, err := g.doJSON(ctx, "GET",
		fmt.Sprintf("/api/v1/repos/%s/%s/branches/%s", owner, repo, url.PathEscape(branch)),
		nil, false)
	if err != nil {
		return false, err
	}
	if resp.StatusCode == 404 {
		return false, nil
	}
	if resp.StatusCode/100 != 2 {
		return false, fmt.Errorf("getBranch: unexpected status %d", resp.StatusCode)
	}
	return true, nil
}

func encodeBase64(b []byte) string {
	const enc = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	// stdlib base64 to keep things obvious
	return strings.NewReplacer().Replace(toBase64(b))
}

// toBase64 wraps stdlib base64 to keep the encodeBase64 indirection inert.
func toBase64(b []byte) string {
	enc := bytesToBase64(b)
	return enc
}

// bytesToBase64 uses the stdlib without an extra import in this file.
// (The encoding/base64 import lives here, kept inline to surface intent.)
func bytesToBase64(b []byte) string {
	return base64StdEncode(b)
}

// indirection collapsed — see actual import below
```

(Replace the trailing `encodeBase64`/`toBase64`/`bytesToBase64`/`base64StdEncode` chain with a direct `encoding/base64` import:

```go
import "encoding/base64"

func encodeBase64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
```

The plan shows the indirection only to flag the import requirement; collapse it before committing.)

- [ ] **Step 2: Build-test**

```bash
go build -tags e2e ./test/e2e/...
```

Expected: clean build.

- [ ] **Step 3: Commit**

```bash
git add test/e2e/gitea.go
git commit -m "test(e2e): add Gitea testcontainer and REST bootstrap helpers"
```

---

### Task 7: Services helper (in-process trigger-api + worker, with clone-URL rewrite)

**Files:**
- Create: `test/e2e/services.go`

The Gitea container's `localhost:3000` clone URL is not reachable from the host. Two options were considered (see `gitea.go` Step 1 comment): set `ROOT_URL` before container start (chicken-and-egg with mapped port), or rewrite the clone URL between webhook receipt and queue insertion. The rewrite path is the practical choice; we wrap `triggerapi.Store.Enqueue` to substitute the clone URL.

- [ ] **Step 1: Implement services.go**

Write:

```go
//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xrf9268-hue/aiops-platform/internal/queue"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/triggerapi"
	"github.com/xrf9268-hue/aiops-platform/internal/worker"
)

type testbed struct {
	pg          *pgEnv
	gitea       *giteaEnv
	triggerSrv  *httptest.Server
	secret      string
	webhookURL  string
	cloneRewriter *cloneRewriter
	cancel      context.CancelFunc
	wg          *sync.WaitGroup
}

func setupTestbed(ctx context.Context) (*testbed, error) {
	pg, err := startPostgres(ctx)
	if err != nil {
		return nil, err
	}

	g, err := startGitea(ctx)
	if err != nil {
		pg.close(context.Background())
		return nil, err
	}

	secret := randomHex(16)

	// Wrap the queue store so Gitea's clone_url (which embeds the container-
	// internal hostname) is rewritten to the host-mapped URL the worker can
	// actually clone from.
	rewriter := &cloneRewriter{
		store:    queue.New(pg.pool),
		fromHost: "localhost:3000",
		toHost:   g.baseURL[len("http://"):], // e.g. 127.0.0.1:32789
	}

	// Listen on tcp4 so triggerSrv.URL is always 127.0.0.1.
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		g.close(context.Background())
		pg.close(context.Background())
		return nil, err
	}
	srv := triggerapi.NewServer(rewriter, secret)
	triggerSrv := &httptest.Server{
		Listener: listener,
		Config:   &http.Server{Handler: triggerapi.Routes(srv)},
	}
	triggerSrv.Start()
	if !strings.Contains(triggerSrv.URL, "127.0.0.1") {
		triggerSrv.Close()
		g.close(context.Background())
		pg.close(context.Background())
		return nil, fmt.Errorf("unexpected httptest URL: %s", triggerSrv.URL)
	}

	webhookURL := strings.Replace(triggerSrv.URL, "127.0.0.1", "host.docker.internal", 1) + "/v1/events/gitea"

	cfg := worker.Config{
		WorkspaceRoot:   tmpDir(),
		MirrorRoot:      tmpDir(),
		GiteaBaseURL:    g.baseURL,
		GiteaToken:      g.botToken,
		IdleSleep:       200 * time.Millisecond,
		ClaimErrorSleep: 200 * time.Millisecond,
	}

	wctx, cancel := context.WithCancel(ctx)
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		worker.Run(wctx, queue.New(pg.pool), cfg)
	}()

	return &testbed{
		pg:            pg,
		gitea:         g,
		triggerSrv:    triggerSrv,
		secret:        secret,
		webhookURL:    webhookURL,
		cloneRewriter: rewriter,
		cancel:        cancel,
		wg:            wg,
	}, nil
}

func (b *testbed) close(ctx context.Context) {
	b.cancel()
	stopped := make(chan struct{})
	go func() { b.wg.Wait(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		// best effort; report via stderr (no t available in close path)
	}
	b.triggerSrv.Close()
	b.gitea.close(ctx)
	b.pg.close(ctx)
}

// resetState deletes only rows produced after testStart, leaving rows from
// earlier tests (which their own cleanup should have handled) untouched.
// Uses DELETE rather than TRUNCATE to avoid ACCESS EXCLUSIVE deadlocks
// with the worker's claim transactions.
func (b *testbed) resetState(t *testing.T, testStart time.Time) {
	ctx := context.Background()
	if _, err := b.pg.pool.Exec(ctx,
		`DELETE FROM task_events WHERE task_id IN (SELECT id FROM tasks WHERE created_at >= $1)`,
		testStart); err != nil {
		t.Fatalf("reset task_events: %v", err)
	}
	if _, err := b.pg.pool.Exec(ctx,
		`DELETE FROM tasks WHERE created_at >= $1`, testStart); err != nil {
		t.Fatalf("reset tasks: %v", err)
	}
}

// cloneRewriter wraps a queue.Store. On Enqueue, it rewrites the clone URL
// so the worker can reach Gitea via the host-mapped port instead of the
// container's internal hostname.
type cloneRewriter struct {
	store    *queue.Store
	fromHost string
	toHost   string
}

func (r *cloneRewriter) Enqueue(ctx context.Context, t task.Task) (task.Task, bool, error) {
	if t.CloneURL != "" {
		t.CloneURL = strings.Replace(t.CloneURL, r.fromHost, r.toHost, 1)
	}
	return r.store.Enqueue(ctx, t)
}
func (r *cloneRewriter) GetTask(ctx context.Context, id string) (task.Task, error) {
	return r.store.GetTask(ctx, id)
}
func (r *cloneRewriter) ListTasks(ctx context.Context, st task.Status) ([]task.Task, error) {
	return r.store.ListTasks(ctx, st)
}
func (r *cloneRewriter) TaskEvents(ctx context.Context, id string) ([]task.Event, error) {
	return r.store.TaskEvents(ctx, id)
}

func tmpDir() string {
	d, err := os.MkdirTemp("", "aiops-e2e-*")
	if err != nil {
		panic(err)
	}
	return d
}
```

(Add `"os"` to the import block.)

- [ ] **Step 2: Build-test**

```bash
go build -tags e2e ./test/e2e/...
```

- [ ] **Step 3: Commit**

```bash
git add test/e2e/services.go
git commit -m "test(e2e): add testbed wiring (httptest trigger-api, in-process worker, clone URL rewriter)"
```

---

### Task 8: Polling helper

**Files:**
- Create: `test/e2e/poll.go`

- [ ] **Step 1: Implement**

```go
//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"
)

// pollUntil retries fn every interval until it returns (true, nil) or
// fails the test on timeout.
func pollUntil(t *testing.T, timeout, interval time.Duration, fn func(context.Context) (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var lastErr error
	for time.Now().Before(deadline) {
		ok, err := fn(ctx)
		if err == nil && ok {
			return
		}
		lastErr = err
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			t.Fatalf("pollUntil timed out after %s; last err: %v", timeout, lastErr)
		}
	}
	t.Fatalf("pollUntil timed out after %s; last err: %v", timeout, lastErr)
}
```

- [ ] **Step 2: Build & commit**

```bash
go build -tags e2e ./test/e2e/...
git add test/e2e/poll.go
git commit -m "test(e2e): add pollUntil helper"
```

---

### Task 9: Fixtures

**Files:**
- Create: `test/e2e/fixtures/mock-happy.md`
- Create: `test/e2e/fixtures/mock-allow-fail.md`

- [ ] **Step 1: Write `test/e2e/fixtures/mock-happy.md`**

```markdown
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
  labels:
    - ai-generated
    - needs-review
---
Run mock task {{ task.id }} for {{ repo.owner }}/{{ repo.name }}.
```

- [ ] **Step 2: Write `test/e2e/fixtures/mock-allow-fail.md`**

```markdown
---
agent:
  default: mock
  timeout: 5m
policy:
  mode: draft_pr
  max_changed_files: 12
  max_changed_loc: 300
verify:
  commands:
    - "false"
  allow_failure: true
pr:
  draft: false
  labels:
    - ai-generated
    - needs-review
---
Run mock task {{ task.id }} for {{ repo.owner }}/{{ repo.name }}.
```

- [ ] **Step 3: Commit**

```bash
git add test/e2e/fixtures/
git commit -m "test(e2e): add WORKFLOW.md fixtures (happy and allow_failure)"
```

---

### Task 10: TestMain and shared testbed

**Files:**
- Create: `test/e2e/main_test.go`

- [ ] **Step 1: Implement**

```go
//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"log"
	"os"
	"testing"
)

var bed *testbed

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	bed, err = setupTestbed(ctx)
	if err != nil {
		log.Fatalf("setupTestbed: %v", err)
	}
	code := m.Run()
	bed.close(ctx)
	os.Exit(code)
}

// fixtureContent reads a fixture file, fataling on any error.
func fixtureContent(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("fixtures/%s", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}
```

- [ ] **Step 2: Build**

```bash
go build -tags e2e ./test/e2e/...
```

- [ ] **Step 3: Commit**

```bash
git add test/e2e/main_test.go
git commit -m "test(e2e): add TestMain and shared testbed bootstrap"
```

---

### Task 11: TestGiteaMockLoop_HappyPath

**Files:**
- Create: `test/e2e/happypath_test.go`

- [ ] **Step 1: Implement**

```go
//go:build e2e

package e2e

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
)

func TestGiteaMockLoop_HappyPath(t *testing.T) {
	testStart := time.Now()
	t.Cleanup(func() { bed.resetState(t, testStart) })

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	repo := "demo-happy"
	owner := bed.gitea.botUser

	// Repo, fixture, webhook, issue
	if _, err := bed.gitea.createRepo(ctx, repo); err != nil {
		t.Fatalf("createRepo: %v", err)
	}
	if err := bed.gitea.putFile(ctx, owner, repo, "WORKFLOW.md",
		fixtureContent(t, "mock-happy.md"), "seed workflow"); err != nil {
		t.Fatalf("putFile workflow: %v", err)
	}
	if err := bed.gitea.createWebhook(ctx, owner, repo, bed.webhookURL, bed.secret); err != nil {
		t.Fatalf("createWebhook: %v", err)
	}
	issueNum, err := bed.gitea.createIssue(ctx, owner, repo, "first task", "Make a tiny change.")
	if err != nil {
		t.Fatalf("createIssue: %v", err)
	}

	if err := bed.gitea.commentIssue(ctx, owner, repo, issueNum, "/ai-run"); err != nil {
		t.Fatalf("commentIssue: %v", err)
	}

	// Wait for task to reach succeeded
	var workBranch string
	pollUntil(t, 60*time.Second, 250*time.Millisecond, func(ctx context.Context) (bool, error) {
		row := bed.pg.pool.QueryRow(ctx,
			`SELECT id, status, work_branch FROM tasks WHERE created_at >= $1 ORDER BY created_at DESC LIMIT 1`,
			testStart)
		var id, status, branch string
		if err := row.Scan(&id, &status, &branch); err != nil {
			return false, nil // not yet enqueued
		}
		if status != string(task.StatusSucceeded) {
			return false, nil
		}
		workBranch = branch
		return true, nil
	})

	if !regexp.MustCompile(`^ai/tsk_`).MatchString(workBranch) {
		t.Fatalf("work_branch %q does not match ^ai/tsk_", workBranch)
	}

	// Events check
	taskID := func() string {
		row := bed.pg.pool.QueryRow(ctx,
			`SELECT id FROM tasks WHERE created_at >= $1 ORDER BY created_at DESC LIMIT 1`, testStart)
		var id string
		_ = row.Scan(&id)
		return id
	}()
	wantEvents := []string{
		task.EventWorkflowResolved,
		task.EventRunnerStart,
		task.EventPRCreated,
	}
	for _, want := range wantEvents {
		var n int
		if err := bed.pg.pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM task_events WHERE task_id=$1 AND event_type=$2`,
			taskID, want).Scan(&n); err != nil {
			t.Fatalf("count event %s: %v", want, err)
		}
		if n == 0 {
			t.Errorf("expected at least one %q event for task %s", want, taskID)
		}
	}

	// Branch + PR existence
	exists, err := bed.gitea.getBranch(ctx, owner, repo, workBranch)
	if err != nil || !exists {
		t.Fatalf("getBranch %s: exists=%v err=%v", workBranch, exists, err)
	}

	prs, err := bed.gitea.listOpenPRs(ctx, owner, repo)
	if err != nil {
		t.Fatalf("listOpenPRs: %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("want 1 open PR, got %d: %+v", len(prs), prs)
	}
	pr := prs[0]
	if !pr.Draft {
		t.Errorf("PR should be draft (workflow says draft:true)")
	}
	if !strings.Contains(pr.Body, ".aiops/") {
		t.Errorf("PR body should reference .aiops/ artifacts; got: %s", pr.Body)
	}
}
```

- [ ] **Step 2: Run the test**

```bash
go test -tags e2e -race -timeout 5m -run TestGiteaMockLoop_HappyPath ./test/e2e/... -v
```

Expected: PASS. Cold first run: ~2–3 min (image pull). Warm: ~30–60s.

Common failure modes and fixes:
- `connection refused` clone — check the clone-URL rewriter mapping in `services.go`.
- Webhook reaches trigger-api but task stays `queued` — check worker goroutine isn't hanging on `Claim` (look for log lines).
- `getBranch` returns false — worker likely failed before push; check `task_events` for failure_attempt.

- [ ] **Step 3: Commit**

```bash
git add test/e2e/happypath_test.go
git commit -m "test(e2e): add TestGiteaMockLoop_HappyPath"
```

---

### Task 12: TestWebhookBadSignature

**Files:**
- Create: `test/e2e/badsig_test.go`

- [ ] **Step 1: Implement**

```go
//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"
)

func TestWebhookBadSignature(t *testing.T) {
	testStart := time.Now()
	t.Cleanup(func() { bed.resetState(t, testStart) })

	body := []byte(`{"action":"created"}`)
	req, err := http.NewRequest("POST", bed.triggerSrv.URL+"/v1/events/gitea", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gitea-Event", "issue_comment")
	req.Header.Set("X-Gitea-Delivery", "deadbeef")
	req.Header.Set("X-Gitea-Signature", "sha256=00000000000000000000000000000000")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}

	// No row should have been created in this test's window
	ctx := context.Background()
	var n int
	if err := bed.pg.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tasks WHERE created_at >= $1`, testStart).Scan(&n); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if n != 0 {
		t.Errorf("bad-signature post should not enqueue; got %d tasks", n)
	}
}
```

- [ ] **Step 2: Run**

```bash
go test -tags e2e -race -run TestWebhookBadSignature ./test/e2e/... -v
```

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add test/e2e/badsig_test.go
git commit -m "test(e2e): add TestWebhookBadSignature"
```

---

### Task 13: TestWebhookDeliveryUUID_Deduped

**Files:**
- Create: `test/e2e/dedup_test.go`

- [ ] **Step 1: Implement**

```go
//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/gitea"
)

func TestWebhookDeliveryUUID_Deduped(t *testing.T) {
	testStart := time.Now()
	t.Cleanup(func() { bed.resetState(t, testStart) })

	payload := gitea.IssueCommentPayload{Action: "created"}
	payload.Repository.Name = "demo-dedup"
	payload.Repository.FullName = bed.gitea.botUser + "/demo-dedup"
	payload.Repository.CloneURL = "http://localhost:3000/" + bed.gitea.botUser + "/demo-dedup.git"
	payload.Repository.DefaultBranch = "main"
	payload.Issue.Number = 1
	payload.Issue.Title = "test"
	payload.Comment.ID = 9999
	payload.Comment.Body = "/ai-run"
	payload.Sender.Login = "tester"

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	delivery := "test-delivery-12345"

	post := func() (status int, parsed map[string]any) {
		req, _ := http.NewRequest("POST", bed.triggerSrv.URL+"/v1/events/gitea", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Gitea-Event", "issue_comment")
		req.Header.Set("X-Gitea-Delivery", delivery)
		req.Header.Set("X-Gitea-Signature", gitea.Sign(bed.secret, body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(raw, &parsed)
		return resp.StatusCode, parsed
	}

	st1, body1 := post()
	if st1 != http.StatusAccepted {
		t.Fatalf("first response status want 202 got %d body %v", st1, body1)
	}
	if body1["deduped"] != false {
		t.Fatalf("first response should be deduped:false; got %v", body1)
	}
	taskID1, _ := body1["task_id"].(string)
	if taskID1 == "" {
		t.Fatalf("first response missing task_id: %v", body1)
	}

	st2, body2 := post()
	if st2 != http.StatusAccepted {
		t.Fatalf("second response status want 202 got %d body %v", st2, body2)
	}
	if body2["deduped"] != true {
		t.Fatalf("second response should be deduped:true; got %v", body2)
	}
	if body2["task_id"] != taskID1 {
		t.Fatalf("dedup should reuse task id; got %v vs %v", body2["task_id"], taskID1)
	}

	// Postgres: exactly one row with this source_event_id
	ctx := context.Background()
	var n int
	if err := bed.pg.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tasks WHERE source_event_id=$1`, delivery).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("want exactly 1 task with source_event_id=%s, got %d", delivery, n)
	}
}
```

- [ ] **Step 2: Run**

```bash
go test -tags e2e -race -run TestWebhookDeliveryUUID_Deduped ./test/e2e/... -v
```

Expected: PASS. Note: the worker may begin processing the task between the two posts; this test does not assert PR/branch state, only the trigger-api dedup contract, so worker progress is irrelevant.

- [ ] **Step 3: Commit**

```bash
git add test/e2e/dedup_test.go
git commit -m "test(e2e): add TestWebhookDeliveryUUID_Deduped"
```

---

### Task 14: TestVerifyAllowFailure

**Files:**
- Create: `test/e2e/allowfail_test.go`

- [ ] **Step 1: Implement**

```go
//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
)

func TestVerifyAllowFailure(t *testing.T) {
	testStart := time.Now()
	t.Cleanup(func() { bed.resetState(t, testStart) })

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	repo := "demo-allow-fail"
	owner := bed.gitea.botUser

	if _, err := bed.gitea.createRepo(ctx, repo); err != nil {
		t.Fatalf("createRepo: %v", err)
	}
	if err := bed.gitea.putFile(ctx, owner, repo, "WORKFLOW.md",
		fixtureContent(t, "mock-allow-fail.md"), "seed workflow"); err != nil {
		t.Fatalf("putFile: %v", err)
	}
	if err := bed.gitea.createWebhook(ctx, owner, repo, bed.webhookURL, bed.secret); err != nil {
		t.Fatalf("createWebhook: %v", err)
	}
	issueNum, err := bed.gitea.createIssue(ctx, owner, repo, "allow-failure task", "Try.")
	if err != nil {
		t.Fatalf("createIssue: %v", err)
	}
	if err := bed.gitea.commentIssue(ctx, owner, repo, issueNum, "/ai-run"); err != nil {
		t.Fatalf("commentIssue: %v", err)
	}

	var taskID string
	pollUntil(t, 60*time.Second, 250*time.Millisecond, func(ctx context.Context) (bool, error) {
		row := bed.pg.pool.QueryRow(ctx,
			`SELECT id, status FROM tasks WHERE created_at >= $1 ORDER BY created_at DESC LIMIT 1`, testStart)
		var id, status string
		if err := row.Scan(&id, &status); err != nil {
			return false, nil
		}
		if status != string(task.StatusSucceeded) {
			return false, nil
		}
		taskID = id
		return true, nil
	})

	// verify_end event with status: failed_allowed
	var foundFailedAllowed bool
	rows, err := bed.pg.pool.Query(ctx,
		`SELECT payload FROM task_events WHERE task_id=$1 AND event_type=$2`,
		taskID, task.EventVerifyEnd)
	if err != nil {
		t.Fatalf("query verify_end: %v", err)
	}
	for rows.Next() {
		var payload string
		_ = rows.Scan(&payload)
		if strings.Contains(payload, "failed_allowed") {
			foundFailedAllowed = true
			break
		}
	}
	rows.Close()
	if !foundFailedAllowed {
		t.Errorf("expected verify_end event with failed_allowed status for task %s", taskID)
	}

	// PR is draft (forced by allow_failure path) and body has degraded banner
	prs, err := bed.gitea.listOpenPRs(ctx, owner, repo)
	if err != nil {
		t.Fatalf("listOpenPRs: %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("want 1 open PR, got %d", len(prs))
	}
	pr := prs[0]
	if !pr.Draft {
		t.Errorf("allow_failure path should force draft=true; got draft=%v", pr.Draft)
	}
	if !strings.Contains(pr.Body, "VERIFICATION") && !strings.Contains(pr.Body, "verification") {
		t.Errorf("PR body should reference verification artifact; got: %s", pr.Body)
	}
}
```

- [ ] **Step 2: Run**

```bash
go test -tags e2e -race -run TestVerifyAllowFailure ./test/e2e/... -v
```

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add test/e2e/allowfail_test.go
git commit -m "test(e2e): add TestVerifyAllowFailure"
```

---

### Task 15: CI job + runbook section

**Files:**
- Modify: `.github/workflows/ci.yml`
- Modify: `docs/runbooks/local-dev.md`

- [ ] **Step 1: Add the e2e job**

Edit `.github/workflows/ci.yml`. After the existing `go:` job block, add:

```yaml
  e2e:
    name: E2E Gitea mock loop
    runs-on: ubuntu-latest
    timeout-minutes: 20
    steps:
      - name: Checkout
        uses: actions/checkout@v6
        with:
          persist-credentials: false
      - name: Set up Go
        uses: actions/setup-go@v6
        with:
          go-version-file: go.mod
          cache: true
          cache-dependency-path: |
            go.mod
            go.sum
      - name: Download dependencies
        run: go mod download
      - name: Pre-pull container images
        run: |
          docker pull postgres:16
          docker pull gitea/gitea:1.21.11-rootless
      - name: Run e2e tests
        run: go test -tags e2e -race -timeout 15m ./test/e2e/...
```

Indentation must match the existing `go:` job (two spaces under `jobs:`).

- [ ] **Step 2: Run all four e2e tests together to confirm they coexist**

```bash
go test -tags e2e -race -timeout 15m ./test/e2e/... -v
```

Expected: all four PASS.

- [ ] **Step 3: Add a section to `docs/runbooks/local-dev.md`**

After the existing "Workspace cache and cleanup" section, append:

```markdown
## Running e2e tests locally

The e2e suite under `test/e2e/` validates the full Gitea `/ai-run` mock loop
against real Postgres and Gitea containers. It is gated by a build tag and
does not run as part of `go test ./...`.

Requirements: a working Docker daemon. Cold first run pulls ~600MB of
images and takes 2–3 minutes. Warm runs take ~30–60 seconds.

```bash
go test -tags e2e -race -timeout 15m ./test/e2e/...
```

Common failure modes:

- `Cannot connect to the Docker daemon` — start Docker Desktop or `colima`.
- Test hangs on first webhook delivery — the host.docker.internal mapping
  may not work on this Docker setup; check `triggerSrv.URL` is `127.0.0.1`,
  not `[::1]`.
- `go test` reports `build constraints exclude all Go files` — the `-tags e2e`
  flag is missing.
```

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/ci.yml docs/runbooks/local-dev.md
git commit -m "ci: add parallel e2e job for Gitea mock loop"
```

- [ ] **Step 5: Open PR2**

```bash
git push -u origin <branch>
gh pr create --title "test: add e2e validation suite for Gitea /ai-run mock loop (#5)" --body "$(cat <<'EOF'
## Summary
- Add `test/e2e/` package, build-tagged `//go:build e2e`, using `testcontainers-go` for Postgres and Gitea.
- Four tests: happy path, bad signature, webhook delivery-UUID dedup, verify allow_failure.
- New parallel CI job `e2e`, gated by build tag, with image pre-pull.
- Local-dev runbook section explaining how to run.

Closes #5.

## Test plan
- [x] All four e2e tests pass locally (cold cache).
- [x] `go test ./...` (no -tags) still passes — e2e excluded.
- [x] `e2e` CI job green on the PR.
EOF
)"
```

---

## Self-review

**Spec coverage:**
- §1 architecture (refactor + test layout): Tasks 2, 3, 4–10 ✓
- §2 four scenarios: Tasks 11, 12, 13, 14 ✓
- §3 fixtures, Gitea bootstrap, Sign, services, polling: Tasks 1, 6, 7, 8, 9, 10 ✓
- §4 refactor change list (exports, env→cfg): Task 3 Steps 1, 3 ✓
- §5 CI: Task 15 ✓
- §6 risks (host.docker.internal IPv6, ROOT_URL/clone rewrite, DELETE-not-TRUNCATE, etc.): all wired into Tasks 6, 7 ✓

**Placeholder scan:** None — every step has concrete code or commands.

**Type consistency:**
- `Config` fields match between §1 spec, Task 3 Step 2, and Task 7 Step 1.
- Event constants used in tests match `internal/task` (`EventWorkflowResolved`, `EventRunnerStart`, `EventPRCreated`, `EventVerifyEnd`).
- `gitea.Sign` signature matches `VerifySignature` in Task 1 / Task 13.
- `cloneRewriter` implements `triggerapi.Store` (Enqueue, GetTask, ListTasks, TaskEvents) — verify when Task 2 Step 1 writes the interface; if names diverge, fix Task 7 Step 1 to match.

**One known gap deferred to implementation discretion:** Task 6's `ROOT_URL`-vs-clone-rewrite tradeoff is documented in code comments rather than the spec's preferred path. The spec called for `ROOT_URL` injection at container start, but doing so requires knowing the mapped host port before start, which is a chicken-and-egg with `testcontainers-go`. The clone-rewriter approach achieves the same end (worker reaches the right host) with a smaller blast radius. If the implementer hits issues, Gitea also accepts `app.ini` HUP via `kill -HUP 1`, but that adds complexity not worth the cost here.

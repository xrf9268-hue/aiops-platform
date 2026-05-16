package worker

import (
	"context"
	"io"
	"log"
	"os"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
)

// runStore is the subset of queue.Store that Run actually exercises:
// claim/complete on the loop, AddEvent* via the EventEmitter contract, and
// Fail/FailTimeout via the failingStore contract. Defined as an interface
// so the worker loop's failure-handling logic can be tested against a fake
// without standing up Postgres. *queue.Store satisfies it implicitly.
type runStore interface {
	Claim(ctx context.Context) (*task.Task, error)
	Complete(ctx context.Context, id string) error
	EventEmitter
	failingStore
}

// Config holds the runtime parameters for the worker process. Fields that were
// previously read via os.Getenv inside the request path are now threaded
// through Config so callers (including in-process e2e tests) can inject them
// without environment mutation.
//
// Per SPEC §1, push, PR creation, and tracker state writes are the agent's
// responsibility. The worker no longer holds Gitea tokens or tracker
// transitioner factories — those are provided to the agent via dynamic
// tools (pending #64).
type Config struct {
	DSN             string
	WorkspaceRoot   string
	MirrorRoot      string
	IdleSleep       time.Duration
	ClaimErrorSleep time.Duration
}

// LoadConfigFromEnv reads the worker configuration from the environment using
// the same defaults the original cmd/worker/main.go used.
func LoadConfigFromEnv() Config {
	return Config{
		DSN:             env("DATABASE_URL", "postgres://aiops:aiops@localhost:5432/aiops?sslmode=disable"),
		WorkspaceRoot:   env("WORKSPACE_ROOT", "/tmp/aiops-workspaces"),
		MirrorRoot:      os.Getenv("AIOPS_MIRROR_ROOT"),
		IdleSleep:       3 * time.Second,
		ClaimErrorSleep: 5 * time.Second,
	}
}

// PrintConfig dispatches the `worker --print-config <workdir>` subcommand.
func PrintConfig(workdir string, stdout, stderr io.Writer) int {
	return printConfig(workdir, stdout, stderr)
}

// Run is the worker's main loop. It claims tasks from the store and executes
// them until ctx is canceled.
func Run(ctx context.Context, store runStore, cfg Config) {
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
		if rterr := runTask(ctx, store, *t, cfg); rterr != nil {
			log.Printf("task %s failed: %v", t.ID, rterr.Err)
			cleanupCtx, cancel := terminalUpdateContext(ctx)
			handleTaskFailure(cleanupCtx, store, *t, rterr.Cfg, rterr.Err)
			cancel()
			if ctx.Err() != nil {
				return
			}
			continue
		}
		cleanupCtx, cancel := terminalUpdateContext(ctx)
		_ = store.Complete(cleanupCtx, t.ID)
		cancel()
		if ctx.Err() != nil {
			return
		}
	}
}

// terminalUpdateContext returns a context derived from ctx that does not
// inherit cancellation. Used so that SIGTERM/SIGINT-driven shutdown does not
// leave a task wedged in `running` because the cancel-propagated UPDATE
// against tasks/task_events was rejected with `context canceled`. Capped at
// 5s so a genuinely broken DB cannot block shutdown indefinitely.
func terminalUpdateContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
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

package worker

import (
	"context"
	"io"
	"log"
	"os"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/queue"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// Config holds the runtime parameters for the worker process. Fields that were
// previously read via os.Getenv inside the request path are now threaded
// through Config so callers (including in-process e2e tests) can inject them
// without environment mutation.
type Config struct {
	DSN             string
	WorkspaceRoot   string
	MirrorRoot      string
	GiteaBaseURL    string
	GiteaToken      string
	IdleSleep       time.Duration
	ClaimErrorSleep time.Duration
	// NewTransitioner builds the per-task tracker.Transitioner used by
	// the Linear status-transition hooks (OnClaim / OnPRCreated /
	// OnFailure). Resolved per-task because each repo carries its own
	// tracker credentials in WORKFLOW.md, not from worker env. Returning
	// nil disables the hooks (e.g. for non-Linear trackers, empty
	// API keys, or tests that do not exercise this path).
	NewTransitioner func(workflow.TrackerConfig) Transitioner
}

// LoadConfigFromEnv reads the worker configuration from the environment using
// the same defaults the original cmd/worker/main.go used.
func LoadConfigFromEnv() Config {
	return Config{
		DSN:             env("DATABASE_URL", "postgres://aiops:aiops@localhost:5432/aiops?sslmode=disable"),
		WorkspaceRoot:   env("WORKSPACE_ROOT", "/tmp/aiops-workspaces"),
		MirrorRoot:      os.Getenv("AIOPS_MIRROR_ROOT"),
		GiteaBaseURL:    os.Getenv("GITEA_BASE_URL"),
		GiteaToken:      os.Getenv("GITEA_TOKEN"),
		IdleSleep:       3 * time.Second,
		ClaimErrorSleep: 5 * time.Second,
		NewTransitioner: defaultNewTransitioner,
	}
}

// defaultNewTransitioner is the production factory for Transitioner.
// Linear is the only tracker today that exposes mutation APIs we wired
// up; gitea webhooks do not need outbound state writes (the state is
// the issue itself in Gitea), so we return nil for non-Linear kinds and
// for Linear configs without an API key (e.g. while a workflow is being
// authored locally without secrets exported). nil is a documented
// no-op signal, not an error.
func defaultNewTransitioner(tcfg workflow.TrackerConfig) Transitioner {
	if tcfg.Kind != "linear" || tcfg.APIKey == "" {
		return nil
	}
	return tracker.NewLinearClient(tcfg)
}

// PrintConfig dispatches the `worker --print-config <workdir>` subcommand.
func PrintConfig(workdir string, stdout, stderr io.Writer) int {
	return printConfig(workdir, stdout, stderr)
}

// Run is the worker's main loop. It claims tasks from the store and executes
// them until ctx is canceled.
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
		if rterr := runTask(ctx, store, *t, cfg); rterr != nil {
			log.Printf("task %s failed: %v", t.ID, rterr.Err)
			cleanupCtx, cancel := terminalUpdateContext(ctx)
			handleTaskFailure(cleanupCtx, store, *t, rterr.Cfg, rterr.Err)
			// Fire the tracker-side failure transition under the same
			// terminal-update context so a parent cancel (SIGTERM) does
			// not leave the Linear issue stuck in "In Progress" while
			// the task itself has been moved to failed/queued. The hook
			// is a no-op when no transitioner factory is configured or
			// the task did not come from Linear.
			var tr Transitioner
			if cfg.NewTransitioner != nil {
				tr = cfg.NewTransitioner(rterr.Cfg.Tracker)
			}
			OnFailure(cleanupCtx, store, tr, *t, rterr.Cfg, rterr.Err)
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

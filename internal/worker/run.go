package worker

import (
	"context"
	"io"
	"log"
	"os"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/queue"
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
	}
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
			handleTaskFailure(ctx, store, *t, rterr.Cfg, rterr.Err)
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

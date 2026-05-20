package worker

import (
	"io"
	"os"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

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
	WorkspaceRoot   string
	MirrorRoot      string
	IdleSleep       time.Duration
	ClaimErrorSleep time.Duration
	Workflow        *workflow.Workflow
}

// LoadConfigFromEnv reads the worker configuration from the environment using
// the same defaults the original cmd/worker/main.go used.
func LoadConfigFromEnv() Config {
	return Config{
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

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

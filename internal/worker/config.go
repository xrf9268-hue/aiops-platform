package worker

import (
	"io"
	"os"

	"github.com/xrf9268-hue/aiops-platform/internal/runner"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// Config holds the runtime parameters for the worker process.
//
// Per SPEC §1, push, PR creation, and tracker state writes are the agent's
// responsibility. The worker no longer holds Gitea tokens or tracker
// transitioner factories — those are provided to the agent via dynamic
// tools (pending #64).
type Config struct {
	WorkspaceRoot string
	MirrorRoot    string
	Workflow      *workflow.Workflow
	// IssueStateRefresher, when non-nil, is consulted by RunTask to build
	// the runner-level SPEC §16.5 per-turn refresh hook
	// (runner.RunInput.RefreshIssueState). Returning nil for a task opts
	// that run out of the refresh (e.g. mock tasks with no tracker row);
	// the runner then falls back to the agent-driven continue flag.
	IssueStateRefresher IssueStateRefresherFactory
}

// IssueStateRefresherFactory builds a per-task refresher closure that the
// runner invokes between turns. The factory receives the task and the
// workflow config that resolved for it (so the closure can read the
// active_states vocabulary) and returns either a callable or nil when the
// task should keep the legacy continue-driven loop.
type IssueStateRefresherFactory func(t task.Task, cfg workflow.Config) runner.IssueStateRefresher

// LoadConfigFromEnv reads the worker configuration from the environment using
// the same defaults the original cmd/worker/main.go used.
func LoadConfigFromEnv() Config {
	return Config{
		WorkspaceRoot: env("WORKSPACE_ROOT", "/tmp/aiops-workspaces"),
		MirrorRoot:    os.Getenv("AIOPS_MIRROR_ROOT"),
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

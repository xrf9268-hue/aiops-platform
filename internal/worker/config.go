// Package worker claims queued tasks and runs the Symphony-style workflow for
// each: workflow resolution, runner invocation, verification, and draft-PR
// handoff.
package worker

import (
	"io"

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
	// CleanTurnBudget optionally tightens the runner's per-session turn cap for
	// this dispatch and treats cap exhaustion as a clean budget stop. It is
	// run-scoped and must not mutate Workflow.Config.
	CleanTurnBudget int
	// IssueStateRefresher, when non-nil, is consulted by RunTask to build
	// the runner-level SPEC §16.5 per-turn refresh hook
	// (runner.RunInput.RefreshIssueState). Returning nil for a task opts
	// that run out of the refresh (e.g. mock tasks with no tracker row);
	// the runner then falls back to the agent-driven continue flag.
	IssueStateRefresher IssueStateRefresherFactory
	// OperatorTerminalStopLookup, when non-nil, builds a process-local latch
	// reader for post-stop mutation auditing. Unlike IssueStateRefresher, this
	// must not fall back to tracker I/O.
	OperatorTerminalStopLookup OperatorTerminalStopLookupFactory
}

// IssueStateRefresherFactory builds a per-task refresher closure that the
// runner invokes between turns. The factory receives the task and the
// workflow config that resolved for it (so the closure can read the
// active_states vocabulary) and returns either a callable or nil when the
// task should keep the legacy continue-driven loop.
type IssueStateRefresherFactory func(t task.Task, cfg workflow.Config) runner.IssueStateRefresher

// OperatorTerminalStopLookupFactory builds a per-task lookup for the
// orchestrator-owned Operator Terminal Stop latch.
type OperatorTerminalStopLookupFactory func(t task.Task, cfg workflow.Config) runner.OperatorTerminalStopLookup

// LoadConfigFromEnv reads the worker configuration from the environment.
//
// Worker env vars use the AIOPS_ prefix as the single naming convention
// (#368). The legacy unprefixed WORKSPACE_ROOT is still honored as a
// deprecated alias for AIOPS_WORKSPACE_ROOT (with a warning) so existing
// deployments keep working; using the wrong form no longer silently falls back
// to the code default.
//
// WorkspaceRoot intentionally has no literal default here — when the workspace
// root env is unset, Config.WorkspaceRoot stays empty and
// EffectiveWorkspaceRoot falls back to the workflow's `Workspace.Root`
// (the SPEC §6.4 default seeded by workflow.DefaultConfig). The previous
// `/tmp/aiops-workspaces` fallback silently shadowed that SPEC default
// regardless of WORKFLOW.md content; see #319.
func LoadConfigFromEnv() Config {
	return Config{
		WorkspaceRoot: ResolveEnv(workspaceRootEnv, workspaceRootEnvLegacy).Value,
		MirrorRoot:    ResolveEnv(mirrorRootEnv, mirrorRootEnvLegacy).Value,
	}
}

// PrintConfig dispatches the `worker --print-config <workdir>` subcommand.
// portOverride carries the CLI --port flag (nil when not passed) so the
// provenance block can attribute server.port to a `cli` source.
func PrintConfig(workdir string, portOverride *int, stdout, stderr io.Writer) int {
	return printConfig(workdir, portOverride, stdout, stderr)
}

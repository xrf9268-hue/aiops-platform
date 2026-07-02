package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/buildinfo"
	"github.com/xrf9268-hue/aiops-platform/internal/doctor"
	"github.com/xrf9268-hue/aiops-platform/internal/gitea"
	"github.com/xrf9268-hue/aiops-platform/internal/orchestrator"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/worker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// version is the build stamp, set at link time via
// -ldflags "-X main.version=<tag>" (release.yml, install.sh, Dockerfile).
// It stays "devel" for an un-stamped `go build`/`go run`; resolveVersion then
// falls back to the VCS revision the Go toolchain records when the binary was
// built from a source tree (absent in Docker, where .dockerignore drops .git —
// hence the explicit build ARG there).
var version = "devel"

// workerShutdownDrainGrace bounds how long run() waits for in-flight agent
// workers after the poll loop exits on SIGTERM/SIGINT. It matches the
// reconcile WorkerExitTimeout (workflow_runtime.go): workers that ignore
// cancellation longer than this are abandoned to process exit, and a second
// signal always terminates immediately.
const workerShutdownDrainGrace = 30 * time.Second

func resolveVersion() string { return buildinfo.Resolve(version) }

func main() { //nolint:gocognit // baseline (#521)
	if len(os.Args) >= 2 && os.Args[1] == "--version" {
		fmt.Println(resolveVersion())
		os.Exit(0)
	}
	if len(os.Args) >= 2 && os.Args[1] == "--print-config" {
		workdir, portOverride, err := parsePrintConfigArgs(os.Args[2:])
		if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(2)
		}
		os.Exit(worker.PrintConfig(workdir, portOverride, os.Stdout, os.Stderr))
	}
	if len(os.Args) >= 2 && os.Args[1] == "--doctor" {
		opts, err := parseDoctorArgs(os.Args[2:])
		if err != nil {
			if errors.Is(err, flag.ErrHelp) {
				os.Exit(0)
			}
			fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(2)
		}
		os.Exit(doctor.Run(context.Background(), opts))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	if err := normalizeRunError(run(ctx, os.Args[1:]), ctx.Err()); err != nil {
		stop()
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	stop()
}
func parseDoctorArgs(args []string) (doctor.Options, error) {
	fs := flag.NewFlagSet("worker --doctor", flag.ContinueOnError)
	mode := fs.String("mode", "mock", "preflight depth: mock or real")
	deploy := fs.String("deploy", "docker", "deployment target for remediation hints and Docker checks: binary or docker")
	dashboardURL := fs.String("dashboard-url", "", "optional worker dashboard base URL to verify /livez, /readyz, and /api/v1/state")
	goTestDir := fs.String("go-test-dir", "", "repository module root for real-mode targeted go test")
	githubIssue := fs.Int("github-issue", 0, "optional GitHub issue number for agent-environment gh and git push preflight")
	githubRepo := fs.String("github-repo", "", "optional owner/name or clone_url repo for --github-issue when a workflow configures multiple GitHub repos (use clone_url to disambiguate one owner/name routed to multiple clone URLs)")
	if err := fs.Parse(reorderDoctorFlags(args)); err != nil {
		return doctor.Options{}, err
	}
	if *mode != "mock" && *mode != "real" {
		return doctor.Options{}, fmt.Errorf("--mode must be mock or real")
	}
	if *deploy != "binary" && *deploy != "docker" {
		return doctor.Options{}, fmt.Errorf("--deploy must be binary or docker")
	}
	if fs.NArg() > 1 {
		return doctor.Options{}, fmt.Errorf("usage: worker --doctor [--mode=mock|real] [--deploy=binary|docker] [--dashboard-url=http://127.0.0.1:4000] [--go-test-dir=/repo-module] [--github-issue=N] [--github-repo=owner/name] [path-to-WORKFLOW.md]")
	}
	var path string
	if fs.NArg() == 1 {
		path = fs.Arg(0)
	}
	return doctor.Options{WorkflowPath: path, Mode: *mode, Deploy: *deploy, DashboardURL: *dashboardURL, GoTestDir: *goTestDir, GitHubIssue: *githubIssue, GitHubRepo: *githubRepo, Stdout: os.Stdout, Stderr: os.Stderr}, nil
}
func reorderDoctorFlags(args []string) []string {
	flags, positional := make([]string, 0, len(args)), make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			positional = append(positional, append([]string{"--"}, args[i+1:]...)...)
			i = len(args)
		case a == "--mode" || a == "-mode" || a == "--deploy" || a == "-deploy" || a == "--dashboard-url" || a == "-dashboard-url" || a == "--go-test-dir" || a == "-go-test-dir" || a == "--github-issue" || a == "-github-issue" || a == "--github-repo" || a == "-github-repo":
			flags = append(flags, a)
			if i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
		case strings.HasPrefix(a, "-"):
			flags = append(flags, a)
		default:
			positional = append(positional, a)
		}
	}
	return append(flags, positional...)
}
func normalizeRunError(err error, runCtxErr error) error {
	if runCtxErr != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return nil
	}
	if errors.Is(err, flag.ErrHelp) {
		// flag.Parse already wrote usage to stderr when -h/--help was
		// requested; treat that as a clean exit, not a fatal error.
		return nil
	}
	return err
}

// parseRunArgs parses the run-mode CLI args (everything after
// os.Args[0]; the --print-config subcommand is handled separately
// before this is called). It returns the positional workflow path and
// an optional --port override.
//
// SPEC §13.7 declares --port as the canonical override for
// server.port in WORKFLOW.md. The value range is -1 (disable), 0
// (ephemeral), or 1..65535. Note that the workflow loader itself
// rejects 0 in WORKFLOW.md (see internal/workflow/loader.go), so CLI
// is the legitimate path for an ephemeral port — tests and operator
// scratch sessions are the motivating callers.
func parseRunArgs(args []string) (string, *int, error) {
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	// Leave fs.Output() at its default (os.Stderr) so `worker --help`
	// and parse diagnostics reach the user. flag.ErrHelp is propagated
	// to the caller, which treats it as a clean exit.
	portFlag := fs.Int("port", 0, "override server.port from WORKFLOW.md: -1 disables the HTTP server, 0 binds an ephemeral port, 1..65535 binds explicitly. SPEC §13.7.")
	// main() intercepts --print-config / --doctor / --version positionally
	// before this flag set ever sees them, so the default `flag` usage hides
	// them. Spell out every subcommand so `worker --help` is a complete map.
	fs.Usage = func() {
		out := fs.Output()
		_, _ = fmt.Fprint(out, "Usage:\n"+
			"  worker [--port=N] [path-to-WORKFLOW.md]            run the orchestrator loop (default)\n"+
			"  worker --print-config [--port=N] <workdir>        print the resolved config and exit\n"+
			"  worker --doctor [--mode=mock|real] [--deploy=binary|docker] [path]  run preflight checks and exit\n"+
			"  worker --version                                  print the build version and exit\n\n"+
			"Run-mode flags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderForFlagParse(args)); err != nil {
		return "", nil, err
	}
	if fs.NArg() > 1 {
		return "", nil, fmt.Errorf("usage: worker [--port=N] [path-to-WORKFLOW.md]")
	}
	var path string
	if fs.NArg() == 1 {
		path = fs.Arg(0)
	}
	override, err := portOverrideFromFlagSet(fs, portFlag)
	if err != nil {
		return "", nil, err
	}
	return path, override, nil
}

// parsePrintConfigArgs parses the args after the `--print-config`
// subcommand token. It requires exactly one positional (the workdir to
// inspect) and accepts the same `--port` override as run mode, so
// `worker --print-config /repo --port=4001` attributes server.port to a
// `cli` source in the provenance block (#375, SPEC §13.7). The override is
// nil when --port is not passed.
func parsePrintConfigArgs(args []string) (string, *int, error) {
	fs := flag.NewFlagSet("worker --print-config", flag.ContinueOnError)
	portFlag := fs.Int("port", 0, "override server.port for provenance reporting: -1 disables the HTTP server, 0 binds an ephemeral port, 1..65535 binds explicitly. SPEC §13.7.")
	if err := fs.Parse(reorderForFlagParse(args)); err != nil {
		return "", nil, err
	}
	if fs.NArg() != 1 {
		return "", nil, fmt.Errorf("usage: worker --print-config [--port=N] <workdir>")
	}
	override, err := portOverrideFromFlagSet(fs, portFlag)
	if err != nil {
		return "", nil, err
	}
	return fs.Arg(0), override, nil
}

// portOverrideFromFlagSet extracts the --port override from a parsed flag
// set shared by run mode and --print-config, so the two paths can never
// diverge on the accepted range or the set-vs-default detection. It uses
// fs.Visit to distinguish "flag explicitly provided" from "flag at its
// default value": a naked sentinel int has no safe pick, since any
// reserved value would conflict with a real --port=N for that N. Returns
// nil when --port was not passed.
func portOverrideFromFlagSet(fs *flag.FlagSet, portFlag *int) (*int, error) {
	var portSet bool
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "port" {
			portSet = true
		}
	})
	if !portSet {
		return nil, nil
	}
	p := *portFlag
	if p < -1 || p > 65535 {
		return nil, fmt.Errorf("--port %d out of range: pass -1 to disable the HTTP server, 0 for an ephemeral port, or 1..65535", p)
	}
	return &p, nil
}

// reorderForFlagParse pulls flag tokens to the front of args so the
// stdlib flag parser (which stops at the first non-flag argument)
// accepts `worker /path/WORKFLOW.md --port=4001` and `worker
// --port=4001 /path/WORKFLOW.md` interchangeably. Operators are not
// expected to remember Go's flag-vs-positional ordering rule, and
// SPEC §13.7 does not impose one.
//
// The reorder is flag-aware (knows --port takes a value) rather than
// a naive "starts with -" split — that's only because --port can be
// passed as two tokens (`--port 4001`). A `--` token preserves
// everything that follows as positional, matching POSIX convention.
func reorderForFlagParse(args []string) []string {
	flags, positional := make([]string, 0, len(args)), make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			// POSIX end-of-options: everything after is positional,
			// even if it begins with "-". Preserve the "--" itself so
			// the stdlib flag parser still recognizes the boundary
			// when it scans the reordered slice.
			tail := append([]string{"--"}, args[i+1:]...)
			positional = append(positional, tail...)
			i = len(args)
		case a == "--port" || a == "-port":
			flags = append(flags, a)
			if i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
		case strings.HasPrefix(a, "-"):
			flags = append(flags, a)
		default:
			positional = append(positional, a)
		}
	}
	return append(flags, positional...)
}

// serverHostOverrideFromEnv returns the AIOPS_SERVER_HOST override, or nil when
// the variable is unset so the workflow `server.host` (then the loopback
// default) still applies. A set-but-empty value is an explicit override:
// normalizeServerHost maps it to the safe loopback default, so
// `AIOPS_SERVER_HOST=` forces loopback even over a workflow `server.host`.
func serverHostOverrideFromEnv() *string {
	host, ok := os.LookupEnv("AIOPS_SERVER_HOST")
	if !ok {
		return nil
	}
	return &host
}
func loadWorkflowForStartupReconcile() (*workflow.Workflow, error) {
	wf, resolution, err := resolveStartupWorkflow(nil)
	if err != nil {
		return nil, err
	}
	logStartupReconcileWorkflow(resolution, wf)
	return wf, nil
}
func resolveStartupWorkflow(args []string) (*workflow.Workflow, *workflow.Resolution, error) {
	path, err := startupWorkflowPath(args)
	if err != nil {
		return nil, nil, err
	}
	if path == "" {
		cfg := workflow.DefaultConfig()
		return &workflow.Workflow{Config: cfg, PromptTemplate: workflow.DefaultPrompt(), Source: workflow.SourceDefault}, &workflow.Resolution{Source: workflow.SourceDefault}, nil
	}
	wf, err := workflow.Load(path)
	if err != nil {
		return nil, nil, err
	}
	hasFront, err := workflow.HasFrontMatterAt(path)
	if err != nil {
		return nil, nil, err
	}
	source := workflow.SourceFile
	if !hasFront {
		source = workflow.SourcePromptOnly
	}
	return wf, &workflow.Resolution{Source: source, Path: path}, nil
}
func startupWorkflowPath(args []string) (string, error) {
	if len(args) > 1 {
		return "", fmt.Errorf("usage: worker [--port=N] [path-to-WORKFLOW.md]")
	}
	if len(args) == 1 {
		return args[0], nil
	}
	if path := worker.WorkflowPathEnv().Value; path != "" {
		return path, nil
	}
	workdir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	path := filepath.Join(workdir, "WORKFLOW.md")
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return path, nil
}
func logStartupReconcileWorkflow(resolution *workflow.Resolution, wf *workflow.Workflow) {
	if resolution.Source == workflow.SourceDefault {
		log.Printf("startup reconciliation: workflow source=%s tracker.kind=%s", resolution.Source, wf.Config.Tracker.Kind)
		return
	}
	log.Printf("startup reconciliation: workflow source=%s path=%s tracker.kind=%s", resolution.Source, resolution.Path, wf.Config.Tracker.Kind)
}
func validateWorkflowForRuntime(path string, source workflow.Source, cfg workflow.Config) error {
	if cfg.Repo.CloneURL == "" {
		if source == workflow.SourceDefault {
			path = "built-in workflow defaults"
		}
		return fmt.Errorf("%s: repo.clone_url is required for poll-based worker runtime", path)
	}
	return nil
}
func startupReconcileConfigForWorkflow(cfg workflow.Config, trackerClient worker.ReconcileTracker) worker.ReconcileConfig {
	hooks := cfg.WorkspaceHooks()
	return worker.ReconcileConfig{
		WorkspaceRoot:      worker.EffectiveWorkspaceRoot(worker.LoadConfigFromEnv(), cfg),
		ActiveStates:       cfg.Tracker.ActiveStates,
		TerminalStates:     cfg.Tracker.TerminalStates,
		TrackerKind:        cfg.Tracker.Kind,
		Tracker:            trackerClient,
		Emitter:            worker.LogEventEmitter{},
		ReconcileTaskID:    "reconcile-startup",
		WorkflowConfig:     cfg,
		BeforeRemoveHook:   hooks.BeforeRemove,
		HookTimeoutMillis:  hooks.TimeoutMs,
		HookEnvPassthrough: hooks.EnvPassthrough,
	}
}
func run(ctx context.Context, args []string) error { //nolint:gocognit,funlen // baseline (#521)
	workflowPath, portOverride, err := parseRunArgs(args)
	if err != nil {
		return err
	}
	log.Printf("aiops worker starting version=%s", resolveVersion())
	var resolveArgs []string
	if workflowPath != "" {
		resolveArgs = []string{workflowPath}
	}
	cfg := worker.LoadConfigFromEnv()
	wf, resolution, err := resolveStartupWorkflow(resolveArgs)
	if err != nil {
		return err
	}
	logStartupReconcileWorkflow(resolution, wf)
	if err := validateWorkflowForRuntime(wf.Path, wf.Source, wf.Config); err != nil {
		return err
	}
	cfg.Workflow = wf
	readiness := &stateHTTPReadiness{}

	trackerClient, err := trackerClientForWorkflow(wf.Config)
	if err != nil {
		return err
	}

	state := orchestrator.NewOrchestratorState(int64(wf.Config.Polling.IntervalMs), wf.Config.Agent.MaxConcurrentAgents)
	state.MaxContinuationTurns = wf.Config.Agent.MaxContinuationTurns
	state.AgentDefault = wf.Config.Agent.Default
	state.BudgetGuardrails = orchestrator.BudgetGuardrails{
		MaxTokensPerClaim:         wf.Config.Agent.MaxTokensPerClaim,
		MaxRuntimeSecondsPerClaim: wf.Config.Agent.MaxRuntimeSecondsPerClaim,
	}
	runtime, err := orchestrator.NewWorkflowRuntime(orchestrator.WorkflowRuntimeConfig{
		Initial:              wf,
		Path:                 resolution.Path,
		Source:               resolution.Source,
		ReloadInterval:       time.Second,
		Emitter:              worker.LogEventEmitter{},
		EventTaskID:          "workflow-runtime",
		Validate:             validateWorkflowForRuntime,
		ReconciliationConfig: reconciliationConfigForWorkflow,
	})
	if err != nil {
		return err
	}
	dispatcher, err := orchestrator.NewRuntimeDispatcher(runtime, cfg, worker.LogEventEmitter{})
	if err != nil {
		return err
	}
	orch := orchestrator.New(state, orchestrator.Deps{
		Dispatcher: dispatcher,
		Scheduler:  orchestrator.RetryScheduler{MaxBackoff: time.Duration(wf.Config.Agent.MaxRetryBackoffMs) * time.Millisecond},
		// SPEC §18.1 active-transition cleanup: the dispatcher removes the
		// workspace (firing before_remove against the live workflow snapshot)
		// when a running issue moves to a terminal state mid-run.
		WorkspaceCleaner: dispatcher,
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		return err
	}
	// Start the listener before startup reconciliation so /readyz can report
	// 503 while reconciliation is still running; the poll loop below still
	// starts only after ReconcileStartup succeeds.
	go func() {
		if err := runStateHTTPServerLoop(ctx, runtime, orch.Snapshot, stateHTTPServerLoopOptions{Refresh: orch.RequestRefresh, Readiness: readiness.Status, PortOverride: portOverride, HostOverride: serverHostOverrideFromEnv()}); err != nil && ctx.Err() == nil {
			log.Printf("state HTTP server loop exited: %v", err)
		}
	}()
	reconcileCfg := startupReconcileConfigForWorkflow(wf.Config, trackerClient)
	reconcileCfg.WorkspaceRoot = worker.EffectiveWorkspaceRoot(cfg, wf.Config)
	if err := worker.ReconcileStartup(ctx, reconcileCfg); err != nil {
		worker.LogReconcileError(err)
		return err
	}
	readiness.MarkReady()
	// SPEC §16.5 per-turn refresh wires through the dispatcher the actor
	// actually spawns workers with — the same instance handed to Deps.Dispatcher
	// above — so the tracker fan-in the poller rebuilds each tick lands where
	// Spawn reads it and operator-cancel takes effect on the current poll tick.
	poller, err := orchestrator.NewRuntimePollerWithTrackerFactory(func(cfg workflow.Config) (orchestrator.IssueStateLister, error) {
		return trackerClientForWorkflow(cfg)
	}, orch, runtime, dispatcher)
	if err != nil {
		return err
	}
	go func() {
		if err := orchestrator.RunWorkflowReloadLoop(ctx, runtime, orchestrator.WorkflowReloadLoopOptions{}); err != nil && ctx.Err() == nil {
			log.Printf("workflow reload loop exited: %v", err)
		}
	}()
	pollErr := orchestrator.RunPollLoopWithRuntime(ctx, poller, runtime, orchestrator.PollLoopRuntimeOptions{})
	// SIGTERM/SIGINT lands here with in-flight agents still running. Drain
	// them before returning from run(): a bare return exits the process,
	// racing the runners' subprocess kill and skipping result collection
	// (issue #1030). Workers see the canceled root context and exit on
	// their own; the grace period only bounds the wait. A second signal
	// still kills the process immediately (signal.NotifyContext stops
	// relaying once canceled).
	if !orch.WaitForWorkers(workerShutdownDrainGrace) {
		log.Printf("event=shutdown_drain_timeout grace=%s note=%q", workerShutdownDrainGrace, "exiting with agent workers still running")
	}
	return pollErr
}
func reconciliationConfigForWorkflow(cfg workflow.Config) orchestrator.ReconciliationConfig {
	// Single source of truth for the field list lives in
	// orchestrator.ReconciliationConfigFromWorkflow; the worker only re-derives
	// InactiveStates (default candidates minus active/terminal, per
	// inferredInactiveStates). Delegating here keeps fields such as
	// RequiredLabels from being silently dropped in production (#682).
	rc := orchestrator.ReconciliationConfigFromWorkflow(cfg)
	rc.InactiveStates = inferredInactiveStates(cfg.Tracker)
	return rc
}
func inferredInactiveStates(cfg workflow.TrackerConfig) []string {
	candidates := cfg.InactiveStates
	if len(candidates) == 0 {
		candidates = defaultInactiveStateCandidates(cfg.Kind)
	}
	active := stateSet(cfg.ActiveStates)
	terminal := stateSet(cfg.TerminalStates)
	out := make([]string, 0, len(candidates))
	seen := map[string]struct{}{}
	for _, state := range candidates {
		key := normalizeState(state)
		if key == "" {
			continue
		}
		if _, ok := active[key]; ok {
			continue
		}
		if _, ok := terminal[key]; ok {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, state)
	}
	return out
}
func defaultInactiveStateCandidates(kind string) []string {
	switch normalizeState(kind) {
	case "github":
		return nil
	case "gitea":
		return []string{"Human Review", "Merging"}
	default:
		return []string{"Backlog", "Human Review"}
	}
}
func stateSet(states []string) map[string]struct{} {
	out := make(map[string]struct{}, len(states))
	for _, state := range states {
		if key := normalizeState(state); key != "" {
			out[key] = struct{}{}
		}
	}
	return out
}
func normalizeState(state string) string {
	return strings.ToLower(strings.TrimSpace(state))
}

type trackerRuntimeClient interface {
	orchestrator.ActiveIssueLister
	worker.ReconcileTracker
}

func trackerClientForWorkflow(cfg workflow.Config) (trackerRuntimeClient, error) {
	switch cfg.Tracker.Kind {
	case "linear":
		return tracker.NewLinearClient(cfg.Tracker), nil
	case "gitea":
		client := gitea.NewTrackerClient(cfg.Tracker, gitea.BaseURLFromEnv(cfg.Tracker), cfg.Repo.Owner, cfg.Repo.Name)
		client.Logf = log.Printf
		return client, nil
	case "github":
		client := tracker.NewGitHubClientFromEnv(cfg.Tracker, cfg.Repo.Owner, cfg.Repo.Name)
		client.Logf = log.Printf
		return client, nil
	default:
		return nil, tracker.NewError(tracker.CategoryUnsupportedTrackerKind, fmt.Sprintf("unsupported tracker.kind %q", cfg.Tracker.Kind), nil)
	}
}

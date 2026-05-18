package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/gitea"
	"github.com/xrf9268-hue/aiops-platform/internal/orchestrator"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/worker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
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
	if err := normalizeRunError(run(ctx, os.Args[1:]), ctx.Err()); err != nil {
		log.Fatal(err)
	}
}

func normalizeRunError(err error, runCtxErr error) error {
	if runCtxErr != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return nil
	}
	return err
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
		return "", fmt.Errorf("usage: worker [path-to-WORKFLOW.md]")
	}
	if len(args) == 1 {
		return args[0], nil
	}
	if path := os.Getenv("AIOPS_WORKFLOW_PATH"); path != "" {
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
		WorkspaceRoot:     worker.LoadConfigFromEnv().WorkspaceRoot,
		ActiveStates:      cfg.Tracker.ActiveStates,
		TerminalStates:    cfg.Tracker.TerminalStates,
		TrackerKind:       cfg.Tracker.Kind,
		Tracker:           trackerClient,
		Emitter:           worker.LogEventEmitter{},
		ReconcileTaskID:   "reconcile-startup",
		BeforeRemoveHook:  hooks.BeforeRemove,
		HookTimeoutMillis: hooks.TimeoutMs,
	}
}

func run(ctx context.Context, args []string) error {
	cfg := worker.LoadConfigFromEnv()
	wf, resolution, err := resolveStartupWorkflow(args)
	if err != nil {
		return err
	}
	logStartupReconcileWorkflow(resolution, wf)
	if err := validateWorkflowForRuntime(wf.Path, wf.Source, wf.Config); err != nil {
		return err
	}
	cfg.Workflow = wf

	trackerClient, err := trackerClientForWorkflow(wf.Config)
	if err != nil {
		return err
	}
	reconcileCfg := startupReconcileConfigForWorkflow(wf.Config, trackerClient)
	reconcileCfg.WorkspaceRoot = cfg.WorkspaceRoot
	if err := worker.ReconcileStartup(ctx, reconcileCfg); err != nil {
		worker.LogReconcileError(err)
		return err
	}

	state := orchestrator.NewOrchestratorState(int64(wf.Config.Tracker.PollIntervalMs), wf.Config.Agent.MaxConcurrentAgents)
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
		Scheduler:  orchestrator.FixedDelayScheduler{Delay: 60 * time.Second},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		return err
	}
	poller, err := orchestrator.NewRuntimePollerWithTrackerFactory(func(cfg workflow.Config) (orchestrator.IssueStateLister, error) {
		return trackerClientForWorkflow(cfg)
	}, orch, runtime, cfg, worker.LogEventEmitter{})
	if err != nil {
		return err
	}
	go func() {
		if err := orchestrator.RunWorkflowReloadLoop(ctx, runtime, orchestrator.WorkflowReloadLoopOptions{}); err != nil && ctx.Err() == nil {
			log.Printf("workflow reload loop exited: %v", err)
		}
	}()
	return orchestrator.RunPollLoopWithRuntime(ctx, poller, runtime, orchestrator.PollLoopRuntimeOptions{})
}

func reconciliationConfigForWorkflow(cfg workflow.Config) orchestrator.ReconciliationConfig {
	return orchestrator.ReconciliationConfig{
		ActiveStates:      cfg.Tracker.ActiveStates,
		TerminalStates:    cfg.Tracker.TerminalStates,
		InactiveStates:    inferredInactiveStates(cfg.Tracker),
		WorkerExitTimeout: 30 * time.Second,
	}
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
	case "gitea":
		return []string{"Human Review"}
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
		baseURL := cfg.Tracker.ProjectSlug
		if baseURL == "" {
			baseURL = env("GITEA_BASE_URL", "http://localhost:3000")
		}
		return gitea.NewTrackerClient(cfg.Tracker, baseURL, cfg.Repo.Owner, cfg.Repo.Name), nil
	default:
		return nil, fmt.Errorf("unsupported tracker.kind %q", cfg.Tracker.Kind)
	}
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

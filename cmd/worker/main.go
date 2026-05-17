package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/gitea"
	"github.com/xrf9268-hue/aiops-platform/internal/orchestrator"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
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
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func loadWorkflowForStartupReconcile() (*workflow.Workflow, error) {
	if path := os.Getenv("AIOPS_WORKFLOW_PATH"); path != "" {
		wf, err := workflow.Load(path)
		if err != nil {
			return nil, err
		}
		hasFront, err := workflow.HasFrontMatterAt(path)
		if err != nil {
			return nil, err
		}
		source := workflow.SourceFile
		if !hasFront {
			source = workflow.SourcePromptOnly
		}
		logStartupReconcileWorkflow(&workflow.Resolution{Source: source, Path: path}, wf)
		return wf, nil
	}

	workdir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	wf, resolution, err := workflow.Resolve(workdir)
	if err != nil {
		return nil, err
	}
	logStartupReconcileWorkflow(resolution, wf)
	return wf, nil
}

func logStartupReconcileWorkflow(resolution *workflow.Resolution, wf *workflow.Workflow) {
	if resolution.Source == workflow.SourceDefault {
		log.Printf("startup reconciliation: workflow source=%s tracker.kind=%s", resolution.Source, wf.Config.Tracker.Kind)
		return
	}
	log.Printf("startup reconciliation: workflow source=%s path=%s tracker.kind=%s", resolution.Source, resolution.Path, wf.Config.Tracker.Kind)
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := worker.LoadConfigFromEnv()
	wf, err := loadWorkflowForStartupReconcile()
	if err != nil {
		return err
	}

	trackerClient, err := trackerClientForWorkflow(wf.Config)
	if err != nil {
		return err
	}
	if err := worker.ReconcileStartup(ctx, worker.ReconcileConfig{
		WorkspaceRoot:   cfg.WorkspaceRoot,
		ActiveStates:    wf.Config.Tracker.ActiveStates,
		TerminalStates:  wf.Config.Tracker.TerminalStates,
		Tracker:         trackerClient,
		Emitter:         worker.LogEventEmitter{},
		ReconcileTaskID: "reconcile-startup",
	}); err != nil {
		worker.LogReconcileError(err)
		return err
	}

	pollInterval := time.Duration(wf.Config.Tracker.PollIntervalMs) * time.Millisecond
	state := orchestrator.NewOrchestratorState(int64(wf.Config.Tracker.PollIntervalMs), wf.Config.Agent.MaxConcurrentAgents)
	dispatcher := orchestrator.WorkerTaskDispatcher{
		BuildTask: func(issue tracker.Issue) (task.Task, error) {
			return orchestrator.TaskFromIssue(issue, wf.Config)
		},
		Config:  cfg,
		Emitter: worker.LogEventEmitter{},
	}
	orch := orchestrator.New(state, orchestrator.Deps{
		Dispatcher: dispatcher,
		Scheduler:  orchestrator.FixedDelayScheduler{Delay: 60 * time.Second},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		return err
	}
	return orchestrator.RunPollLoop(ctx, orchestrator.NewPoller(trackerClient, orch), pollInterval)
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

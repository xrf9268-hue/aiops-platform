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
		logStartupReconcileWorkflow(&workflow.Resolution{Source: workflow.SourceFile, Path: path}, wf)
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
		log.Printf("startup reconciliation: workflow source=%s tracker.kind=%s; reconciliation will be skipped unless tracker.kind is linear", resolution.Source, wf.Config.Tracker.Kind)
		return
	}
	if wf.Config.Tracker.Kind != "linear" {
		log.Printf("startup reconciliation: workflow source=%s path=%s tracker.kind=%s; reconciliation will be skipped unless tracker.kind is linear", resolution.Source, resolution.Path, wf.Config.Tracker.Kind)
		return
	}
	log.Printf("startup reconciliation: workflow source=%s path=%s tracker.kind=%s", resolution.Source, resolution.Path, wf.Config.Tracker.Kind)
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := worker.LoadConfigFromEnv()
	pool, err := pgxpool.New(ctx, cfg.DSN)
	if err != nil {
		return err
	}
	defer pool.Close()

	store := queue.New(pool)
	if wf, err := loadWorkflowForStartupReconcile(); err != nil {
		return err
	} else if wf.Config.Tracker.Kind == "linear" {
		if err := worker.ReconcileStartup(ctx, worker.ReconcileConfig{
			WorkspaceRoot:   cfg.WorkspaceRoot,
			ActiveStates:    wf.Config.Tracker.ActiveStates,
			TerminalStates:  wf.Config.Tracker.TerminalStates,
			Tracker:         tracker.NewLinearClient(wf.Config.Tracker),
			Emitter:         worker.LogEventEmitter{},
			ReconcileTaskID: "reconcile-startup",
		}); err != nil {
			worker.LogReconcileError(err)
			return err
		}
	}

	worker.Run(ctx, store, cfg)
	return nil
}

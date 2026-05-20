package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
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
	if cfg.Repo.CloneURL == "" && len(cfg.Services) == 0 {
		if source == workflow.SourceDefault {
			path = "built-in workflow defaults"
		}
		return fmt.Errorf("%s: repo.clone_url is required for poll-based worker runtime", path)
	}
	for i, service := range cfg.Services {
		if service.Repo.CloneURL == "" {
			return fmt.Errorf("%s: services[%d].repo.clone_url is required for poll-based worker runtime", path, i)
		}
	}
	return nil
}

func startupReconcileConfigForWorkflow(cfg workflow.Config, trackerClient worker.ReconcileTracker) worker.ReconcileConfig {
	hooks := cfg.WorkspaceHooks()
	return worker.ReconcileConfig{
		WorkspaceRoot:       worker.EffectiveWorkspaceRoot(worker.LoadConfigFromEnv(), cfg),
		ActiveStates:        cfg.Tracker.ActiveStates,
		TerminalStates:      cfg.Tracker.TerminalStates,
		TrackerKind:         cfg.Tracker.Kind,
		Tracker:             trackerClient,
		Emitter:             worker.LogEventEmitter{},
		ReconcileTaskID:     "reconcile-startup",
		BeforeRemoveHook:    hooks.BeforeRemove,
		HookTimeoutMillis:   hooks.TimeoutMs,
		ActiveWorkspaceKeys: worker.ActiveWorkspaceKeysForWorkflow(cfg),
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
	reconcileCfg.WorkspaceRoot = worker.EffectiveWorkspaceRoot(cfg, wf.Config)
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
		Scheduler:  orchestrator.RetryScheduler{MaxBackoff: time.Duration(wf.Config.Agent.MaxRetryBackoffMs) * time.Millisecond},
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
	if wf.Config.Server.Port >= 0 {
		serverErrs := make(chan error, 1)
		server := newStateHTTPServer(wf.Config.Server.Port, orch)
		listener, err := net.Listen("tcp", server.Addr)
		if err != nil {
			return err
		}
		log.Printf("state HTTP server listening on http://%s", listener.Addr().String())
		go func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("state HTTP server shutdown error: %v", err)
			}
		}()
		go func() {
			if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serverErrs <- err
				return
			}
			serverErrs <- nil
		}()
		go func() {
			if err := <-serverErrs; err != nil && ctx.Err() == nil {
				log.Printf("state HTTP server exited: %v", err)
			}
		}()
	}
	return orchestrator.RunPollLoopWithRuntime(ctx, poller, runtime, orchestrator.PollLoopRuntimeOptions{})
}

func newStateHTTPServer(port int, orch *orchestrator.Orchestrator) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/api/v1/state", stateHTTPHandler(orch.Snapshot))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><title>aiops-platform state</title><h1>aiops-platform</h1><p>Runtime state: <a href=\"/api/v1/state\">/api/v1/state</a></p>"))
	})
	return &http.Server{
		Addr:              fmt.Sprintf("127.0.0.1:%d", port),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

type stateSnapshotFunc func(context.Context) (orchestrator.StateView, error)

type apiStateResponse struct {
	GeneratedAt                time.Time                       `json:"generated_at"`
	PollIntervalMs             int64                           `json:"poll_interval_ms"`
	MaxConcurrentAgents        int                             `json:"max_concurrent_agents"`
	MaxConcurrentAgentsByState map[string]int                  `json:"max_concurrent_agents_by_state,omitempty"`
	Counts                     apiStateCounts                  `json:"counts"`
	Running                    []apiStateRunning               `json:"running"`
	Retrying                   []apiStateRetry                 `json:"retrying"`
	Completed                  []orchestrator.IssueID          `json:"completed"`
	Failed                     []orchestrator.IssueID          `json:"failed"`
	CodexTotals                orchestrator.CodexTotals        `json:"codex_totals"`
	RateLimits                 *orchestrator.RateLimitSnapshot `json:"rate_limits,omitempty"`
}

type apiStateCounts struct {
	Running   int `json:"running"`
	Retrying  int `json:"retrying"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
}

type apiStateRunning struct {
	IssueID       orchestrator.IssueID `json:"issue_id"`
	Identifier    string               `json:"issue_identifier,omitempty"`
	StartedAt     time.Time            `json:"started_at"`
	RetryAttempt  *int                 `json:"retry_attempt,omitempty"`
	WorkspacePath string               `json:"workspace_path,omitempty"`
	LastCodexAt   time.Time            `json:"last_codex_at,omitempty"`
}

type apiStateRetry struct {
	IssueID    orchestrator.IssueID `json:"issue_id"`
	Identifier string               `json:"issue_identifier,omitempty"`
	Attempt    int                  `json:"attempt"`
	DueAt      time.Time            `json:"due_at"`
	Error      string               `json:"error,omitempty"`
}

func stateHTTPHandler(snapshot stateSnapshotFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		view, err := snapshot(r.Context())
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				http.Error(w, err.Error(), 499)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(apiStateFromView(view)); err != nil {
			log.Printf("encode /api/v1/state response: %v", err)
		}
	})
}

func apiStateFromView(view orchestrator.StateView) apiStateResponse {
	running := make([]apiStateRunning, 0, len(view.Running))
	for _, row := range view.Running {
		running = append(running, apiStateRunning{
			IssueID:       row.IssueID,
			Identifier:    row.Identifier,
			StartedAt:     row.StartedAt,
			RetryAttempt:  row.RetryAttempt,
			WorkspacePath: row.WorkspacePath,
			LastCodexAt:   row.LastCodexAt,
		})
	}
	retrying := make([]apiStateRetry, 0, len(view.Retrying))
	for _, row := range view.Retrying {
		retrying = append(retrying, apiStateRetry{
			IssueID:    row.IssueID,
			Identifier: row.Identifier,
			Attempt:    row.Attempt,
			DueAt:      row.DueAt,
			Error:      row.Error,
		})
	}
	return apiStateResponse{
		GeneratedAt:                time.Now().UTC(),
		PollIntervalMs:             view.PollIntervalMs,
		MaxConcurrentAgents:        view.MaxConcurrentAgents,
		MaxConcurrentAgentsByState: copyConcurrencyLimits(view.MaxConcurrentAgentsByState),
		Counts: apiStateCounts{
			Running:   len(view.Running),
			Retrying:  len(view.Retrying),
			Completed: len(view.Completed),
			Failed:    len(view.Failed),
		},
		Running:     running,
		Retrying:    retrying,
		Completed:   append([]orchestrator.IssueID(nil), view.Completed...),
		Failed:      append([]orchestrator.IssueID(nil), view.Failed...),
		CodexTotals: view.CodexTotals,
		RateLimits:  view.CodexRateLimits,
	}
}

func copyConcurrencyLimits(src map[string]int) map[string]int {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]int, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
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
		projectConfigs := orchestrator.TrackerProjectConfigs(cfg)
		if len(projectConfigs) == 1 {
			return tracker.NewLinearClient(projectConfigs[0].Tracker), nil
		}
		clients := make([]trackerRuntimeClient, 0, len(projectConfigs))
		for _, projectCfg := range projectConfigs {
			clients = append(clients, tracker.NewLinearClient(projectCfg.Tracker))
		}
		return multiTrackerRuntimeClient{trackers: clients}, nil
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

type multiTrackerRuntimeClient struct {
	trackers []trackerRuntimeClient
}

func (c multiTrackerRuntimeClient) ListActiveIssues(ctx context.Context) ([]tracker.Issue, error) {
	var issues []tracker.Issue
	var errOut error
	for _, stateTracker := range c.trackers {
		got, err := stateTracker.ListActiveIssues(ctx)
		if err != nil {
			errOut = errors.Join(errOut, err)
			continue
		}
		issues = append(issues, got...)
	}
	return issues, errOut
}

func (c multiTrackerRuntimeClient) ListIssuesByStates(ctx context.Context, states []string) ([]tracker.Issue, error) {
	var issues []tracker.Issue
	var errOut error
	for _, stateTracker := range c.trackers {
		got, err := stateTracker.ListIssuesByStates(ctx, states)
		if err != nil {
			errOut = errors.Join(errOut, err)
			continue
		}
		issues = append(issues, got...)
	}
	return issues, errOut
}

func (c multiTrackerRuntimeClient) Trackers() []trackerRuntimeClient {
	return append([]trackerRuntimeClient(nil), c.trackers...)
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

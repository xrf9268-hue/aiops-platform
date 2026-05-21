package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
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
	maxFailureRetries := wf.Config.Agent.MaxRetryAttemptsValue()
	maxTurns := wf.Config.Agent.MaxTurns
	orch := orchestrator.New(state, orchestrator.Deps{
		Dispatcher:        dispatcher,
		Scheduler:         orchestrator.RetryScheduler{MaxBackoff: time.Duration(wf.Config.Agent.MaxRetryBackoffMs) * time.Millisecond},
		MaxFailureRetries: &maxFailureRetries,
		MaxTurns:          &maxTurns,
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
	go func() {
		if err := runStateHTTPServerLoop(ctx, runtime, orch.Snapshot, stateHTTPServerLoopOptions{Refresh: orch.RequestRefresh}); err != nil && ctx.Err() == nil {
			log.Printf("state HTTP server loop exited: %v", err)
		}
	}()
	return orchestrator.RunPollLoopWithRuntime(ctx, poller, runtime, orchestrator.PollLoopRuntimeOptions{})
}

type stateHTTPServerController struct {
	snapshot stateSnapshotFunc
	refresh  stateRefreshFunc

	desiredSet  bool
	desiredPort int
	cancel      context.CancelFunc
	addr        net.Addr
	serverDone  <-chan struct{}
}

func newStateHTTPServerController(snapshot stateSnapshotFunc, refresh ...stateRefreshFunc) *stateHTTPServerController {
	return &stateHTTPServerController{snapshot: snapshot, refresh: optionalStateRefreshFunc(refresh)}
}

func (c *stateHTTPServerController) apply(ctx context.Context, port int) error {
	c.refreshStopped()
	if c.desiredSet && c.desiredPort == port {
		return nil
	}
	c.stop()
	if port < 0 {
		c.desiredSet = true
		c.desiredPort = port
		log.Printf("state HTTP server disabled by server.port=%d", port)
		return nil
	}
	serverCtx, cancel := context.WithCancel(ctx)
	handle, err := startStateHTTPServer(serverCtx, port, c.snapshot, c.refresh)
	if err != nil {
		cancel()
		return err
	}
	if handle == nil {
		cancel()
		return nil
	}
	c.desiredSet = true
	c.desiredPort = port
	c.cancel = cancel
	c.addr = handle.Addr
	c.serverDone = handle.Done
	return nil
}

func (c *stateHTTPServerController) refreshStopped() {
	if c.serverDone == nil {
		return
	}
	select {
	case <-c.serverDone:
		if c.cancel != nil {
			c.cancel()
		}
		c.clear()
	default:
	}
}

func (c *stateHTTPServerController) stop() {
	if c.cancel != nil {
		c.cancel()
	}
	if c.serverDone != nil {
		select {
		case <-c.serverDone:
		case <-time.After(stateHTTPServerShutdownTimeout):
			log.Printf("state HTTP server did not stop within %s", stateHTTPServerShutdownTimeout)
		}
	}
	c.clear()
}

func (c *stateHTTPServerController) clear() {
	c.desiredSet = false
	c.desiredPort = 0
	c.cancel = nil
	c.addr = nil
	c.serverDone = nil
}

type stateHTTPServerLoopOptions struct {
	Sleep           func(context.Context, time.Duration) error
	StopAfterChecks int
	Refresh         stateRefreshFunc
}

func runStateHTTPServerLoop(ctx context.Context, runtime *orchestrator.WorkflowRuntime, snapshot stateSnapshotFunc, opts stateHTTPServerLoopOptions) error {
	if runtime == nil {
		return errors.New("state HTTP server loop requires workflow runtime")
	}
	controller := newStateHTTPServerController(snapshot, opts.Refresh)
	defer controller.stop()
	sleep := opts.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	interval := runtime.ReloadInterval()
	if interval <= 0 {
		interval = time.Second
	}
	checks := 0
	for {
		port := -1
		if snap := runtime.Current(); snap.Workflow != nil {
			port = snap.Workflow.Config.Server.Port
		}
		if err := controller.apply(ctx, port); err != nil {
			return err
		}
		checks++
		if opts.StopAfterChecks > 0 && checks >= opts.StopAfterChecks {
			return nil
		}
		if err := sleep(ctx, interval); err != nil {
			return err
		}
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

const stateHTTPServerShutdownTimeout = 10 * time.Second

type stateHTTPServerHandle struct {
	Addr net.Addr
	Done <-chan struct{}
}

func startStateHTTPServer(ctx context.Context, port int, snapshot stateSnapshotFunc, refresh ...stateRefreshFunc) (*stateHTTPServerHandle, error) {
	if port < 0 {
		log.Printf("state HTTP server disabled by server.port=%d", port)
		return nil, nil
	}
	server := newStateHTTPServer(port, snapshot, refresh...)
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		log.Printf("state HTTP server disabled because listen on %s failed: %v", server.Addr, err)
		return nil, nil
	}
	log.Printf("state HTTP server listening on http://%s", listener.Addr().String())
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("state HTTP server shutdown error: %v", err)
			}
		case <-done:
			return
		}
	}()
	go func() {
		defer close(done)
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			if ctx.Err() == nil {
				log.Printf("state HTTP server exited: %v", err)
			}
		}
	}()
	return &stateHTTPServerHandle{Addr: listener.Addr(), Done: done}, nil
}

func newStateHTTPServer(port int, snapshot stateSnapshotFunc, refresh ...stateRefreshFunc) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/api/v1/state", stateHTTPHandler(snapshot))
	mux.Handle("/api/v1/refresh", refreshHTTPHandler(optionalStateRefreshFunc(refresh)))
	mux.Handle("/api/v1/", issueHTTPHandler(snapshot))
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
		Handler:           loopbackHostOnly(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
}

func loopbackHostOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackHTTPHost(r.Host) {
			http.Error(w, "misdirected request", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopbackHTTPHost(hostport string) bool {
	if hostport == "" {
		return false
	}
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	} else if strings.Contains(hostport, ":") && !strings.HasPrefix(hostport, "[") {
		// "host:port" without IPv6 brackets that failed to split — malformed.
		return false
	}
	// Strip IPv6 brackets only when the host is properly bracketed (e.g. "[::1]").
	// Reject unpaired brackets — "[::1" or "::1]" are malformed Host values.
	if strings.HasPrefix(host, "[") || strings.HasSuffix(host, "]") {
		if !strings.HasPrefix(host, "[") || !strings.HasSuffix(host, "]") {
			return false
		}
		host = host[1 : len(host)-1]
	}
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

type stateSnapshotFunc func(context.Context) (orchestrator.StateView, error)
type stateRefreshFunc func(context.Context) (orchestrator.RefreshRequestResult, error)

const (
	refreshRequestHeader      = "X-AIOPS-Refresh"
	refreshRequestHeaderValue = "true"
)

func optionalStateRefreshFunc(refresh []stateRefreshFunc) stateRefreshFunc {
	if len(refresh) == 0 {
		return nil
	}
	return refresh[0]
}

type apiStateResponse struct {
	GeneratedAt                time.Time                       `json:"generated_at"`
	PollIntervalMs             int64                           `json:"poll_interval_ms"`
	MaxConcurrentAgents        int                             `json:"max_concurrent_agents"`
	MaxConcurrentAgentsByState map[string]int                  `json:"max_concurrent_agents_by_state,omitempty"`
	Counts                     apiStateCounts                  `json:"counts"`
	Running                    []apiStateRunning               `json:"running"`
	Blocked                    []apiStateBlocked               `json:"blocked"`
	Retrying                   []apiStateRetry                 `json:"retrying"`
	Completed                  []orchestrator.IssueID          `json:"completed"`
	Failed                     []orchestrator.IssueID          `json:"failed"`
	CodexTotals                apiCodexTotals                  `json:"codex_totals"`
	RateLimits                 *orchestrator.RateLimitSnapshot `json:"rate_limits,omitempty"`
}

type apiCodexTotals struct {
	InputTokens    int64   `json:"input_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
	TotalTokens    int64   `json:"total_tokens"`
	SecondsRunning float64 `json:"seconds_running"`
}

type apiStateCounts struct {
	Running   int `json:"running"`
	Blocked   int `json:"blocked"`
	Retrying  int `json:"retrying"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
}

type apiStateRunning struct {
	IssueID       orchestrator.IssueID `json:"issue_id"`
	Identifier    string               `json:"issue_identifier,omitempty"`
	StartedAt     *time.Time           `json:"started_at,omitempty"`
	RetryAttempt  *int                 `json:"retry_attempt,omitempty"`
	WorkspacePath string               `json:"workspace_path,omitempty"`
	LastCodexAt   *time.Time           `json:"last_codex_at,omitempty"`
}

type apiStateBlocked struct {
	IssueID       orchestrator.IssueID `json:"issue_id"`
	Identifier    string               `json:"issue_identifier,omitempty"`
	State         string               `json:"state,omitempty"`
	BlockedAt     *time.Time           `json:"blocked_at,omitempty"`
	WorkspacePath string               `json:"workspace_path,omitempty"`
	SessionID     string               `json:"session_id,omitempty"`
	LastCodexAt   *time.Time           `json:"last_codex_at,omitempty"`
	Method        string               `json:"method,omitempty"`
	Error         string               `json:"error,omitempty"`
}

type apiStateRetry struct {
	IssueID    orchestrator.IssueID `json:"issue_id"`
	Identifier string               `json:"issue_identifier,omitempty"`
	Attempt    int                  `json:"attempt"`
	DueAt      *time.Time           `json:"due_at,omitempty"`
	Error      string               `json:"error,omitempty"`
}

type apiIssueResponse struct {
	IssueIdentifier string               `json:"issue_identifier"`
	IssueID         orchestrator.IssueID `json:"issue_id"`
	Status          string               `json:"status"`
	Workspace       struct {
		Path string `json:"path,omitempty"`
	} `json:"workspace"`
	Attempts struct {
		RestartCount        int  `json:"restart_count"`
		CurrentRetryAttempt *int `json:"current_retry_attempt"`
	} `json:"attempts"`
	Running      *apiStateRunning `json:"running"`
	Retry        *apiStateRetry   `json:"retry"`
	RecentEvents []map[string]any `json:"recent_events"`
	LastError    *string          `json:"last_error"`
	Tracked      map[string]any   `json:"tracked"`
}

type apiErrorResponse struct {
	Error apiError `json:"error"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func stateHTTPHandler(snapshot stateSnapshotFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		view, err := snapshot(r.Context())
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				writeAPIError(w, http.StatusServiceUnavailable, "request_cancelled", err.Error())
				return
			}
			writeAPIError(w, http.StatusInternalServerError, apiErrorCode(err), err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(apiStateFromView(view)); err != nil {
			log.Printf("encode /api/v1/state response: %v", err)
		}
	})
}

func issueHTTPHandler(snapshot stateSnapshotFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		identifier, err := issueIdentifierFromPath(r.URL.Path)
		if err != nil {
			writeAPIError(w, http.StatusNotFound, "issue_not_found", err.Error())
			return
		}
		view, err := snapshot(r.Context())
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				writeAPIError(w, http.StatusServiceUnavailable, "request_cancelled", err.Error())
				return
			}
			writeAPIError(w, http.StatusInternalServerError, apiErrorCode(err), err.Error())
			return
		}
		payload, ok := apiIssueFromView(view, identifier)
		if !ok {
			writeAPIError(w, http.StatusNotFound, "issue_not_found", fmt.Sprintf("issue %q was not found in the current runtime state", identifier))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(payload); err != nil {
			log.Printf("encode /api/v1/%s response: %v", identifier, err)
		}
	})
}

func refreshHTTPHandler(refresh stateRefreshFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		if err := validateRefreshBody(r); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_refresh_body", err.Error())
			return
		}
		if !validRefreshHeader(r) {
			writeAPIError(w, http.StatusForbidden, "refresh_header_required", fmt.Sprintf("%s: %s header is required", refreshRequestHeader, refreshRequestHeaderValue))
			return
		}
		if refresh == nil {
			writeAPIError(w, http.StatusServiceUnavailable, "refresh_unavailable", "refresh trigger is not configured")
			return
		}
		result, err := refresh(r.Context())
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				writeAPIError(w, http.StatusServiceUnavailable, "refresh_unavailable", err.Error())
				return
			}
			writeAPIError(w, http.StatusInternalServerError, "refresh_failed", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		if err := json.NewEncoder(w).Encode(result); err != nil {
			log.Printf("encode /api/v1/refresh response: %v", err)
		}
	})
}

func validRefreshHeader(r *http.Request) bool {
	return strings.EqualFold(strings.TrimSpace(r.Header.Get(refreshRequestHeader)), refreshRequestHeaderValue)
}

func issueIdentifierFromPath(path string) (string, error) {
	raw := strings.TrimPrefix(path, "/api/v1/")
	raw = strings.Trim(raw, "/")
	if raw == "" {
		return "", errors.New("missing issue identifier")
	}
	identifier, err := url.PathUnescape(raw)
	if err != nil {
		return "", fmt.Errorf("invalid issue identifier %q", raw)
	}
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return "", errors.New("missing issue identifier")
	}
	return identifier, nil
}

func validateRefreshBody(r *http.Request) error {
	if r.Body == nil {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	bodyText := strings.TrimSpace(string(body))
	if bodyText == "" {
		return nil
	}
	if bodyText[0] != '{' {
		return fmt.Errorf("refresh request body must be empty or {}")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(bodyText), &object); err != nil {
		return fmt.Errorf("refresh request body must be empty or {}")
	}
	if len(object) != 0 {
		return fmt.Errorf("refresh request body must be empty or {}")
	}
	return nil
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(apiErrorResponse{Error: apiError{Code: code, Message: message}}); err != nil {
		log.Printf("encode API error response: %v", err)
	}
}

func apiIssueFromView(view orchestrator.StateView, identifier string) (apiIssueResponse, bool) {
	want := normalizeIssueLookup(identifier)
	base := func(issueID orchestrator.IssueID, identifier, status string) apiIssueResponse {
		return apiIssueResponse{
			IssueIdentifier: identifier,
			IssueID:         issueID,
			Status:          status,
			RecentEvents:    []map[string]any{},
			Tracked:         map[string]any{},
		}
	}
	for _, row := range view.Running {
		if !matchesIssueLookup(row.IssueID, row.Identifier, want) {
			continue
		}
		running := apiRunningFromView(row)
		payload := base(row.IssueID, row.Identifier, "running")
		payload.Workspace.Path = row.WorkspacePath
		payload.Attempts.CurrentRetryAttempt = copyIntPointer(row.RetryAttempt)
		payload.Attempts.RestartCount = restartCountFromRetryAttempt(row.RetryAttempt)
		payload.Running = &running
		return payload, true
	}
	for _, row := range view.Retrying {
		if !matchesIssueLookup(row.IssueID, row.Identifier, want) {
			continue
		}
		retry := apiRetryFromView(row)
		payload := base(row.IssueID, row.Identifier, "retrying")
		payload.Attempts.CurrentRetryAttempt = &retry.Attempt
		payload.Attempts.RestartCount = restartCountFromRetryAttempt(&retry.Attempt)
		payload.Retry = &retry
		payload.LastError = stringPointerIfNotEmpty(row.Error)
		return payload, true
	}
	return apiIssueResponse{}, false
}

// restartCountFromRetryAttempt mirrors the Symphony Elixir reference
// (lib/symphony_elixir_web/presenter.ex: `max(retry_attempt - 1, 0)`), and
// matches the SPEC §13.7.2 example payload where restart_count=1 corresponds
// to current_retry_attempt=2 (i.e. one prior restart triggered the second
// attempt). nil retry attempt means the issue has not been retried, so the
// restart count is zero.
func restartCountFromRetryAttempt(retryAttempt *int) int {
	if retryAttempt == nil {
		return 0
	}
	if *retryAttempt <= 0 {
		return 0
	}
	return *retryAttempt - 1
}

func matchesIssueLookup(issueID orchestrator.IssueID, identifier, normalizedWant string) bool {
	return normalizeIssueLookup(identifier) == normalizedWant || normalizeIssueLookup(string(issueID)) == normalizedWant
}

func normalizeIssueLookup(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func apiRunningFromView(row orchestrator.RunningView) apiStateRunning {
	var startedAt *time.Time
	if !row.StartedAt.IsZero() {
		v := row.StartedAt
		startedAt = &v
	}
	var lastCodexAt *time.Time
	if !row.LastCodexAt.IsZero() {
		v := row.LastCodexAt
		lastCodexAt = &v
	}
	return apiStateRunning{
		IssueID:       row.IssueID,
		Identifier:    row.Identifier,
		StartedAt:     startedAt,
		RetryAttempt:  copyIntPointer(row.RetryAttempt),
		WorkspacePath: row.WorkspacePath,
		LastCodexAt:   lastCodexAt,
	}
}

func apiRetryFromView(row orchestrator.RetryView) apiStateRetry {
	var dueAt *time.Time
	if !row.DueAt.IsZero() {
		v := row.DueAt
		dueAt = &v
	}
	return apiStateRetry{
		IssueID:    row.IssueID,
		Identifier: row.Identifier,
		Attempt:    row.Attempt,
		DueAt:      dueAt,
		Error:      row.Error,
	}
}

func copyIntPointer(in *int) *int {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func stringPointerIfNotEmpty(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func apiErrorCode(err error) string {
	if category, ok := workflow.ErrorCategory(err); ok {
		return string(category)
	}
	if category, ok := tracker.ErrorCategory(err); ok {
		return string(category)
	}
	return "internal_error"
}

func apiStateFromView(view orchestrator.StateView) apiStateResponse {
	generatedAt := view.GeneratedAt
	if generatedAt.IsZero() {
		generatedAt = time.Now().UTC()
	}
	running := make([]apiStateRunning, 0, len(view.Running))
	for _, row := range view.Running {
		running = append(running, apiRunningFromView(row))
	}
	sort.Slice(running, func(i, j int) bool {
		return running[i].IssueID < running[j].IssueID
	})
	blocked := make([]apiStateBlocked, 0, len(view.Blocked))
	for _, row := range view.Blocked {
		var blockedAt *time.Time
		if !row.BlockedAt.IsZero() {
			v := row.BlockedAt
			blockedAt = &v
		}
		var lastCodexAt *time.Time
		if !row.LastCodexAt.IsZero() {
			v := row.LastCodexAt
			lastCodexAt = &v
		}
		blocked = append(blocked, apiStateBlocked{
			IssueID:       row.IssueID,
			Identifier:    row.Identifier,
			State:         row.State,
			BlockedAt:     blockedAt,
			WorkspacePath: row.WorkspacePath,
			SessionID:     row.SessionID,
			LastCodexAt:   lastCodexAt,
			Method:        row.Method,
			Error:         row.Error,
		})
	}
	sort.Slice(blocked, func(i, j int) bool {
		return blocked[i].IssueID < blocked[j].IssueID
	})
	retrying := make([]apiStateRetry, 0, len(view.Retrying))
	for _, row := range view.Retrying {
		retrying = append(retrying, apiRetryFromView(row))
	}
	sort.Slice(retrying, func(i, j int) bool {
		return retrying[i].IssueID < retrying[j].IssueID
	})
	completed := append([]orchestrator.IssueID(nil), view.Completed...)
	sort.Slice(completed, func(i, j int) bool {
		return completed[i] < completed[j]
	})
	failed := append([]orchestrator.IssueID(nil), view.Failed...)
	sort.Slice(failed, func(i, j int) bool {
		return failed[i] < failed[j]
	})
	var rateLimits *orchestrator.RateLimitSnapshot
	if view.CodexRateLimits != nil {
		copied := *view.CodexRateLimits
		rateLimits = &copied
	}
	return apiStateResponse{
		GeneratedAt:                generatedAt,
		PollIntervalMs:             view.PollIntervalMs,
		MaxConcurrentAgents:        view.MaxConcurrentAgents,
		MaxConcurrentAgentsByState: copyConcurrencyLimits(view.MaxConcurrentAgentsByState),
		Counts: apiStateCounts{
			Running:   len(view.Running),
			Blocked:   len(view.Blocked),
			Retrying:  len(view.Retrying),
			Completed: len(view.Completed),
			Failed:    len(view.Failed),
		},
		Running:   running,
		Blocked:   blocked,
		Retrying:  retrying,
		Completed: completed,
		Failed:    failed,
		CodexTotals: apiCodexTotals{
			InputTokens:    view.CodexTotals.InputTokens,
			OutputTokens:   view.CodexTotals.OutputTokens,
			TotalTokens:    view.CodexTotals.TotalTokens,
			SecondsRunning: view.CodexTotals.SecondsRunning,
		},
		RateLimits: rateLimits,
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
	case "github":
		return nil
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
	case "github":
		baseURL := cfg.Tracker.BaseURL
		if baseURL == "" {
			baseURL = env("GITHUB_API_BASE_URL", "https://api.github.com")
		}
		return tracker.NewGitHubClient(cfg.Tracker, baseURL, cfg.Repo.Owner, cfg.Repo.Name), nil
	default:
		return nil, tracker.NewError(tracker.CategoryUnsupportedTrackerKind, fmt.Sprintf("unsupported tracker.kind %q", cfg.Tracker.Kind), nil)
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

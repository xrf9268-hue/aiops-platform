package main

// stateapi.go holds the SPEC §13.7 state API surface: the JSON response
// types, the /api/v1 handlers, and the orchestrator StateView -> API mappers.
// The HTTP server lifecycle that mounts these handlers lives in
// statehttp_server.go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/orchestrator"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

const (
	refreshRequestHeader      = "X-AIOPS-Refresh"
	refreshRequestHeaderValue = "true"
	refreshBodyLimitBytes     = 1 << 20
)

var errRefreshBodyTooLarge = errors.New("refresh request body exceeds 1 MiB")

type apiStateResponse struct {
	GeneratedAt                time.Time              `json:"generated_at"`
	PollIntervalMs             int64                  `json:"poll_interval_ms"`
	MaxConcurrentAgents        int                    `json:"max_concurrent_agents"`
	MaxConcurrentAgentsByState map[string]int         `json:"max_concurrent_agents_by_state,omitempty"`
	Counts                     apiStateCounts         `json:"counts"`
	Running                    []apiStateRunning      `json:"running"`
	Blocked                    []apiStateBlocked      `json:"blocked"`
	Retrying                   []apiStateRetry        `json:"retrying"`
	Completed                  []orchestrator.IssueID `json:"completed"`
	Failed                     []orchestrator.IssueID `json:"failed"`
	CodexTotals                apiCodexTotals         `json:"codex_totals"`
	// RateLimits is the latest Codex rate-limit payload (SPEC §13.7.2). It
	// is emitted unconditionally — `null` until a `rate_limit_updated`
	// notification is observed — so operators can rely on the key always
	// being present (upstream parity, #328). A nil snapshot marshals to
	// JSON null, not an omitted key.
	RateLimits *orchestrator.RateLimitSnapshot `json:"rate_limits"`
}
type apiCodexTotals struct {
	InputTokens    int64   `json:"input_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
	TotalTokens    int64   `json:"total_tokens"`
	SecondsRunning float64 `json:"seconds_running"`
}
type apiStateCounts struct {
	Running int `json:"running"`
	Blocked int `json:"blocked"`
	// Retrying is the current retry-backoff queue depth.
	Retrying int `json:"retrying"`
	// Completed is the size of the FIFO-bounded recent-completed set
	// (the same set published as `state.completed`). For lifetime
	// totals across worker restarts and FIFO evictions, use
	// completed_total. SPEC §13.7 §4.1.8.
	Completed int `json:"completed"`
	// Failed is the size of the dispatch-suppression set the
	// orchestrator currently holds — i.e. the count of issues whose
	// non-retryable failure still blocks redispatch. Unlike
	// `completed`, this is NOT bounded by the recent-FIFO cap: the
	// suppression set must keep entries until ReleaseFailedIfIssueChanged
	// observes a tracker state/updated_at change, or the entry would
	// spin every poll cycle. For the recent N IDs that /api/v1/state
	// publishes under `failed`, see that array. For the lifetime
	// monotonic counter, see `failed_total`.
	Failed int `json:"failed"`
	// CompletedTotal / FailedTotal are monotonic counters that count
	// every observed Succeeded / NonRetryableFailed transition since
	// process start, independent of FIFO eviction or release. Added
	// for #234 so long-running deployments still expose a true
	// lifetime number when the bounded sets have rotated.
	CompletedTotal int64 `json:"completed_total"`
	FailedTotal    int64 `json:"failed_total"`
}
type apiStateRunning struct {
	IssueID    orchestrator.IssueID `json:"issue_id"`
	Identifier string               `json:"issue_identifier,omitempty"`
	// State / SessionID / TurnCount / LastEvent / LastMessage are part of
	// the SPEC §13.7.2 running-row contract — the sample literally shows
	// `"last_message": ""` and `"turn_count": 7`, so a freshly-dispatched
	// run with zero/empty values must still emit the keys. omitempty would
	// let consumers confuse "known zero/empty" with "field missing".
	State             string           `json:"state"`
	SessionID         string           `json:"session_id"`
	TurnCount         int              `json:"turn_count"`
	LastEvent         string           `json:"last_event"`
	LastMessage       string           `json:"last_message"`
	StartedAt         *time.Time       `json:"started_at,omitempty"`
	LastEventAt       *time.Time       `json:"last_event_at,omitempty"`
	RetryAttempt      *int             `json:"retry_attempt,omitempty"`
	WorkspacePath     string           `json:"workspace_path,omitempty"`
	Tokens            apiRunningTokens `json:"tokens"`
	CodexAppServerPID int              `json:"codex_app_server_pid,omitempty"`
}

// apiRunningTokens mirrors SPEC §13.7.2's per-running-row `tokens` object.
type apiRunningTokens struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}
type apiStateBlocked struct {
	IssueID           orchestrator.IssueID `json:"issue_id"`
	Identifier        string               `json:"issue_identifier,omitempty"`
	State             string               `json:"state,omitempty"`
	BlockedAt         *time.Time           `json:"blocked_at,omitempty"`
	WorkspacePath     string               `json:"workspace_path,omitempty"`
	SessionID         string               `json:"session_id,omitempty"`
	LastEventAt       *time.Time           `json:"last_event_at,omitempty"`
	Method            string               `json:"method,omitempty"`
	Error             string               `json:"error,omitempty"`
	CodexAppServerPID int                  `json:"codex_app_server_pid,omitempty"`
}
type apiStateRetry struct {
	IssueID    orchestrator.IssueID   `json:"issue_id"`
	Identifier string                 `json:"issue_identifier,omitempty"`
	Attempt    int                    `json:"attempt"`
	DueAt      *time.Time             `json:"due_at,omitempty"`
	Error      string                 `json:"error,omitempty"`
	Kind       orchestrator.RetryKind `json:"kind"`
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
				log.Printf("state snapshot cancelled: %v", err)
				writeAPIError(w, http.StatusServiceUnavailable, "request_cancelled", "request cancelled before snapshot completed")
				return
			}
			log.Printf("state snapshot error: %v", err)
			writeAPIError(w, http.StatusInternalServerError, apiErrorCode(err), "snapshot temporarily unavailable")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(apiStateFromView(view)); err != nil {
			log.Printf("encode /api/v1/state response: %v", err)
		}
	})
}
func issueHTTPHandler(snapshot stateSnapshotFunc) http.Handler { //nolint:gocognit // baseline (#521)
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
				log.Printf("issue snapshot cancelled: %v", err)
				writeAPIError(w, http.StatusServiceUnavailable, "request_cancelled", "request cancelled before snapshot completed")
				return
			}
			log.Printf("issue snapshot error: %v", err)
			writeAPIError(w, http.StatusInternalServerError, apiErrorCode(err), "snapshot temporarily unavailable")
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
func refreshHTTPHandler(refresh stateRefreshFunc) http.Handler { //nolint:gocognit // baseline (#521)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		if err := validateRefreshBody(r); err != nil {
			if errors.Is(err, errRefreshBodyTooLarge) {
				writeAPIError(w, http.StatusRequestEntityTooLarge, "refresh_body_too_large", err.Error())
				return
			}
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
				log.Printf("refresh cancelled: %v", err)
				writeAPIError(w, http.StatusServiceUnavailable, "refresh_unavailable", "request cancelled before refresh completed")
				return
			}
			log.Printf("refresh error: %v", err)
			writeAPIError(w, http.StatusInternalServerError, "refresh_failed", "refresh trigger temporarily unavailable")
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
	if r.ContentLength > refreshBodyLimitBytes {
		return errRefreshBodyTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, refreshBodyLimitBytes+1))
	if err != nil {
		return err
	}
	if len(body) > refreshBodyLimitBytes {
		return errRefreshBodyTooLarge
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
	if payload, ok := apiTerminalIssueFromView(view, want); ok {
		return payload, true
	}
	return apiIssueResponse{}, false
}
func apiTerminalIssueFromView(view orchestrator.StateView, normalizedWant string) (apiIssueResponse, bool) { //nolint:gocognit // baseline (#521)
	base := func(issueID orchestrator.IssueID, identifier, status string) apiIssueResponse {
		return apiIssueResponse{
			IssueIdentifier: identifier,
			IssueID:         issueID,
			Status:          status,
			RecentEvents:    []map[string]any{},
			Tracked:         map[string]any{},
		}
	}
	for i := len(view.RecentEvents) - 1; i >= 0; i-- {
		ev := view.RecentEvents[i]
		if ev.Kind != orchestrator.RuntimeEventCompleted && ev.Kind != orchestrator.RuntimeEventFailed {
			continue
		}
		if !matchesIssueLookup(ev.IssueID, ev.Identifier, normalizedWant) {
			continue
		}
		payload := base(ev.IssueID, ev.Identifier, string(ev.Kind))
		payload.RecentEvents = []map[string]any{apiRuntimeEventFromView(ev)}
		if ev.Kind == orchestrator.RuntimeEventFailed {
			payload.LastError = stringPointerIfNotEmpty(ev.Message)
		}
		return payload, true
	}
	for _, issueID := range view.Completed {
		if matchesIssueLookup(issueID, "", normalizedWant) {
			return base(issueID, string(issueID), "completed"), true
		}
	}
	for _, issueID := range view.Failed {
		if matchesIssueLookup(issueID, "", normalizedWant) {
			return base(issueID, string(issueID), "failed"), true
		}
	}
	return apiIssueResponse{}, false
}
func apiRuntimeEventFromView(ev orchestrator.RuntimeEvent) map[string]any {
	out := map[string]any{
		"kind":       ev.Kind,
		"issue_id":   ev.IssueID,
		"identifier": ev.Identifier,
		"message":    ev.Message,
		"at":         ev.At,
	}
	if ev.Branch != "" {
		out["branch"] = ev.Branch
	}
	if ev.PRURL != "" {
		out["pr_url"] = ev.PRURL
	}
	return out
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
	var lastEventAt *time.Time
	if !row.LastEventAt.IsZero() {
		v := row.LastEventAt
		lastEventAt = &v
	}
	return apiStateRunning{
		IssueID:       row.IssueID,
		Identifier:    row.Identifier,
		State:         row.State,
		SessionID:     row.SessionID,
		TurnCount:     row.TurnCount,
		LastEvent:     row.LastEvent,
		LastMessage:   redactStateAPILastMessage(row.LastMessage),
		StartedAt:     startedAt,
		LastEventAt:   lastEventAt,
		RetryAttempt:  copyIntPointer(row.RetryAttempt),
		WorkspacePath: row.WorkspacePath,
		Tokens: apiRunningTokens{
			InputTokens:  row.Tokens.InputTokens,
			OutputTokens: row.Tokens.OutputTokens,
			TotalTokens:  row.Tokens.TotalTokens,
		},
		CodexAppServerPID: row.CodexAppServerPID,
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
		Kind:       retryKindOrFailure(row.Kind),
	}
}
func retryKindOrFailure(kind orchestrator.RetryKind) orchestrator.RetryKind {
	if kind == "" {
		return orchestrator.RetryKindFailure
	}
	return kind
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
	running := sortedAPIRunningRows(view.Running)
	blocked := make([]apiStateBlocked, 0, len(view.Blocked))
	for _, row := range view.Blocked {
		var blockedAt *time.Time
		if !row.BlockedAt.IsZero() {
			v := row.BlockedAt
			blockedAt = &v
		}
		var lastEventAt *time.Time
		if !row.LastEventAt.IsZero() {
			v := row.LastEventAt
			lastEventAt = &v
		}
		blocked = append(blocked, apiStateBlocked{
			IssueID:           row.IssueID,
			Identifier:        row.Identifier,
			State:             row.State,
			BlockedAt:         blockedAt,
			WorkspacePath:     row.WorkspacePath,
			SessionID:         row.SessionID,
			LastEventAt:       lastEventAt,
			Method:            row.Method,
			Error:             row.Error,
			CodexAppServerPID: row.CodexAppServerPID,
		})
	}
	sort.Slice(blocked, func(i, j int) bool {
		return blocked[i].IssueID < blocked[j].IssueID
	})
	retrying := sortedAPIRetryRows(view.Retrying)
	completed := append([]orchestrator.IssueID(nil), view.Completed...)
	sort.Slice(completed, func(i, j int) bool {
		return completed[i] < completed[j]
	})
	failed := append([]orchestrator.IssueID(nil), view.Failed...)
	sort.Slice(failed, func(i, j int) bool {
		return failed[i] < failed[j]
	})
	return apiStateResponse{
		GeneratedAt:                generatedAt,
		PollIntervalMs:             view.PollIntervalMs,
		MaxConcurrentAgents:        view.MaxConcurrentAgents,
		MaxConcurrentAgentsByState: copyConcurrencyLimits(view.MaxConcurrentAgentsByState),
		Counts:                     apiCountsFromView(view),
		Running:                    running,
		Blocked:                    blocked,
		Retrying:                   retrying,
		Completed:                  completed,
		Failed:                     failed,
		CodexTotals: apiCodexTotals{
			InputTokens:    view.CodexTotals.InputTokens,
			OutputTokens:   view.CodexTotals.OutputTokens,
			TotalTokens:    view.CodexTotals.TotalTokens,
			SecondsRunning: view.CodexTotals.SecondsRunning,
		},
		RateLimits: copyRateLimitsForAPI(view.CodexRateLimits),
	}
}
func sortedAPIRunningRows(rows []orchestrator.RunningView) []apiStateRunning {
	running := make([]apiStateRunning, 0, len(rows))
	for _, row := range rows {
		running = append(running, apiRunningFromView(row))
	}
	sort.Slice(running, func(i, j int) bool {
		return running[i].IssueID < running[j].IssueID
	})
	return running
}
func sortedAPIRetryRows(rows []orchestrator.RetryView) []apiStateRetry {
	retrying := make([]apiStateRetry, 0, len(rows))
	for _, row := range rows {
		retrying = append(retrying, apiRetryFromView(row))
	}
	sort.Slice(retrying, func(i, j int) bool {
		return retrying[i].IssueID < retrying[j].IssueID
	})
	return retrying
}
func apiCountsFromView(view orchestrator.StateView) apiStateCounts {
	return apiStateCounts{
		Running:        len(view.Running),
		Blocked:        len(view.Blocked),
		Retrying:       len(view.Retrying),
		Completed:      len(view.Completed),
		Failed:         view.FailedSuppressedCount,
		CompletedTotal: view.CumulativeCompletedTotal,
		FailedTotal:    view.CumulativeFailedTotal,
	}
}
func copyRateLimitsForAPI(src *orchestrator.RateLimitSnapshot) *orchestrator.RateLimitSnapshot {
	if src == nil {
		return nil
	}
	copied := *src
	return &copied
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

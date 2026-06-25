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
	"github.com/xrf9268-hue/aiops-platform/internal/stateapi"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

const (
	refreshRequestHeader      = "X-AIOPS-Refresh"
	refreshRequestHeaderValue = "true"
	refreshBodyLimitBytes     = 1 << 20
)

var errRefreshBodyTooLarge = errors.New("refresh request body exceeds 1 MiB")

// The /api/v1/state wire DTOs live in internal/stateapi so cmd/worker
// (producer) and cmd/tui (consumer) share one definition (#793). The
// orchestrator.StateView -> wire mappers below stay here: they are
// single-consumer projection, not part of the shared contract. The
// /api/v1/<issue> and error DTOs below stay here for the same reason —
// nothing else consumes them.
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
	Running      *stateapi.Running `json:"running"`
	Retry        *stateapi.Retry   `json:"retry"`
	RecentEvents []map[string]any  `json:"recent_events"`
	LastError    *string           `json:"last_error"`
	Tracked      map[string]any    `json:"tracked"`
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
	for _, stop := range view.OperatorTerminalStops {
		if matchesIssueLookup(stop.IssueID, stop.Identifier, normalizedWant) {
			return base(stop.IssueID, stop.Identifier, "operator_terminal_stop"), true
		}
	}
	for i := len(view.RecentEvents) - 1; i >= 0; i-- {
		ev := view.RecentEvents[i]
		if ev.Kind != orchestrator.RuntimeEventCompleted &&
			ev.Kind != orchestrator.RuntimeEventFailed &&
			ev.Kind != orchestrator.RuntimeEventReconcileStopped &&
			ev.Kind != orchestrator.RuntimeEventAgentHandoffReconcileStopped &&
			ev.Kind != orchestrator.RuntimeEventActiveSuccessNoHandoff &&
			ev.Kind != orchestrator.RuntimeEventOperatorTerminalStop {
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
	for _, issueID := range view.AgentHandoffReconcileStopped {
		if matchesIssueLookup(issueID, "", normalizedWant) {
			return base(issueID, string(issueID), "agent_handoff_reconcile_stopped"), true
		}
	}
	for _, issueID := range view.ActiveSuccessNoHandoff {
		if matchesIssueLookup(issueID, "", normalizedWant) {
			return base(issueID, string(issueID), "active_success_no_handoff"), true
		}
	}
	for _, issueID := range view.Completed {
		if matchesIssueLookup(issueID, "", normalizedWant) {
			return base(issueID, string(issueID), "completed"), true
		}
	}
	// Keep the per-issue lookup consistent with the aggregate: an ID surfaced in
	// reconcile_stopped_with_progress must be drillable here instead of returning
	// issue_not_found (#557). Checked after the more-specific buckets and
	// completed so those newer signals take precedence for overlapping IDs. A
	// failed run is surfaced by the RuntimeEventFailed scan above (failures now
	// retry per SPEC §8.4 rather than being parked in a suppression set).
	for _, issueID := range view.ReconcileStoppedWithProgress {
		if matchesIssueLookup(issueID, "", normalizedWant) {
			return base(issueID, string(issueID), "reconcile_stopped_with_progress"), true
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
func apiRunningFromView(row orchestrator.RunningView) stateapi.Running {
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
	return stateapi.Running{
		IssueID:       string(row.IssueID),
		Identifier:    row.Identifier,
		IssueURL:      row.IssueURL,
		State:         row.State,
		SessionID:     row.SessionID,
		TurnCount:     row.TurnCount,
		LastEvent:     row.LastEvent,
		LastMessage:   redactStateAPILastMessage(row.LastMessage),
		StartedAt:     startedAt,
		LastEventAt:   lastEventAt,
		RetryAttempt:  copyIntPointer(row.RetryAttempt),
		WorkspacePath: row.WorkspacePath,
		Tokens: stateapi.RunningTokens{
			InputTokens:  row.Tokens.InputTokens,
			OutputTokens: row.Tokens.OutputTokens,
			TotalTokens:  row.Tokens.TotalTokens,
		},
		CodexAppServerPID: row.CodexAppServerPID,
		AgentProvider:     row.AgentProvider,
		AgentModel:        row.AgentModel,
		WorkflowSource:    row.WorkflowSource,
		WorkflowPath:      row.WorkflowPath,
	}
}
func apiRetryFromView(row orchestrator.RetryView) stateapi.Retry {
	var dueAt *time.Time
	if !row.DueAt.IsZero() {
		v := row.DueAt
		dueAt = &v
	}
	return stateapi.Retry{
		IssueID:        string(row.IssueID),
		Identifier:     row.Identifier,
		IssueURL:       row.IssueURL,
		Attempt:        row.Attempt,
		DueAt:          dueAt,
		Error:          row.Error,
		Kind:           string(retryKindOrFailure(row.Kind)),
		StartupFailure: apiStartupFailure(row.StartupFailure),
	}
}

func apiStartupFailure(in *task.StartupFailure) *stateapi.StartupFailure {
	if in == nil {
		return nil
	}
	return &stateapi.StartupFailure{
		Phase: in.Phase,
		Error: in.Error,
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
func apiStateFromView(view orchestrator.StateView) stateapi.StateResponse {
	generatedAt := view.GeneratedAt
	if generatedAt.IsZero() {
		generatedAt = time.Now().UTC()
	}
	running := sortedAPIRunningRows(view.Running)
	blocked := make([]stateapi.Blocked, 0, len(view.Blocked))
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
		blocked = append(blocked, stateapi.Blocked{
			IssueID:           string(row.IssueID),
			Identifier:        row.Identifier,
			IssueURL:          row.IssueURL,
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
	operatorStops := sortedAPIOperatorTerminalStops(view.OperatorTerminalStops)
	return stateapi.StateResponse{
		Version:                      resolveVersion(),
		AgentDefault:                 view.AgentDefault,
		GeneratedAt:                  generatedAt,
		PollIntervalMs:               view.PollIntervalMs,
		MaxConcurrentAgents:          view.MaxConcurrentAgents,
		MaxConcurrentAgentsByState:   copyConcurrencyLimits(view.MaxConcurrentAgentsByState),
		Counts:                       apiCountsFromView(view),
		Running:                      running,
		Blocked:                      blocked,
		Retrying:                     retrying,
		Completed:                    sortedIssueIDStrings(view.Completed),
		ReconcileStoppedWithProgress: sortedIssueIDStrings(view.ReconcileStoppedWithProgress),
		AgentHandoffReconcileStopped: sortedIssueIDStrings(view.AgentHandoffReconcileStopped),
		ActiveSuccessNoHandoff:       sortedIssueIDStrings(view.ActiveSuccessNoHandoff),
		OperatorTerminalStops:        operatorStops,
		CodexTotals: stateapi.CodexTotals{
			InputTokens:    view.CodexTotals.InputTokens,
			OutputTokens:   view.CodexTotals.OutputTokens,
			TotalTokens:    view.CodexTotals.TotalTokens,
			SecondsRunning: view.CodexTotals.SecondsRunning,
		},
		RateLimits: rateLimitsForAPI(view.CodexRateLimits),
	}
}

// sortedIssueIDStrings converts an orchestrator IssueID slice to the wire
// []string and sorts it. It returns nil (not an empty slice) for empty input
// so the JSON key marshals to `null`, preserving the wire shape these ID
// lists have always emitted (the row lists below emit `[]` via make(.., 0)).
func sortedIssueIDStrings(ids []orchestrator.IssueID) []string {
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(id)
	}
	sort.Strings(out)
	return out
}
func sortedAPIRunningRows(rows []orchestrator.RunningView) []stateapi.Running {
	running := make([]stateapi.Running, 0, len(rows))
	for _, row := range rows {
		running = append(running, apiRunningFromView(row))
	}
	sort.Slice(running, func(i, j int) bool {
		return running[i].IssueID < running[j].IssueID
	})
	return running
}
func sortedAPIRetryRows(rows []orchestrator.RetryView) []stateapi.Retry {
	retrying := make([]stateapi.Retry, 0, len(rows))
	for _, row := range rows {
		retrying = append(retrying, apiRetryFromView(row))
	}
	sort.Slice(retrying, func(i, j int) bool {
		return retrying[i].IssueID < retrying[j].IssueID
	})
	return retrying
}
func apiCountsFromView(view orchestrator.StateView) stateapi.Counts {
	return stateapi.Counts{
		Running:                           len(view.Running),
		Blocked:                           len(view.Blocked),
		Retrying:                          len(view.Retrying),
		Completed:                         len(view.Completed),
		CompletedTotal:                    view.CumulativeCompletedTotal,
		ReconcileStoppedWithProgress:      len(view.ReconcileStoppedWithProgress),
		ReconcileStoppedWithProgressTotal: view.CumulativeReconcileStoppedWithProgressTotal,
		AgentHandoffReconcileStopped:      len(view.AgentHandoffReconcileStopped),
		AgentHandoffReconcileStoppedTotal: view.CumulativeAgentHandoffReconcileStoppedTotal,
		ActiveSuccessNoHandoff:            len(view.ActiveSuccessNoHandoff),
		ActiveSuccessNoHandoffTotal:       view.CumulativeActiveSuccessNoHandoffTotal,
		OperatorTerminalStops:             len(view.OperatorTerminalStops),
		OperatorTerminalStopsTotal:        view.CumulativeOperatorTerminalStopsTotal,
	}
}

func sortedAPIOperatorTerminalStops(rows []orchestrator.OperatorTerminalStopView) []stateapi.OperatorTerminalStop {
	out := make([]stateapi.OperatorTerminalStop, 0, len(rows))
	for _, row := range rows {
		var stoppedAt *time.Time
		if !row.StoppedAt.IsZero() {
			v := row.StoppedAt
			stoppedAt = &v
		}
		var firstSuppressedAt *time.Time
		if !row.FirstSuppressedAt.IsZero() {
			v := row.FirstSuppressedAt
			firstSuppressedAt = &v
		}
		out = append(out, stateapi.OperatorTerminalStop{
			IssueID:               string(row.IssueID),
			Identifier:            row.Identifier,
			State:                 row.State,
			StoppedAt:             stoppedAt,
			SuppressedDispatches:  row.SuppressedDispatches,
			FirstSuppressedAt:     firstSuppressedAt,
			FirstSuppressedState:  row.FirstSuppressedState,
			FirstSuppressedReason: row.FirstSuppressedReason,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].IssueID < out[j].IssueID
	})
	return out
}

// rateLimitsForAPI projects the orchestrator rate-limit snapshot to the wire
// map. A nil snapshot yields a nil map so `rate_limits` marshals to JSON null
// (the always-present key contract, #328). The returned map aliases the
// snapshot's backing store, matching the prior shallow copy — safe because the
// view is itself a point-in-time snapshot that the response is marshaled from
// immediately.
func rateLimitsForAPI(src *orchestrator.RateLimitSnapshot) map[string]any {
	if src == nil {
		return nil
	}
	return map[string]any(*src)
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

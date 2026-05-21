package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/gitea"
	"github.com/xrf9268-hue/aiops-platform/internal/orchestrator"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/worker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

type fakeReconcileTracker struct {
	issues []tracker.Issue
}

func samePath(a, b string) bool {
	eval := func(path string) string {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return path
		}
		return resolved
	}
	return eval(a) == eval(b)
}

func (f fakeReconcileTracker) ListIssuesByStates(_ context.Context, states []string) ([]tracker.Issue, error) {
	want := map[string]struct{}{}
	for _, state := range states {
		want[state] = struct{}{}
	}
	var out []tracker.Issue
	for _, issue := range f.issues {
		if _, ok := want[issue.State]; ok {
			out = append(out, issue)
		}
	}
	return out, nil
}

func TestStateHTTPHandlerReturnsRuntimeStateSnapshot(t *testing.T) {
	generatedAt := time.Date(2026, 5, 20, 9, 30, 0, 0, time.UTC)
	view := orchestrator.StateView{
		GeneratedAt:         generatedAt,
		PollIntervalMs:      30000,
		MaxConcurrentAgents: 2,
		Running: []orchestrator.RunningView{{
			IssueID:    "issue-2",
			Identifier: "MT-650",
		}, {
			IssueID:    "issue-1",
			Identifier: "MT-649",
		}},
		Retrying: []orchestrator.RetryView{{
			IssueID:    "issue-2",
			Identifier: "MT-650",
			Attempt:    2,
			Error:      "no available orchestrator slots",
		}, {
			IssueID:    "issue-1",
			Identifier: "MT-649",
			Attempt:    1,
			Error:      "retry soon",
		}},
		Blocked: []orchestrator.BlockedView{{
			IssueID:    "issue-7",
			Identifier: "MT-655",
			State:      "In Progress",
			Method:     "item/tool/requestUserInput",
			Error:      "input required",
		}, {
			IssueID:    "issue-6",
			Identifier: "MT-654",
			State:      "In Progress",
			Method:     "mcpServer/elicitation/request",
			Error:      "input required",
		}},
		Completed: []orchestrator.IssueID{"issue-9", "issue-3"},
		Failed:    []orchestrator.IssueID{"issue-8", "issue-4"},
		CodexTotals: orchestrator.CodexTotals{
			InputTokens:    10,
			OutputTokens:   20,
			TotalTokens:    30,
			SecondsRunning: 1.5,
		},
	}
	handler := stateHTTPHandler(func(context.Context) (orchestrator.StateView, error) {
		return view, nil
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/state", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q, want application/json", got)
	}
	var payload struct {
		GeneratedAt         string `json:"generated_at"`
		PollIntervalMs      int64  `json:"poll_interval_ms"`
		MaxConcurrentAgents int    `json:"max_concurrent_agents"`
		Counts              struct {
			Running  int `json:"running"`
			Retrying int `json:"retrying"`
			Blocked  int `json:"blocked"`
		} `json:"counts"`
		Running []struct {
			IssueID         string `json:"issue_id"`
			IssueIdentifier string `json:"issue_identifier"`
		} `json:"running"`
		Blocked []struct {
			IssueID         string `json:"issue_id"`
			IssueIdentifier string `json:"issue_identifier"`
			State           string `json:"state"`
			Method          string `json:"method"`
			Error           string `json:"error"`
		} `json:"blocked"`
		Retrying []struct {
			IssueID         string `json:"issue_id"`
			IssueIdentifier string `json:"issue_identifier"`
			Attempt         int    `json:"attempt"`
			Error           string `json:"error"`
		} `json:"retrying"`
		Completed   []string `json:"completed"`
		Failed      []string `json:"failed"`
		CodexTotals struct {
			InputTokens    int64   `json:"input_tokens"`
			OutputTokens   int64   `json:"output_tokens"`
			TotalTokens    int64   `json:"total_tokens"`
			SecondsRunning float64 `json:"seconds_running"`
		} `json:"codex_totals"`
	}
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw response: %v; body=%s", err, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, w.Body.String())
	}
	if payload.GeneratedAt == "" {
		t.Fatal("generated_at is empty")
	}
	if payload.GeneratedAt != generatedAt.Format(time.RFC3339) {
		t.Fatalf("generated_at = %q, want snapshot timestamp %q", payload.GeneratedAt, generatedAt.Format(time.RFC3339))
	}
	if payload.PollIntervalMs != 30000 || payload.MaxConcurrentAgents != 2 {
		t.Fatalf("state metadata = poll_interval_ms=%d max_concurrent_agents=%d, want 30000/2", payload.PollIntervalMs, payload.MaxConcurrentAgents)
	}
	if payload.Counts.Running != 2 || payload.Counts.Retrying != 2 || payload.Counts.Blocked != 2 {
		t.Fatalf("counts = %+v, want running=2 retrying=2 blocked=2", payload.Counts)
	}
	if len(payload.Running) != 2 || payload.Running[0].IssueID != "issue-1" || payload.Running[1].IssueID != "issue-2" {
		t.Fatalf("running = %+v, want sorted issue-1 then issue-2", payload.Running)
	}
	if len(payload.Blocked) != 2 || payload.Blocked[0].IssueID != "issue-6" || payload.Blocked[1].IssueID != "issue-7" {
		t.Fatalf("blocked = %+v, want sorted issue-6 then issue-7", payload.Blocked)
	}
	if payload.Blocked[0].IssueIdentifier != "MT-654" || payload.Blocked[0].State != "In Progress" || payload.Blocked[0].Method != "mcpServer/elicitation/request" || payload.Blocked[0].Error != "input required" {
		t.Fatalf("blocked row = %+v, want issue metadata plus input-required method/error", payload.Blocked[0])
	}
	if len(payload.Retrying) != 2 || payload.Retrying[0].IssueID != "issue-1" || payload.Retrying[1].IssueID != "issue-2" {
		t.Fatalf("retrying = %+v, want sorted issue-1 then issue-2", payload.Retrying)
	}
	if !reflect.DeepEqual(payload.Completed, []string{"issue-3", "issue-9"}) {
		t.Fatalf("completed = %+v, want sorted issue-3 issue-9", payload.Completed)
	}
	if !reflect.DeepEqual(payload.Failed, []string{"issue-4", "issue-8"}) {
		t.Fatalf("failed = %+v, want sorted issue-4 issue-8", payload.Failed)
	}
	if payload.CodexTotals.InputTokens != 10 || payload.CodexTotals.OutputTokens != 20 || payload.CodexTotals.TotalTokens != 30 || payload.CodexTotals.SecondsRunning != 1.5 {
		t.Fatalf("codex_totals = %+v, want snake_case token totals", payload.CodexTotals)
	}
	rawTotals, ok := raw["codex_totals"].(map[string]any)
	if !ok {
		t.Fatalf("raw codex_totals = %#v, want object", raw["codex_totals"])
	}
	for _, badKey := range []string{"InputTokens", "OutputTokens", "TotalTokens", "SecondsRunning"} {
		if _, ok := rawTotals[badKey]; ok {
			t.Fatalf("codex_totals contains Go field name %q: %#v", badKey, rawTotals)
		}
	}
	rawRunning, ok := raw["running"].([]any)
	if !ok {
		t.Fatalf("raw running = %#v, want array", raw["running"])
	}
	for _, row := range rawRunning {
		rowObject, ok := row.(map[string]any)
		if !ok {
			t.Fatalf("raw running row = %#v, want object", row)
		}
		if _, ok := rowObject["started_at"]; ok {
			t.Fatalf("zero started_at should be omitted from running row: %#v", rowObject)
		}
		if _, ok := rowObject["last_codex_at"]; ok {
			t.Fatalf("zero last_codex_at should be omitted from running row: %#v", rowObject)
		}
	}
	rawBlocked, ok := raw["blocked"].([]any)
	if !ok {
		t.Fatalf("raw blocked = %#v, want array", raw["blocked"])
	}
	for _, row := range rawBlocked {
		rowObject, ok := row.(map[string]any)
		if !ok {
			t.Fatalf("raw blocked row = %#v, want object", row)
		}
		if _, ok := rowObject["blocked_at"]; ok {
			t.Fatalf("zero blocked_at should be omitted from blocked row: %#v", rowObject)
		}
		if _, ok := rowObject["last_codex_at"]; ok {
			t.Fatalf("zero last_codex_at should be omitted from blocked row: %#v", rowObject)
		}
	}
	rawRetrying, ok := raw["retrying"].([]any)
	if !ok {
		t.Fatalf("raw retrying = %#v, want array", raw["retrying"])
	}
	for _, row := range rawRetrying {
		rowObject, ok := row.(map[string]any)
		if !ok {
			t.Fatalf("raw retrying row = %#v, want object", row)
		}
		if _, ok := rowObject["due_at"]; ok {
			t.Fatalf("zero due_at should be omitted from retry row: %#v", rowObject)
		}
	}
}

func TestStateHTTPHandlerPropagatesRequestCancellation(t *testing.T) {
	requestErr := context.Canceled
	handler := stateHTTPHandler(func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, requestErr
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/state", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status code = %d, want %d for request cancellation", w.Code, http.StatusServiceUnavailable)
	}
}

func TestStateHTTPHandlerRejectsNonGET(t *testing.T) {
	handler := stateHTTPHandler(func(context.Context) (orchestrator.StateView, error) {
		t.Fatal("snapshot function should not be called for non-GET requests")
		return orchestrator.StateView{}, nil
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/state", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status code = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestIssueHTTPHandlerReturnsRunningIssueByIdentifier(t *testing.T) {
	startedAt := time.Date(2026, 5, 21, 9, 0, 0, 0, time.UTC)
	retryAttempt := 2
	server := newStateHTTPServer(0, func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{
			Running: []orchestrator.RunningView{{
				IssueID:       "issue-1",
				Identifier:    "MT-649",
				StartedAt:     startedAt,
				RetryAttempt:  &retryAttempt,
				WorkspacePath: "/tmp/symphony/MT-649",
			}},
		}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4000/api/v1/mt-649", nil)
	w := httptest.NewRecorder()
	server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q, want application/json", got)
	}
	var payload struct {
		IssueID         string `json:"issue_id"`
		IssueIdentifier string `json:"issue_identifier"`
		Status          string `json:"status"`
		Workspace       struct {
			Path string `json:"path"`
		} `json:"workspace"`
		Attempts struct {
			RestartCount        int  `json:"restart_count"`
			CurrentRetryAttempt *int `json:"current_retry_attempt"`
		} `json:"attempts"`
		Running *struct {
			StartedAt string `json:"started_at"`
		} `json:"running"`
		Retry *struct{} `json:"retry"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode issue response: %v; body=%s", err, w.Body.String())
	}
	if payload.IssueID != "issue-1" || payload.IssueIdentifier != "MT-649" || payload.Status != "running" {
		t.Fatalf("issue payload identity/status = %+v, want running MT-649 issue-1", payload)
	}
	if payload.Workspace.Path != "/tmp/symphony/MT-649" {
		t.Fatalf("workspace path = %q, want running workspace path", payload.Workspace.Path)
	}
	if payload.Attempts.CurrentRetryAttempt == nil || *payload.Attempts.CurrentRetryAttempt != retryAttempt {
		t.Fatalf("current_retry_attempt = %v, want %d", payload.Attempts.CurrentRetryAttempt, retryAttempt)
	}
	// SPEC §13.7.2 example: restart_count=1 corresponds to current_retry_attempt=2
	// (matches Elixir reference: max(retry_attempt - 1, 0)).
	if want := retryAttempt - 1; payload.Attempts.RestartCount != want {
		t.Fatalf("restart_count = %d, want %d for current_retry_attempt %d", payload.Attempts.RestartCount, want, retryAttempt)
	}
	if payload.Running == nil || payload.Running.StartedAt != startedAt.Format(time.RFC3339) {
		t.Fatalf("running row = %+v, want started_at %s", payload.Running, startedAt.Format(time.RFC3339))
	}
	if payload.Retry != nil {
		t.Fatalf("retry = %+v, want null for running issue", payload.Retry)
	}
}

func TestIssueHTTPHandlerReturnsRestartCountForRetryingIssue(t *testing.T) {
	dueAt := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	server := newStateHTTPServer(0, func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{
			Retrying: []orchestrator.RetryView{{
				IssueID:    "issue-2",
				Identifier: "MT-650",
				Attempt:    3,
				DueAt:      dueAt,
				Error:      "tracker temporarily unavailable",
			}},
		}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4000/api/v1/MT-650", nil)
	w := httptest.NewRecorder()
	server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	var payload struct {
		Status   string `json:"status"`
		Attempts struct {
			RestartCount        int  `json:"restart_count"`
			CurrentRetryAttempt *int `json:"current_retry_attempt"`
		} `json:"attempts"`
		LastError *string `json:"last_error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode issue response: %v; body=%s", err, w.Body.String())
	}
	if payload.Status != "retrying" {
		t.Fatalf("status = %q, want retrying", payload.Status)
	}
	if payload.Attempts.CurrentRetryAttempt == nil || *payload.Attempts.CurrentRetryAttempt != 3 {
		t.Fatalf("current_retry_attempt = %v, want 3", payload.Attempts.CurrentRetryAttempt)
	}
	if payload.Attempts.RestartCount != 2 {
		t.Fatalf("restart_count = %d, want 2 for current_retry_attempt 3", payload.Attempts.RestartCount)
	}
	if payload.LastError == nil || *payload.LastError != "tracker temporarily unavailable" {
		t.Fatalf("last_error = %v, want retry error", payload.LastError)
	}
}

func TestIssueHTTPHandlerOmitsRestartCountForFirstRun(t *testing.T) {
	server := newStateHTTPServer(0, func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{
			Running: []orchestrator.RunningView{{
				IssueID:    "issue-3",
				Identifier: "MT-651",
				StartedAt:  time.Date(2026, 5, 21, 9, 30, 0, 0, time.UTC),
				// RetryAttempt is nil: the issue has never been retried.
			}},
		}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4000/api/v1/MT-651", nil)
	w := httptest.NewRecorder()
	server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status code = %d; body=%s", w.Code, w.Body.String())
	}
	var payload struct {
		Attempts struct {
			RestartCount        int  `json:"restart_count"`
			CurrentRetryAttempt *int `json:"current_retry_attempt"`
		} `json:"attempts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode issue response: %v; body=%s", err, w.Body.String())
	}
	if payload.Attempts.CurrentRetryAttempt != nil {
		t.Fatalf("current_retry_attempt = %v, want nil for first run", payload.Attempts.CurrentRetryAttempt)
	}
	if payload.Attempts.RestartCount != 0 {
		t.Fatalf("restart_count = %d, want 0 for first run", payload.Attempts.RestartCount)
	}
}

func TestIssueHTTPHandlerReturns404EnvelopeForUnknownIssue(t *testing.T) {
	server := newStateHTTPServer(0, func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4000/api/v1/NOPE-1", nil)
	w := httptest.NewRecorder()
	server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status code = %d, want %d; body=%s", w.Code, http.StatusNotFound, w.Body.String())
	}
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode error response: %v; body=%s", err, w.Body.String())
	}
	if payload.Error.Code != "issue_not_found" || !strings.Contains(payload.Error.Message, "NOPE-1") {
		t.Fatalf("error = %+v, want issue_not_found mentioning identifier", payload.Error)
	}
}

func TestIssueHTTPHandlerRejectsNonGET(t *testing.T) {
	called := false
	server := newStateHTTPServer(0, func(context.Context) (orchestrator.StateView, error) {
		called = true
		return orchestrator.StateView{}, nil
	})

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4000/api/v1/MT-649", nil)
	w := httptest.NewRecorder()
	server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status code = %d, want %d; body=%s", w.Code, http.StatusMethodNotAllowed, w.Body.String())
	}
	if called {
		t.Fatal("snapshot function should not be called for non-GET issue request")
	}
	if got := w.Header().Get("Allow"); got != http.MethodGet {
		t.Fatalf("Allow = %q, want GET", got)
	}
}

func TestRefreshHTTPHandlerQueuesAndCoalescesImmediatePoll(t *testing.T) {
	results := []orchestrator.RefreshRequestResult{
		{Queued: true, Coalesced: false, RequestedAt: time.Date(2026, 5, 21, 9, 10, 0, 0, time.UTC), Operations: []string{"poll", "reconcile"}},
		{Queued: true, Coalesced: true, RequestedAt: time.Date(2026, 5, 21, 9, 10, 1, 0, time.UTC), Operations: []string{"poll", "reconcile"}},
	}
	calls := 0
	server := newStateHTTPServer(0, func(context.Context) (orchestrator.StateView, error) {
		t.Fatal("snapshot function should not be called for refresh requests")
		return orchestrator.StateView{}, nil
	}, func(context.Context) (orchestrator.RefreshRequestResult, error) {
		if calls >= len(results) {
			t.Fatalf("refresh called %d times, want at most %d", calls+1, len(results))
		}
		result := results[calls]
		calls++
		return result, nil
	})

	first := httptest.NewRecorder()
	firstReq := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4000/api/v1/refresh", nil)
	firstReq.Header.Set(refreshRequestHeader, refreshRequestHeaderValue)
	server.Handler.ServeHTTP(first, firstReq)
	second := httptest.NewRecorder()
	secondReq := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4000/api/v1/refresh", strings.NewReader("{}"))
	secondReq.Header.Set(refreshRequestHeader, refreshRequestHeaderValue)
	server.Handler.ServeHTTP(second, secondReq)

	if first.Code != http.StatusAccepted {
		t.Fatalf("first status code = %d, want %d; body=%s", first.Code, http.StatusAccepted, first.Body.String())
	}
	if second.Code != http.StatusAccepted {
		t.Fatalf("second status code = %d, want %d; body=%s", second.Code, http.StatusAccepted, second.Body.String())
	}
	if calls != 2 {
		t.Fatalf("refresh calls = %d, want 2", calls)
	}
	var firstPayload, secondPayload struct {
		Queued      bool     `json:"queued"`
		Coalesced   bool     `json:"coalesced"`
		RequestedAt string   `json:"requested_at"`
		Operations  []string `json:"operations"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstPayload); err != nil {
		t.Fatalf("decode first refresh response: %v; body=%s", err, first.Body.String())
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondPayload); err != nil {
		t.Fatalf("decode second refresh response: %v; body=%s", err, second.Body.String())
	}
	if !firstPayload.Queued || firstPayload.Coalesced {
		t.Fatalf("first refresh payload = %+v, want queued and not coalesced", firstPayload)
	}
	if !secondPayload.Queued || !secondPayload.Coalesced {
		t.Fatalf("second refresh payload = %+v, want queued and coalesced", secondPayload)
	}
	if !reflect.DeepEqual(firstPayload.Operations, []string{"poll", "reconcile"}) || !reflect.DeepEqual(secondPayload.Operations, []string{"poll", "reconcile"}) {
		t.Fatalf("refresh operations = %+v / %+v, want poll+reconcile", firstPayload.Operations, secondPayload.Operations)
	}
}

func TestRefreshHTTPHandlerRequiresRefreshHeader(t *testing.T) {
	called := false
	server := newStateHTTPServer(0, func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, nil
	}, func(context.Context) (orchestrator.RefreshRequestResult, error) {
		called = true
		return orchestrator.RefreshRequestResult{}, nil
	})

	resp := httptest.NewRecorder()
	server.Handler.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4000/api/v1/refresh", nil))

	if resp.Code != http.StatusForbidden {
		t.Fatalf("status code = %d, want %d; body=%s", resp.Code, http.StatusForbidden, resp.Body.String())
	}
	if called {
		t.Fatal("refresh function should not be called without refresh header")
	}
}

func TestRefreshHTTPHandlerRejectsUnsupportedMethods(t *testing.T) {
	called := false
	server := newStateHTTPServer(0, func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, nil
	}, func(context.Context) (orchestrator.RefreshRequestResult, error) {
		called = true
		return orchestrator.RefreshRequestResult{}, nil
	})

	getResp := httptest.NewRecorder()
	server.Handler.ServeHTTP(getResp, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4000/api/v1/refresh", nil))
	if getResp.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status code = %d, want %d; body=%s", getResp.Code, http.StatusMethodNotAllowed, getResp.Body.String())
	}
	if got := getResp.Header().Get("Allow"); got != http.MethodPost {
		t.Fatalf("Allow = %q, want POST", got)
	}
	if called {
		t.Fatal("refresh function should not be called for unsupported method")
	}
}

func TestRefreshHTTPHandlerRejectsUnsupportedBodies(t *testing.T) {
	called := false
	server := newStateHTTPServer(0, func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, nil
	}, func(context.Context) (orchestrator.RefreshRequestResult, error) {
		called = true
		return orchestrator.RefreshRequestResult{}, nil
	})

	for _, body := range []string{`[]`, `null`, `"refresh"`, `{"force":true}`} {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4000/api/v1/refresh", strings.NewReader(body))
		req.Header.Set(refreshRequestHeader, refreshRequestHeaderValue)
		resp := httptest.NewRecorder()
		server.Handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusBadRequest {
			t.Fatalf("body %s status code = %d, want %d; response=%s", body, resp.Code, http.StatusBadRequest, resp.Body.String())
		}
	}
	if called {
		t.Fatal("refresh function should not be called for unsupported bodies")
	}
}

func TestStateHTTPServerRejectsNonLoopbackHost(t *testing.T) {
	called := false
	server := newStateHTTPServer(0, func(context.Context) (orchestrator.StateView, error) {
		called = true
		return orchestrator.StateView{}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "http://evil.example/api/v1/state", nil)
	w := httptest.NewRecorder()
	server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("status code = %d, want %d", w.Code, http.StatusMisdirectedRequest)
	}
	if called {
		t.Fatal("snapshot function should not be called for non-loopback Host")
	}
}

func TestStateHTTPServerAllowsLoopbackHost(t *testing.T) {
	called := false
	server := newStateHTTPServer(0, func(context.Context) (orchestrator.StateView, error) {
		called = true
		return orchestrator.StateView{}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4000/api/v1/state", nil)
	w := httptest.NewRecorder()
	server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if !called {
		t.Fatal("snapshot function was not called for loopback Host")
	}
}

func TestStateHTTPServerAllowsIPv6LoopbackHost(t *testing.T) {
	called := false
	server := newStateHTTPServer(0, func(context.Context) (orchestrator.StateView, error) {
		called = true
		return orchestrator.StateView{}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "http://[::1]:4000/api/v1/state", nil)
	w := httptest.NewRecorder()
	server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if !called {
		t.Fatal("snapshot function was not called for IPv6 loopback Host")
	}
}

func TestIsLoopbackHTTPHost(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"127.0.0.1:4000", true},
		{"127.0.0.1", true},
		{"127.1.2.3:8080", true},
		{"localhost:4000", true},
		{"localhost", true},
		{"[::1]:4000", true},
		{"[::1]", true},
		{"::1", false},
		{"1.2.3.4:4000", false},
		{"evil.example", false},
		{"evil.example:4000", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := isLoopbackHTTPHost(c.in); got != c.want {
				t.Fatalf("isLoopbackHTTPHost(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestStartStateHTTPServerSkipsDisabledPort(t *testing.T) {
	handle, err := startStateHTTPServer(context.Background(), -1, func(context.Context) (orchestrator.StateView, error) {
		t.Fatal("disabled state server must not evaluate snapshot")
		return orchestrator.StateView{}, nil
	})
	if err != nil {
		t.Fatalf("startStateHTTPServer disabled: %v", err)
	}
	if handle != nil {
		t.Fatalf("disabled state server handle = %v, want nil", handle)
	}
}

func TestStartStateHTTPServerDoesNotFailWorkerWhenPortInUse(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port

	handle, err := startStateHTTPServer(context.Background(), port, func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, nil
	})
	if err != nil {
		t.Fatalf("startStateHTTPServer occupied port: %v", err)
	}
	if handle != nil {
		t.Fatalf("occupied port state server handle = %v, want nil", handle)
	}
}

func TestStartStateHTTPServerBindsPrivateLoopback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handle, err := startStateHTTPServer(ctx, 0, func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, nil
	})
	if err != nil {
		t.Fatalf("startStateHTTPServer: %v", err)
	}
	tcpAddr, ok := handle.Addr.(*net.TCPAddr)
	if !ok {
		t.Fatalf("state server addr = %T %v, want TCP address", handle.Addr, handle.Addr)
	}
	if !tcpAddr.IP.Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("state server bind IP = %s, want 127.0.0.1", tcpAddr.IP)
	}
	cancel()
	select {
	case <-handle.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("state server did not stop after context cancellation")
	}
}

func TestStateHTTPServerControllerStopsOnDisabledReload(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	controller := newStateHTTPServerController(func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, nil
	})

	if err := controller.apply(ctx, 0); err != nil {
		t.Fatalf("start state HTTP server: %v", err)
	}
	if controller.cancel == nil || controller.addr == nil {
		t.Fatalf("controller after start = cancel:%v addr:%v, want running server", controller.cancel, controller.addr)
	}
	if err := controller.apply(ctx, -1); err != nil {
		t.Fatalf("disable state HTTP server: %v", err)
	}
	if controller.cancel != nil || controller.addr != nil {
		t.Fatalf("controller after disable = cancel:%v addr:%v, want stopped server", controller.cancel, controller.addr)
	}
}

func TestStateHTTPServerControllerRetriesSamePortAfterListenFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	controller := newStateHTTPServerController(func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, nil
	})
	if err := controller.apply(ctx, port); err != nil {
		t.Fatalf("apply occupied port: %v", err)
	}
	if controller.cancel != nil || controller.addr != nil || controller.desiredSet {
		t.Fatalf("controller after failed listen = cancel:%v addr:%v desiredSet:%v, want retryable idle state", controller.cancel, controller.addr, controller.desiredSet)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}
	if err := controller.apply(ctx, port); err != nil {
		t.Fatalf("retry freed port: %v", err)
	}
	if controller.cancel == nil || controller.addr == nil {
		t.Fatalf("controller after retry = cancel:%v addr:%v, want running server", controller.cancel, controller.addr)
	}
	controller.stop()
}

func TestStateHTTPServerControllerRestartsPreviousPortAfterFailedReload(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	oldPort := freeTCPPort(t)
	blockedListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve blocked port: %v", err)
	}
	defer blockedListener.Close()
	blockedPort := blockedListener.Addr().(*net.TCPAddr).Port

	controller := newStateHTTPServerController(func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, nil
	})
	if err := controller.apply(ctx, oldPort); err != nil {
		t.Fatalf("start old port: %v", err)
	}
	if controller.cancel == nil || controller.addr == nil {
		t.Fatalf("controller after old port start = cancel:%v addr:%v, want running server", controller.cancel, controller.addr)
	}
	if err := controller.apply(ctx, blockedPort); err != nil {
		t.Fatalf("apply blocked port: %v", err)
	}
	if controller.cancel != nil || controller.addr != nil || controller.desiredSet {
		t.Fatalf("controller after blocked reload = cancel:%v addr:%v desiredSet:%v, want retryable idle state", controller.cancel, controller.addr, controller.desiredSet)
	}
	if err := controller.apply(ctx, oldPort); err != nil {
		t.Fatalf("restart old port: %v", err)
	}
	if controller.cancel == nil || controller.addr == nil {
		t.Fatalf("controller after old port restart = cancel:%v addr:%v, want running server", controller.cancel, controller.addr)
	}
	controller.stop()
}

func TestStateHTTPServerControllerRestartsAfterServerExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	close(done)
	controller := newStateHTTPServerController(func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, nil
	})
	controller.desiredSet = true
	controller.desiredPort = 0
	controller.cancel = func() {}
	controller.addr = &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 65535}
	controller.serverDone = done

	if err := controller.apply(ctx, 0); err != nil {
		t.Fatalf("restart after server exit: %v", err)
	}
	if controller.cancel == nil || controller.addr == nil {
		t.Fatalf("controller after restart = cancel:%v addr:%v, want running server", controller.cancel, controller.addr)
	}
	controller.stop()
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve free port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release free port: %v", err)
	}
	return port
}

func TestRunTreatsCanceledPollContextAsGracefulShutdown(t *testing.T) {
	if err := normalizeRunError(context.Canceled, context.Canceled); err != nil {
		t.Fatalf("normalizeRunError(context.Canceled, context.Canceled) = %v, want nil", err)
	}
	if err := normalizeRunError(context.DeadlineExceeded, context.DeadlineExceeded); err != nil {
		t.Fatalf("normalizeRunError(context.DeadlineExceeded, context.DeadlineExceeded) = %v, want nil", err)
	}
	if err := normalizeRunError(os.ErrNotExist, nil); err == nil {
		t.Fatal("normalizeRunError(non-context error) = nil, want original error")
	}
}

func TestRunDoesNotTreatUnrelatedDeadlineErrorAsGracefulShutdown(t *testing.T) {
	err := normalizeRunError(context.DeadlineExceeded, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("normalizeRunError(context.DeadlineExceeded, nil) = %v, want deadline error", err)
	}
}

func TestWorkerEntrypointDoesNotRequirePostgresQueue(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	for _, forbidden := range []string{"internal/queue", "pgxpool", "DATABASE_URL"} {
		if strings.Contains(string(src), forbidden) {
			t.Fatalf("cmd/worker/main.go contains %q; worker startup must use tracker + orchestrator runtime state, not the Postgres queue", forbidden)
		}
	}
	for _, required := range []string{"orchestrator.NewOrchestratorState", "orchestrator.NewWorkflowRuntime", "orchestrator.NewRuntimeDispatcher", "orchestrator.NewRuntimePoller", "orchestrator.RunPollLoopWithRuntime", "orchestrator.RunWorkflowReloadLoop"} {
		if !strings.Contains(string(src), required) {
			t.Fatalf("cmd/worker/main.go missing %q; worker startup must poll tracker issues through dynamically reloaded reconciled orchestrator runtime state", required)
		}
	}
}

func TestLoadWorkflowForStartupReconcileUsesConfiguredWorkflowPath(t *testing.T) {
	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "linear-workflow.md")
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\ntracker:\n  kind: linear\n  project_slug: platform\n  active_states: [\"AI Ready\"]\n  terminal_states: [\"Done\"]\n---\nprompt\n"
	if err := os.WriteFile(workflowPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	t.Setenv("AIOPS_WORKFLOW_PATH", workflowPath)

	var logs bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldOutput)

	wf, err := loadWorkflowForStartupReconcile()
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	if wf.Path != workflowPath {
		t.Fatalf("workflow path = %q, want %q", wf.Path, workflowPath)
	}
	if wf.Config.Tracker.Kind != "linear" {
		t.Fatalf("tracker kind = %q, want linear", wf.Config.Tracker.Kind)
	}
	gotLog := logs.String()
	for _, want := range []string{"startup reconciliation: workflow source=file", "path=" + workflowPath, "tracker.kind=linear"} {
		if !strings.Contains(gotLog, want) {
			t.Fatalf("startup reconciliation log = %q, want substring %q", gotLog, want)
		}
	}
	if strings.Contains(gotLog, "reconciliation will be skipped") {
		t.Fatalf("startup reconciliation log = %q, did not expect skip diagnostic", gotLog)
	}
}

func TestResolveStartupWorkflowUsesPositionalPath(t *testing.T) {
	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "service-WORKFLOW.md")
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\ntracker:\n  kind: linear\n  project_slug: platform\n---\nservice prompt\n"
	if err := os.WriteFile(workflowPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	wf, res, err := resolveStartupWorkflow([]string{workflowPath})
	if err != nil {
		t.Fatalf("resolve startup workflow: %v", err)
	}
	if wf.Path != workflowPath {
		t.Fatalf("workflow path = %q, want %q", wf.Path, workflowPath)
	}
	if res.Source != workflow.SourceFile || res.Path != workflowPath {
		t.Fatalf("resolution = %+v, want file at positional path", res)
	}
}

func TestResolveStartupWorkflowDefaultsToCwdWorkflowOnly(t *testing.T) {
	dir := t.TempDir()
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldwd) })

	if err := os.MkdirAll(filepath.Join(dir, ".aiops"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(dir, ".aiops", "WORKFLOW.md")
	if err := os.WriteFile(legacyPath, []byte("legacy prompt\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	wf, res, err := resolveStartupWorkflow(nil)
	if err != nil {
		t.Fatalf("resolveStartupWorkflow without cwd WORKFLOW.md: %v", err)
	}
	if res.Source != workflow.SourceDefault || res.Path != "" {
		t.Fatalf("resolution = %+v, want built-in default without legacy .aiops path", res)
	}
	if wf.Path != "" || wf.Source != workflow.SourceDefault {
		t.Fatalf("workflow = %+v, want built-in default source", wf)
	}
}

func TestLoadWorkflowForStartupReconcileLogsConfiguredGiteaWorkflow(t *testing.T) {
	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "gitea-workflow.md")
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\ntracker:\n  kind: gitea\n---\nprompt\n"
	if err := os.WriteFile(workflowPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	t.Setenv("AIOPS_WORKFLOW_PATH", workflowPath)

	var logs bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldOutput)

	wf, err := loadWorkflowForStartupReconcile()
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	if wf.Config.Tracker.Kind != "gitea" {
		t.Fatalf("tracker kind = %q, want gitea", wf.Config.Tracker.Kind)
	}
	gotLog := logs.String()
	for _, want := range []string{"startup reconciliation: workflow source=file", "path=" + workflowPath, "tracker.kind=gitea"} {
		if !strings.Contains(gotLog, want) {
			t.Fatalf("startup reconciliation log = %q, want substring %q", gotLog, want)
		}
	}
	if strings.Contains(gotLog, "reconciliation will be skipped") {
		t.Fatalf("startup reconciliation log = %q, did not expect skip diagnostic", gotLog)
	}
}

func TestStartupReconcileConfigUsesEffectiveWorkspaceHooks(t *testing.T) {
	cfg := workflow.DefaultConfig()
	cfg.Hooks = workflow.WorkspaceHooks{
		BeforeRemove: workflow.WorkspaceHook{Commands: []string{"printf top-level"}},
		TimeoutMs:    1234,
	}

	reconcile := startupReconcileConfigForWorkflow(cfg, nil)
	if !reflect.DeepEqual(reconcile.BeforeRemoveHook.Commands, []string{"printf top-level"}) {
		t.Fatalf("BeforeRemoveHook.Commands = %#v, want top-level effective hook", reconcile.BeforeRemoveHook.Commands)
	}
	if reconcile.HookTimeoutMillis != 1234 {
		t.Fatalf("HookTimeoutMillis = %d, want top-level effective timeout", reconcile.HookTimeoutMillis)
	}
}

func TestStartupReconcileConfigHonorsWorkflowWorkspaceRoot(t *testing.T) {
	yamlRoot := filepath.Join(t.TempDir(), "yaml-workspaces")
	workflowPath := writeWorkflowForStartupReconcileTest(t, fmt.Sprintf("workspace:\n  root: %s\n", yamlRoot))
	wf, err := workflow.Load(workflowPath)
	if err != nil {
		t.Fatalf("Load workflow: %v", err)
	}
	t.Setenv("WORKSPACE_ROOT", filepath.Join(t.TempDir(), "env-workspaces"))

	reconcile := startupReconcileConfigForWorkflow(wf.Config, nil)
	if reconcile.WorkspaceRoot != yamlRoot {
		t.Fatalf("WorkspaceRoot = %q, want workflow workspace.root %q", reconcile.WorkspaceRoot, yamlRoot)
	}
}

func TestStartupReconcileConfigFallsBackToEnvWorkspaceRoot(t *testing.T) {
	workflowPath := writeWorkflowForStartupReconcileTest(t, "")
	wf, err := workflow.Load(workflowPath)
	if err != nil {
		t.Fatalf("Load workflow: %v", err)
	}
	envRoot := filepath.Join(t.TempDir(), "env-workspaces")
	t.Setenv("WORKSPACE_ROOT", envRoot)

	reconcile := startupReconcileConfigForWorkflow(wf.Config, nil)
	if reconcile.WorkspaceRoot != envRoot {
		t.Fatalf("WorkspaceRoot = %q, want env workspace root %q", reconcile.WorkspaceRoot, envRoot)
	}
}

func TestStartupReconcileConfigHonorsExplicitDefaultWorkflowWorkspaceRoot(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	yamlRoot := filepath.Join(home, "aiops-workspaces")
	workflowPath := writeWorkflowForStartupReconcileTest(t, fmt.Sprintf("workspace:\n  root: %s\n", yamlRoot))
	wf, err := workflow.Load(workflowPath)
	if err != nil {
		t.Fatalf("Load workflow: %v", err)
	}
	envRoot := filepath.Join(t.TempDir(), "env-workspaces")
	t.Setenv("WORKSPACE_ROOT", envRoot)

	reconcile := startupReconcileConfigForWorkflow(wf.Config, nil)
	if reconcile.WorkspaceRoot != yamlRoot {
		t.Fatalf("WorkspaceRoot = %q, want explicit workflow default root %q", reconcile.WorkspaceRoot, yamlRoot)
	}
}

func writeWorkflowForStartupReconcileTest(t *testing.T, extraFrontMatter string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	body := "---\n" + extraFrontMatter + `repo:
  owner: acme
  name: demo
  clone_url: https://example.invalid/acme/demo.git
  default_branch: main
` + "---\nPrompt body\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	return path
}

func TestStartupReconcileConfigPreservesServiceRoutedActiveWorkspaceKey(t *testing.T) {
	cfg := workflow.DefaultConfig()
	cfg.Tracker.Kind = "linear"
	cfg.Tracker.ProjectSlug = "platform"
	cfg.Services = []workflow.ServiceConfig{
		{
			Name:    "api",
			Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "platform", Labels: []string{"api"}},
		},
	}

	reconcile := startupReconcileConfigForWorkflow(cfg, nil)
	if reconcile.ActiveWorkspaceKeys == nil {
		t.Fatal("ActiveWorkspaceKeys is nil; startup reconciliation will not recognize service-routed workspace keys")
	}

	keys := reconcile.ActiveWorkspaceKeys(tracker.Issue{
		ID:          "abc-123",
		Identifier:  "ENG-1",
		State:       "Rework",
		ProjectSlug: "platform",
		Labels:      []string{"api"},
		UpdatedAt:   mustTime("2026-05-19T03:00:00Z"),
	})
	for _, want := range []string{"abc-123-service-api", "abc-123-service-api-rework-2026-05-19t03-00-00z"} {
		if !containsString(keys, want) {
			t.Fatalf("active workspace keys = %#v, want %s", keys, want)
		}
	}
}

func TestStartupReconcileKeepsServiceRoutedReworkWorkspaceAfterUpdatedAtChanges(t *testing.T) {
	root := t.TempDir()
	activePath := filepath.Join(root, "acme", "api", "linear_issue", "abc-123-service-api-rework-2026-05-18t03-00-00z")
	terminalPath := filepath.Join(root, "acme", "api", "linear_issue", "done-1")
	for _, path := range []string{activePath, terminalPath} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
	}
	cfg := workflow.DefaultConfig()
	cfg.Tracker.Kind = "linear"
	cfg.Tracker.ActiveStates = []string{"Rework"}
	cfg.Tracker.TerminalStates = []string{"Done"}
	cfg.Tracker.ProjectSlug = "platform"
	cfg.Services = []workflow.ServiceConfig{{
		Name:    "api",
		Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "platform", Labels: []string{"api"}},
	}}
	reconcile := startupReconcileConfigForWorkflow(cfg, fakeReconcileTracker{issues: []tracker.Issue{
		{ID: "abc-123", Identifier: "ENG-1", State: "Rework", ProjectSlug: "platform", Labels: []string{"api"}, UpdatedAt: mustTime("2026-05-19T03:00:00Z")},
		{ID: "done-1", Identifier: "DONE-1", State: "Done"},
	}})
	reconcile.WorkspaceRoot = root

	if err := worker.ReconcileStartup(context.Background(), reconcile); err != nil {
		t.Fatalf("ReconcileStartup: %v", err)
	}
	if _, err := os.Stat(activePath); err != nil {
		t.Fatalf("active service Rework workspace should remain after updatedAt changes: %v", err)
	}
	if _, err := os.Stat(terminalPath); !os.IsNotExist(err) {
		t.Fatalf("terminal workspace should be removed, stat err=%v", err)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestTrackerClientForWorkflowBuildsMultiProjectLinearClientForServiceRoutes(t *testing.T) {
	cfg := workflow.DefaultConfig()
	cfg.Tracker.Kind = "linear"
	cfg.Tracker.ProjectSlug = ""
	cfg.Services = []workflow.ServiceConfig{
		{Name: "api", Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "api-platform"}},
		{Name: "web", Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "web-platform"}},
	}

	client, err := trackerClientForWorkflow(cfg)
	if err != nil {
		t.Fatalf("tracker client: %v", err)
	}

	multi, ok := client.(interface{ Trackers() []trackerRuntimeClient })
	if !ok {
		t.Fatalf("client type = %T, want multi-project tracker", client)
	}
	got := multi.Trackers()
	if len(got) != 2 {
		t.Fatalf("linear tracker count = %d, want 2 service projects", len(got))
	}
	projects := make([]string, 0, len(got))
	for _, client := range got {
		linearClient, ok := client.(*tracker.LinearClient)
		if !ok {
			t.Fatalf("linear tracker type = %T, want *tracker.LinearClient", client)
		}
		projects = append(projects, linearClient.Config.ProjectSlug)
	}
	if !reflect.DeepEqual(projects, []string{"api-platform", "web-platform"}) {
		t.Fatalf("linear tracker projects = %#v, want service projects", projects)
	}
}

func TestTrackerClientForWorkflowUsesGiteaProjectSlugBeforeEnvBaseURL(t *testing.T) {
	t.Setenv("GITEA_BASE_URL", "https://gitea-env.example.test/")
	cfg := workflow.DefaultConfig()
	cfg.Tracker.Kind = "gitea"
	cfg.Tracker.ProjectSlug = "https://gitea-workflow.example.test/"
	cfg.Repo.Owner = "owner"
	cfg.Repo.Name = "repo"

	client, err := trackerClientForWorkflow(cfg)
	if err != nil {
		t.Fatalf("tracker client: %v", err)
	}
	giteaClient, ok := client.(*gitea.TrackerClient)
	if !ok {
		t.Fatalf("client type = %T, want *gitea.TrackerClient", client)
	}
	if giteaClient.BaseURL != "https://gitea-workflow.example.test" {
		t.Fatalf("base URL = %q, want tracker.project_slug without trailing slash", giteaClient.BaseURL)
	}
}

func TestValidateWorkflowForRuntimeRejectsPromptOnlyWorkflowMissingTaskFields(t *testing.T) {
	cfg := workflow.DefaultConfig()

	err := validateWorkflowForRuntime("WORKFLOW.md", workflow.SourcePromptOnly, cfg)
	if err == nil {
		t.Fatal("validateWorkflowForRuntime(prompt-only defaults) = nil, want repo.clone_url error")
	}
	for _, want := range []string{"WORKFLOW.md", "repo.clone_url", "poll-based worker runtime"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("validateWorkflowForRuntime error = %v, want substring %q", err, want)
		}
	}
}

func TestValidateWorkflowForRuntimeRejectsDefaultWorkflowMissingTaskFields(t *testing.T) {
	cfg := workflow.DefaultConfig()

	err := validateWorkflowForRuntime("WORKFLOW.md", workflow.SourceDefault, cfg)
	if err == nil {
		t.Fatal("validateWorkflowForRuntime(default workflow) = nil, want repo.clone_url error")
	}
	for _, want := range []string{"built-in workflow defaults", "repo.clone_url", "poll-based worker runtime"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("validateWorkflowForRuntime error = %v, want substring %q", err, want)
		}
	}
}

func TestValidateWorkflowForRuntimeAcceptsConfiguredRepo(t *testing.T) {
	cfg := workflow.DefaultConfig()
	cfg.Repo.CloneURL = "git@example.com:o/r.git"

	for _, source := range []workflow.Source{workflow.SourceFile, workflow.SourcePromptOnly, workflow.SourceDefault} {
		if err := validateWorkflowForRuntime("WORKFLOW.md", source, cfg); err != nil {
			t.Fatalf("validateWorkflowForRuntime(source=%s) = %v, want nil", source, err)
		}
	}
}

func TestValidateWorkflowForRuntimeAcceptsServiceOnlyRepos(t *testing.T) {
	cfg := workflow.DefaultConfig()
	cfg.Services = []workflow.ServiceConfig{
		{Name: "api", Repo: workflow.RepoConfig{CloneURL: "git@example.com:o/api.git"}},
	}

	if err := validateWorkflowForRuntime("WORKFLOW.md", workflow.SourceFile, cfg); err != nil {
		t.Fatalf("validateWorkflowForRuntime(service-only repos) = %v, want nil", err)
	}
}

func TestWorkerReconciliationConfigIncludesInactiveStates(t *testing.T) {
	cfg := workflow.DefaultConfig()
	cfg.Repo.CloneURL = "git@example.com:o/r.git"
	cfg.Tracker.Kind = "linear"
	cfg.Tracker.ActiveStates = []string{"AI Ready", "In Progress", "Rework"}
	cfg.Tracker.TerminalStates = []string{"Done", "Canceled"}

	reconcile := reconciliationConfigForWorkflow(cfg)
	if len(reconcile.InactiveStates) == 0 {
		t.Fatalf("inactive reconciliation states = %v, want non-empty states for explicit inactive tracker observations", reconcile.InactiveStates)
	}
	if containsState(reconcile.InactiveStates, "AI Ready") || containsState(reconcile.InactiveStates, "In Progress") || containsState(reconcile.InactiveStates, "Rework") {
		t.Fatalf("inactive reconciliation states = %v, must not include configured active states", reconcile.InactiveStates)
	}
	if containsState(reconcile.InactiveStates, "Done") || containsState(reconcile.InactiveStates, "Canceled") {
		t.Fatalf("inactive reconciliation states = %v, must not duplicate terminal states", reconcile.InactiveStates)
	}
	if !containsState(reconcile.InactiveStates, "Backlog") || !containsState(reconcile.InactiveStates, "Human Review") {
		t.Fatalf("inactive reconciliation states = %v, want Backlog and Human Review", reconcile.InactiveStates)
	}
}

func TestTrackerClientForWorkflowBuildsGitHubClient(t *testing.T) {
	cfg := workflow.DefaultConfig()
	cfg.Repo.Owner = "xrf9268-hue"
	cfg.Repo.Name = "aiops-platform"
	cfg.Tracker.Kind = "github"
	cfg.Tracker.APIKey = "github-token"
	cfg.Tracker.BaseURL = "https://api.github.test"

	client, err := trackerClientForWorkflow(cfg)
	if err != nil {
		t.Fatalf("trackerClientForWorkflow: %v", err)
	}
	githubClient, ok := client.(*tracker.GitHubClient)
	if !ok {
		t.Fatalf("client type = %T, want *tracker.GitHubClient", client)
	}
	if githubClient.Owner != "xrf9268-hue" || githubClient.Repo != "aiops-platform" {
		t.Fatalf("github repo = %s/%s", githubClient.Owner, githubClient.Repo)
	}
	if githubClient.BaseURL != "https://api.github.test" {
		t.Fatalf("github base URL = %q", githubClient.BaseURL)
	}
}

func TestWorkerReconciliationConfigDoesNotProbeUnmappedGiteaInactiveStates(t *testing.T) {
	cfg := workflow.DefaultConfig()
	cfg.Repo.CloneURL = "git@example.com:o/r.git"
	cfg.Tracker.Kind = "gitea"
	cfg.Tracker.ActiveStates = []string{"AI Ready", "In Progress", "Rework"}
	cfg.Tracker.TerminalStates = []string{"Done", "Canceled"}

	reconcile := reconciliationConfigForWorkflow(cfg)
	if containsState(reconcile.InactiveStates, "Backlog") {
		t.Fatalf("inactive reconciliation states = %v, must not include unmapped Gitea Backlog state", reconcile.InactiveStates)
	}
	if !containsState(reconcile.InactiveStates, "Human Review") {
		t.Fatalf("inactive reconciliation states = %v, want mapped Gitea Human Review state", reconcile.InactiveStates)
	}
}

func TestWorkerReconciliationConfigUsesWorkflowInactiveStates(t *testing.T) {
	cfg := workflow.DefaultConfig()
	cfg.Repo.CloneURL = "git@example.com:o/r.git"
	cfg.Tracker.ActiveStates = []string{"AI Ready", "In Progress"}
	cfg.Tracker.TerminalStates = []string{"Done", "Canceled"}
	cfg.Tracker.InactiveStates = []string{"Paused", "Blocked", "Done", "AI Ready"}

	reconcile := reconciliationConfigForWorkflow(cfg)
	if !containsState(reconcile.InactiveStates, "Paused") || !containsState(reconcile.InactiveStates, "Blocked") {
		t.Fatalf("inactive reconciliation states = %v, want workflow-configured inactive states", reconcile.InactiveStates)
	}
	if containsState(reconcile.InactiveStates, "Done") || containsState(reconcile.InactiveStates, "AI Ready") {
		t.Fatalf("inactive reconciliation states = %v, must exclude configured active/terminal states", reconcile.InactiveStates)
	}
}

func containsState(states []string, want string) bool {
	for _, state := range states {
		if state == want {
			return true
		}
	}
	return false
}

func TestValidateWorkflowForRuntimeRejectsFrontMatterWorkflowMissingTaskFields(t *testing.T) {
	cfg := workflow.DefaultConfig()

	err := validateWorkflowForRuntime("WORKFLOW.md", workflow.SourceFile, cfg)
	if err == nil {
		t.Fatal("validateWorkflowForRuntime(file source defaults) = nil, want repo.clone_url error")
	}
	for _, want := range []string{"WORKFLOW.md", "repo.clone_url", "poll-based worker runtime"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("validateWorkflowForRuntime error = %v, want substring %q", err, want)
		}
	}
}

func TestLoadWorkflowForStartupReconcileClassifiesConfiguredPromptOnlyWorkflow(t *testing.T) {
	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "prompt-only-workflow.md")
	body := "Follow the repository workflow without YAML front matter.\n"
	if err := os.WriteFile(workflowPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	t.Setenv("AIOPS_WORKFLOW_PATH", workflowPath)

	var logs bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldOutput)

	wf, err := loadWorkflowForStartupReconcile()
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	if wf.Config.Tracker.Kind != workflow.DefaultConfig().Tracker.Kind {
		t.Fatalf("tracker kind = %q, want default %q", wf.Config.Tracker.Kind, workflow.DefaultConfig().Tracker.Kind)
	}
	gotLog := logs.String()
	for _, want := range []string{"startup reconciliation: workflow source=prompt_only", "path=" + workflowPath, "tracker.kind=gitea"} {
		if !strings.Contains(gotLog, want) {
			t.Fatalf("startup reconciliation log = %q, want substring %q", gotLog, want)
		}
	}
	for _, forbidden := range []string{"workflow source=file", "reconciliation will be skipped"} {
		if strings.Contains(gotLog, forbidden) {
			t.Fatalf("startup reconciliation log = %q, did not expect %q", gotLog, forbidden)
		}
	}
}

func TestLoadWorkflowForStartupReconcileResolvesCWDWorkflowAndLogsSource(t *testing.T) {
	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\ntracker:\n  kind: linear\n  project_slug: platform\n---\nprompt\n"
	if err := os.WriteFile(workflowPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	resolvedPath, err := filepath.EvalSymlinks(filepath.Join(dir, "WORKFLOW.md"))
	if err != nil {
		resolvedPath = filepath.Join(dir, "WORKFLOW.md")
	}
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() {
		if err := os.Chdir(oldwd); err != nil {
			t.Fatalf("restore wd: %v", err)
		}
	}()

	var logs bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldOutput)

	wf, err := loadWorkflowForStartupReconcile()
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	if wf.Path != resolvedPath {
		t.Fatalf("workflow path = %q, want %q", wf.Path, resolvedPath)
	}
	if wf.Config.Tracker.Kind != "linear" {
		t.Fatalf("tracker kind = %q, want linear", wf.Config.Tracker.Kind)
	}
	gotLog := logs.String()
	for _, want := range []string{"startup reconciliation: workflow source=file", "path=" + resolvedPath, "tracker.kind=linear"} {
		if !strings.Contains(gotLog, want) {
			t.Fatalf("startup reconciliation log = %q, want substring %q", gotLog, want)
		}
	}
}

func TestLoadWorkflowForStartupReconcileDefaultsWhenNoWorkflowExists(t *testing.T) {
	dir := t.TempDir()
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() {
		if err := os.Chdir(oldwd); err != nil {
			t.Fatalf("restore wd: %v", err)
		}
	}()

	var logs bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldOutput)

	wf, err := loadWorkflowForStartupReconcile()
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	if wf.Config.Tracker.Kind != workflow.DefaultConfig().Tracker.Kind {
		t.Fatalf("tracker kind = %q, want default %q", wf.Config.Tracker.Kind, workflow.DefaultConfig().Tracker.Kind)
	}
	gotLog := logs.String()
	for _, want := range []string{"startup reconciliation: workflow source=default", "tracker.kind=gitea"} {
		if !strings.Contains(gotLog, want) {
			t.Fatalf("startup reconciliation log = %q, want substring %q", gotLog, want)
		}
	}
	if strings.Contains(gotLog, "reconciliation will be skipped") {
		t.Fatalf("startup reconciliation log = %q, did not expect skip diagnostic", gotLog)
	}
}

func mustTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		panic(err)
	}
	return parsed
}

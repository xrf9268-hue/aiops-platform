package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/gitea"
	"github.com/xrf9268-hue/aiops-platform/internal/orchestrator"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func newLoopbackRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.RemoteAddr = "127.0.0.1:54321"
	return req
}

// TestApiRunningRowAlwaysEmitsSpec13_7_2StatusKeys pins the contract from
// SPEC §13.7.2: state / session_id / turn_count / last_event / last_message
// are required keys on each running row, so consumers can distinguish
// "known zero/empty" from "field missing". A freshly-dispatched row with
// zero values must still emit all five keys.
func TestApiRunningRowAlwaysEmitsSpec13_7_2StatusKeys(t *testing.T) {
	row := apiRunningFromView(orchestrator.RunningView{IssueID: "issue-1", Identifier: "ENG-1"})
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"state", "session_id", "turn_count", "last_event", "last_message", "tokens"} {
		if _, ok := got[key]; !ok {
			t.Errorf("stateapi.Running JSON missing %q for a freshly-dispatched run: %s", key, raw)
		}
	}
}

// TestApiStateRowsEmitIssueURLWhenAvailable pins SPEC §13.7's SHOULD-level
// `issue_url`: running/retrying/blocked rows must surface the tracker-provided
// URL when present and omit the key (omitempty) when absent. The URL is already
// carried in *Entry.Issue.URL; this is the projection layer the state API was
// missing. Mutation: dropping IssueURL from any DTO/builder fails the matching
// "present" case; dropping omitempty fails the matching "absent" case.
func TestApiStateRowsEmitIssueURLWhenAvailable(t *testing.T) {
	const url = "https://tracker.example/issues/MT-649"
	keyPresent := func(t *testing.T, raw []byte, want string) {
		t.Helper()
		var got map[string]json.RawMessage
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		v, ok := got["issue_url"]
		if !ok {
			t.Fatalf("issue_url missing; want %q in %s", want, raw)
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			t.Fatalf("issue_url unmarshal: %v", err)
		}
		if s != want {
			t.Fatalf("issue_url = %q; want %q", s, want)
		}
	}
	keyAbsent := func(t *testing.T, raw []byte) {
		t.Helper()
		var got map[string]json.RawMessage
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if _, ok := got["issue_url"]; ok {
			t.Fatalf("issue_url present; want omitted (omitempty) in %s", raw)
		}
	}
	rows := map[string]struct {
		withURL any
		noURL   any
	}{
		"running": {
			withURL: apiRunningFromView(orchestrator.RunningView{IssueID: "i1", Identifier: "MT-649", IssueURL: url}),
			noURL:   apiRunningFromView(orchestrator.RunningView{IssueID: "i1", Identifier: "MT-649"}),
		},
		"retry": {
			withURL: apiRetryFromView(orchestrator.RetryView{IssueID: "i1", Identifier: "MT-649", IssueURL: url}),
			noURL:   apiRetryFromView(orchestrator.RetryView{IssueID: "i1", Identifier: "MT-649"}),
		},
	}
	for name, tc := range rows {
		t.Run(name+" present", func(t *testing.T) {
			raw, err := json.Marshal(tc.withURL)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			keyPresent(t, raw, url)
		})
		t.Run(name+" absent", func(t *testing.T) {
			raw, err := json.Marshal(tc.noURL)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			keyAbsent(t, raw)
		})
	}

	// Blocked rows are projected through apiStateFromView, not a standalone
	// builder, so exercise that path.
	resp := apiStateFromView(orchestrator.StateView{
		Blocked: []orchestrator.BlockedView{{IssueID: "i1", Identifier: "MT-649", IssueURL: url}},
	})
	rawBlocked, err := json.Marshal(resp.Blocked[0])
	if err != nil {
		t.Fatalf("marshal blocked: %v", err)
	}
	keyPresent(t, rawBlocked, url)
	respNo := apiStateFromView(orchestrator.StateView{
		Blocked: []orchestrator.BlockedView{{IssueID: "i1", Identifier: "MT-649"}},
	})
	rawBlockedNo, err := json.Marshal(respNo.Blocked[0])
	if err != nil {
		t.Fatalf("marshal blocked no-url: %v", err)
	}
	keyAbsent(t, rawBlockedNo)
}

// TestApiStateSurfacesAgentModelMetadata pins the #977 projection seam: a
// running row carries agent_provider/agent_model when the runner reported them
// and omits the keys (omitempty) otherwise, and the top-level worker default
// surfaces as agent_default. Mutation: dropping a field in apiRunningFromView /
// apiStateFromView fails the "present" case; dropping omitempty fails "absent".
func TestApiStateSurfacesAgentModelMetadata(t *testing.T) {
	stringKey := func(t *testing.T, raw []byte, key, want string) {
		t.Helper()
		var got map[string]json.RawMessage
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		v, ok := got[key]
		if want == "" {
			if ok {
				t.Fatalf("%s present; want omitted (omitempty) in %s", key, raw)
			}
			return
		}
		if !ok {
			t.Fatalf("%s missing; want %q in %s", key, want, raw)
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			t.Fatalf("%s unmarshal: %v", key, err)
		}
		if s != want {
			t.Fatalf("%s = %q; want %q", key, s, want)
		}
	}

	withModel, err := json.Marshal(apiRunningFromView(orchestrator.RunningView{
		IssueID: "i1", Identifier: "MT-649",
		AgentProvider: "codex-app-server", AgentModel: "gpt-5.3-codex-spark",
	}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	stringKey(t, withModel, "agent_provider", "codex-app-server")
	stringKey(t, withModel, "agent_model", "gpt-5.3-codex-spark")

	noModel, err := json.Marshal(apiRunningFromView(orchestrator.RunningView{IssueID: "i1", Identifier: "MT-649"}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	stringKey(t, noModel, "agent_provider", "")
	stringKey(t, noModel, "agent_model", "")

	withDefault, err := json.Marshal(apiStateFromView(orchestrator.StateView{AgentDefault: "codex-app-server"}))
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	stringKey(t, withDefault, "agent_default", "codex-app-server")

	noDefault, err := json.Marshal(apiStateFromView(orchestrator.StateView{}))
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	stringKey(t, noDefault, "agent_default", "")
}

// TestApiStateSurfacesWorkflowProfileMetadata pins the #983 projection seam: a
// running row carries workflow_source/workflow_path when the worker reported
// them and omits each key (omitempty) otherwise — a default-workflow run keeps
// workflow_source=default with no workflow_path. Mutation: dropping a field in
// apiRunningFromView fails the "present" case; dropping omitempty fails "absent".
func TestApiStateSurfacesWorkflowProfileMetadata(t *testing.T) {
	stringKey := func(t *testing.T, raw []byte, key, want string) {
		t.Helper()
		var got map[string]json.RawMessage
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		v, ok := got[key]
		if want == "" {
			if ok {
				t.Fatalf("%s present; want omitted (omitempty) in %s", key, raw)
			}
			return
		}
		if !ok {
			t.Fatalf("%s missing; want %q in %s", key, want, raw)
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			t.Fatalf("%s unmarshal: %v", key, err)
		}
		if s != want {
			t.Fatalf("%s = %q; want %q", key, s, want)
		}
	}

	withFile, err := json.Marshal(apiRunningFromView(orchestrator.RunningView{
		IssueID: "i1", Identifier: "MT-983",
		WorkflowSource: "file", WorkflowPath: "/srv/reviewer/WORKFLOW.md",
	}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	stringKey(t, withFile, "workflow_source", "file")
	stringKey(t, withFile, "workflow_path", "/srv/reviewer/WORKFLOW.md")

	// Default workflow: source=default with no resolved file path.
	withDefault, err := json.Marshal(apiRunningFromView(orchestrator.RunningView{
		IssueID: "i1", Identifier: "MT-983", WorkflowSource: "default",
	}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	stringKey(t, withDefault, "workflow_source", "default")
	stringKey(t, withDefault, "workflow_path", "")

	none, err := json.Marshal(apiRunningFromView(orchestrator.RunningView{IssueID: "i1", Identifier: "MT-983"}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	stringKey(t, none, "workflow_source", "")
	stringKey(t, none, "workflow_path", "")
}

func TestApiIssueFromViewFindsRecentTerminalEventByIdentifier(t *testing.T) {
	view := orchestrator.StateView{
		Completed: []orchestrator.IssueID{"linear-global-id"},
		RecentEvents: []orchestrator.RuntimeEvent{{
			Kind:       orchestrator.RuntimeEventCompleted,
			IssueID:    "linear-global-id",
			Identifier: "ENG-1",
			Message:    "worker exited cleanly",
			At:         time.Unix(1700000000, 0).UTC(),
		}},
	}
	got, ok := apiIssueFromView(view, "ENG-1")
	if !ok {
		t.Fatal("apiIssueFromView(ENG-1) ok = false; want true")
	}
	if got.Status != "completed" || got.IssueID != "linear-global-id" {
		t.Fatalf("terminal issue = (%s, %s); want (completed, linear-global-id)", got.Status, got.IssueID)
	}
	if len(got.RecentEvents) != 1 {
		t.Fatalf("recent event count = %d; want 1", len(got.RecentEvents))
	}
}

// TestApiIssueFromViewFindsReconcileStoppedByIdentifierAndID pins the per-issue
// lookup consistency: the bucket stores the global id, but the recorded runtime
// event carries the human identifier, so BOTH /api/v1/<identifier> and
// /api/v1/<global-id> resolve a reconcile-stopped run instead of issue_not_found
// (#557). The empty-identifier bucket alone could only match the global id.
func TestApiIssueFromViewFindsReconcileStoppedByIdentifierAndID(t *testing.T) {
	view := orchestrator.StateView{
		ReconcileStoppedWithProgress: []orchestrator.IssueID{"linear-uuid-51"},
		RecentEvents: []orchestrator.RuntimeEvent{{
			Kind:       orchestrator.RuntimeEventReconcileStopped,
			IssueID:    "linear-uuid-51",
			Identifier: "AIS-51",
			Message:    "reconcile stopped run after ≥1 completed turn",
			At:         time.Unix(1700000000, 0).UTC(),
		}},
	}
	for _, want := range []string{"AIS-51", "linear-uuid-51"} {
		got, ok := apiIssueFromView(view, want)
		if !ok {
			t.Fatalf("apiIssueFromView(%q) ok = false; want true", want)
		}
		if got.Status != "reconcile_stopped_with_progress" || got.IssueID != "linear-uuid-51" {
			t.Fatalf("apiIssueFromView(%q) = (%s, %s); want (reconcile_stopped_with_progress, linear-uuid-51)", want, got.Status, got.IssueID)
		}
	}
}

// TestApiIssueFromViewResolvesReconcileStoppedFromBucketWhenEventAgedOut: once the
// runtime event rotates out of RecentEvents, the global id is still drillable via
// the reconcile_stopped_with_progress bucket fallback (mirroring completed/failed).
func TestApiIssueFromViewResolvesReconcileStoppedFromBucketWhenEventAgedOut(t *testing.T) {
	view := orchestrator.StateView{
		ReconcileStoppedWithProgress: []orchestrator.IssueID{"linear-uuid-51"},
	}
	got, ok := apiIssueFromView(view, "linear-uuid-51")
	if !ok || got.Status != "reconcile_stopped_with_progress" {
		t.Fatalf("apiIssueFromView(linear-uuid-51) = (%v, %s); want (true, reconcile_stopped_with_progress)", ok, got.Status)
	}
}

func TestApiIssueFromViewFindsAgentHandoffReconcileStoppedByIdentifierAndID(t *testing.T) {
	view := orchestrator.StateView{
		AgentHandoffReconcileStopped: []orchestrator.IssueID{"linear-uuid-62"},
		RecentEvents: []orchestrator.RuntimeEvent{{
			Kind:       orchestrator.RuntimeEventAgentHandoffReconcileStopped,
			IssueID:    "linear-uuid-62",
			Identifier: "AIS-62",
			Message:    "reconcile stopped run after agent-side current-issue state handoff",
			At:         time.Unix(1700000000, 0).UTC(),
		}},
	}
	for _, want := range []string{"AIS-62", "linear-uuid-62"} {
		got, ok := apiIssueFromView(view, want)
		if !ok {
			t.Fatalf("apiIssueFromView(%q) ok = false; want true", want)
		}
		if got.Status != "agent_handoff_reconcile_stopped" || got.IssueID != "linear-uuid-62" {
			t.Fatalf("apiIssueFromView(%q) = (%s, %s); want (agent_handoff_reconcile_stopped, linear-uuid-62)", want, got.Status, got.IssueID)
		}
	}
}

func TestApiIssueFromViewResolvesAgentHandoffReconcileStoppedFromBucketWhenEventAgedOut(t *testing.T) {
	view := orchestrator.StateView{
		AgentHandoffReconcileStopped: []orchestrator.IssueID{"linear-uuid-62"},
	}
	got, ok := apiIssueFromView(view, "linear-uuid-62")
	if !ok || got.Status != "agent_handoff_reconcile_stopped" {
		t.Fatalf("apiIssueFromView(linear-uuid-62) = (%v, %s); want (true, agent_handoff_reconcile_stopped)", ok, got.Status)
	}
}

func TestApiIssueFromViewFindsActiveSuccessNoHandoffByIdentifierAndID(t *testing.T) {
	view := orchestrator.StateView{
		ActiveSuccessNoHandoff: []orchestrator.IssueID{"gitea-issue-12"},
		RecentEvents: []orchestrator.RuntimeEvent{{
			Kind:       orchestrator.RuntimeEventActiveSuccessNoHandoff,
			IssueID:    "gitea-issue-12",
			Identifier: "#12",
			Message:    "worker exited cleanly while issue remained active with no agent handoff",
			At:         time.Unix(1700000000, 0).UTC(),
		}},
	}
	for _, want := range []string{"#12", "gitea-issue-12"} {
		got, ok := apiIssueFromView(view, want)
		if !ok {
			t.Fatalf("apiIssueFromView(%q) ok = false; want true", want)
		}
		if got.Status != "active_success_no_handoff" || got.IssueID != "gitea-issue-12" {
			t.Fatalf("apiIssueFromView(%q) = (%s, %s); want (active_success_no_handoff, gitea-issue-12)", want, got.Status, got.IssueID)
		}
	}
}

func TestApiIssueFromViewResolvesActiveSuccessNoHandoffFromBucketWhenEventAgedOut(t *testing.T) {
	view := orchestrator.StateView{
		Completed:              []orchestrator.IssueID{"gitea-issue-12"},
		ActiveSuccessNoHandoff: []orchestrator.IssueID{"gitea-issue-12"},
	}
	got, ok := apiIssueFromView(view, "gitea-issue-12")
	if !ok || got.Status != "active_success_no_handoff" {
		t.Fatalf("apiIssueFromView(gitea-issue-12) = (%v, %s); want (true, active_success_no_handoff)", ok, got.Status)
	}
}

func TestApiIssueFromViewPrefersAgentHandoffWhenBucketsOverlap(t *testing.T) {
	view := orchestrator.StateView{
		ReconcileStoppedWithProgress: []orchestrator.IssueID{"linear-uuid-63"},
		AgentHandoffReconcileStopped: []orchestrator.IssueID{"linear-uuid-63"},
	}
	got, ok := apiIssueFromView(view, "linear-uuid-63")
	if !ok || got.Status != "agent_handoff_reconcile_stopped" {
		t.Fatalf("apiIssueFromView(linear-uuid-63) = (%v, %s); want (true, agent_handoff_reconcile_stopped)", ok, got.Status)
	}
}

func TestStateResponseSurfacesOperatorTerminalStops(t *testing.T) {
	stoppedAt := time.Date(2026, 6, 4, 7, 0, 0, 0, time.UTC)
	firstSuppressedAt := stoppedAt.Add(time.Minute)
	view := orchestrator.StateView{
		OperatorTerminalStops: []orchestrator.OperatorTerminalStopView{{
			IssueID:               "linear-uuid-70",
			Identifier:            "AIS-70",
			State:                 "Canceled",
			StoppedAt:             stoppedAt,
			SuppressedDispatches:  2,
			FirstSuppressedAt:     firstSuppressedAt,
			FirstSuppressedState:  "In Progress",
			FirstSuppressedReason: "active_candidate_after_operator_terminal_stop",
		}},
		// The lifetime total exceeds the one published (bounded) row: this models a
		// worker that has rotated past the #667 cap, so the API must expose the
		// surviving cumulative count, not just the capped length.
		CumulativeOperatorTerminalStopsTotal: 1500,
	}
	resp := apiStateFromView(view)
	if resp.Counts.OperatorTerminalStops != 1 {
		t.Fatalf("operator terminal stop count = %d, want 1", resp.Counts.OperatorTerminalStops)
	}
	if resp.Counts.OperatorTerminalStopsTotal != 1500 {
		t.Fatalf("operator_terminal_stops_total = %d, want 1500 (lifetime total must survive cap eviction)", resp.Counts.OperatorTerminalStopsTotal)
	}
	if len(resp.OperatorTerminalStops) != 1 {
		t.Fatalf("operator_terminal_stops = %+v, want one row", resp.OperatorTerminalStops)
	}
	row := resp.OperatorTerminalStops[0]
	if row.IssueID != "linear-uuid-70" || row.Identifier != "AIS-70" || row.State != "Canceled" {
		t.Fatalf("operator terminal stop row = %+v, want AIS-70 Canceled", row)
	}
	if row.StoppedAt == nil || !row.StoppedAt.Equal(stoppedAt) {
		t.Fatalf("stopped_at = %v, want %s", row.StoppedAt, stoppedAt)
	}
	if row.FirstSuppressedAt == nil || !row.FirstSuppressedAt.Equal(firstSuppressedAt) || row.SuppressedDispatches != 2 {
		t.Fatalf("suppression evidence = %+v, want first suppression time and count 2", row)
	}
	if row.FirstSuppressedState != "In Progress" || row.FirstSuppressedReason != "active_candidate_after_operator_terminal_stop" {
		t.Fatalf("first suppression fields = %+v, want state/reason evidence", row)
	}
	got, ok := apiIssueFromView(view, "AIS-70")
	if !ok || got.Status != "operator_terminal_stop" || got.IssueID != "linear-uuid-70" {
		t.Fatalf("apiIssueFromView(AIS-70) = (%v, %s, %s), want operator_terminal_stop linear-uuid-70", ok, got.Status, got.IssueID)
	}
}

func TestApiIssueFromViewPrefersOperatorTerminalStopLatch(t *testing.T) {
	view := orchestrator.StateView{
		Completed: []orchestrator.IssueID{"linear-uuid-70"},
		OperatorTerminalStops: []orchestrator.OperatorTerminalStopView{{
			IssueID:    "linear-uuid-70",
			Identifier: "AIS-70",
			State:      "Canceled",
			StoppedAt:  time.Date(2026, 6, 4, 7, 0, 0, 0, time.UTC),
		}},
	}
	got, ok := apiIssueFromView(view, "linear-uuid-70")
	if !ok || got.Status != "operator_terminal_stop" {
		t.Fatalf("apiIssueFromView(linear-uuid-70) = (%v, %s); want (true, operator_terminal_stop)", ok, got.Status)
	}
}

// TestApiRunningRowEmitsLastEventAt pins the SPEC §13.7.2 running-row
// contract: once a runtime event has been observed the row exposes
// last_event_at as an RFC3339 string; no back-compat alias is emitted.
func TestApiRunningRowEmitsLastEventAt(t *testing.T) {
	lastEvent := time.Date(2026, 5, 20, 9, 5, 30, 0, time.UTC)
	row := apiRunningFromView(orchestrator.RunningView{
		IssueID:     "issue-1",
		Identifier:  "ENG-1",
		LastEventAt: lastEvent,
	})
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	lastEventAt, ok := got["last_event_at"].(string)
	if !ok {
		t.Fatalf("last_event_at must be present once an event is observed: %s", raw)
	}
	if _, exists := got["last_codex_at"]; exists {
		t.Fatalf("last_codex_at must not be emitted (removed in #342): %s", raw)
	}
	if want := lastEvent.Format(time.RFC3339); lastEventAt != want {
		t.Fatalf("last_event_at = %q, want %q (RFC3339 of source)", lastEventAt, want)
	}
}

// TestStateResponseSurfacesObservedRateLimits pins the other half of #328:
// once a rate_limit_updated payload has been recorded, /api/v1/state surfaces
// it verbatim under the top-level rate_limits key.
func TestStateResponseSurfacesObservedRateLimits(t *testing.T) {
	snap := orchestrator.RateLimitSnapshot{"primary": map[string]any{"remaining": float64(42)}}
	resp := apiStateFromView(orchestrator.StateView{CodexRateLimits: &snap})
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rateLimits, ok := got["rate_limits"].(map[string]any)
	if !ok {
		t.Fatalf("rate_limits = %#v, want observed payload object", got["rate_limits"])
	}
	primary, ok := rateLimits["primary"].(map[string]any)
	if !ok || primary["remaining"] != float64(42) {
		t.Fatalf("rate_limits = %#v, want primary.remaining surfaced verbatim", rateLimits)
	}
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
			StartupFailure: &task.StartupFailure{
				Phase: "thread/start",
				Error: "codex app-server read timeout after 5000ms",
			},
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
		Completed:                             []orchestrator.IssueID{"issue-9", "issue-3"},
		ActiveSuccessNoHandoff:                []orchestrator.IssueID{"issue-12"},
		CumulativeActiveSuccessNoHandoffTotal: 6,
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
			Running                     int   `json:"running"`
			Retrying                    int   `json:"retrying"`
			Blocked                     int   `json:"blocked"`
			ActiveSuccessNoHandoff      int   `json:"active_success_no_handoff"`
			ActiveSuccessNoHandoffTotal int64 `json:"active_success_no_handoff_total"`
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
			Kind            string `json:"kind"`
			StartupFailure  *struct {
				Phase string `json:"phase"`
				Error string `json:"error"`
			} `json:"startup_failure"`
		} `json:"retrying"`
		Completed              []string `json:"completed"`
		ActiveSuccessNoHandoff []string `json:"active_success_no_handoff"`
		CodexTotals            struct {
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
	if payload.Counts.ActiveSuccessNoHandoff != 1 || payload.Counts.ActiveSuccessNoHandoffTotal != 6 {
		t.Fatalf("active no-handoff counts = %+v, want active_success_no_handoff=1 total=6", payload.Counts)
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
	if payload.Retrying[0].Kind != string(orchestrator.RetryKindFailure) || payload.Retrying[1].Kind != string(orchestrator.RetryKindFailure) {
		t.Fatalf("retrying kinds = %q/%q, want failure defaults", payload.Retrying[0].Kind, payload.Retrying[1].Kind)
	}
	if payload.Retrying[0].StartupFailure == nil || payload.Retrying[0].StartupFailure.Phase != "thread/start" {
		t.Fatalf("retrying startup_failure = %+v; want thread/start", payload.Retrying[0].StartupFailure)
	}
	if !reflect.DeepEqual(payload.Completed, []string{"issue-3", "issue-9"}) {
		t.Fatalf("completed = %+v, want sorted issue-3 issue-9", payload.Completed)
	}
	if !reflect.DeepEqual(payload.ActiveSuccessNoHandoff, []string{"issue-12"}) {
		t.Fatalf("active_success_no_handoff = %+v, want [issue-12]", payload.ActiveSuccessNoHandoff)
	}
	// SPEC §8.4: failures retry rather than being parked in a suppression set,
	// so the state surface no longer emits a `failed` array (#584, D29 closed).
	if _, ok := raw["failed"]; ok {
		t.Fatalf("state response still emits a `failed` key: %#v", raw)
	}
	if _, ok := raw["counts"].(map[string]any)["failed"]; ok {
		t.Fatalf("state counts still emit a `failed` key: %#v", raw["counts"])
	}
	if _, ok := raw["counts"].(map[string]any)["failed_total"]; ok {
		t.Fatalf("state counts still emit a `failed_total` key: %#v", raw["counts"])
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
		if _, ok := rowObject["last_event_at"]; ok {
			t.Fatalf("zero last_event_at should be omitted from running row: %#v", rowObject)
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
		if _, ok := rowObject["last_event_at"]; ok {
			t.Fatalf("zero last_event_at should be omitted from blocked row: %#v", rowObject)
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
	// rate_limits is emitted unconditionally per SPEC §13.7.2 (#328): the
	// key must be present even when no rate-limit event has been observed,
	// in which case it serializes to JSON null (not an omitted key).
	rawRateLimits, ok := raw["rate_limits"]
	if !ok {
		t.Fatalf("rate_limits key must be present even when unobserved: %#v", raw)
	}
	if rawRateLimits != nil {
		t.Fatalf("rate_limits = %#v, want null when no rate-limit event observed", rawRateLimits)
	}
}

func TestApiRetryFromViewSurfacesQuotaBackoffKind(t *testing.T) {
	row := apiRetryFromView(orchestrator.RetryView{
		IssueID:    "issue-1",
		Identifier: "ENG-1",
		Attempt:    1,
		Error:      "quota backoff",
		Kind:       orchestrator.RetryKindQuotaBackoff,
	})
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["kind"] != "quota_backoff" {
		t.Fatalf("retry kind = %#v, want quota_backoff; raw=%s", got["kind"], raw)
	}
}

func TestApiRetryFromViewAlwaysEmitsKind(t *testing.T) {
	row := apiRetryFromView(orchestrator.RetryView{
		IssueID:    "issue-1",
		Identifier: "ENG-1",
		Attempt:    1,
	})
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["kind"] != "failure" {
		t.Fatalf("retry kind = %v; want failure; raw=%s", got["kind"], raw)
	}
}

func TestRootDashboardServesStateDepictingReactApp(t *testing.T) {
	server := newStateHTTPServer("127.0.0.1", 0, func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, nil
	})

	req := newLoopbackRequest(http.MethodGet, "http://127.0.0.1:4000/", nil)
	w := httptest.NewRecorder()
	server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("content type = %q, want text/html", got)
	}
	html := w.Body.String()
	for _, want := range []string{"<title>aiops · Worker status</title>", `id="root"`, "/api/v1/state"} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard HTML missing %q:\n%s", want, html)
		}
	}
	if strings.Contains(html, "Runtime state: <a href=\"/api/v1/state\">") {
		t.Fatalf("dashboard is still the old API-link stub: %s", html)
	}
	if strings.Contains(html, "/src/main.jsx") {
		t.Fatalf("dashboard fallback references unserved Vite source: %s", html)
	}
	// Favicon wiring (Symphony#90): the served document links the favicon with the
	// live content digest templated in, never the un-rendered placeholder.
	if strings.Contains(html, faviconVersionPlaceholder) {
		t.Fatalf("dashboard HTML still carries the un-templated favicon placeholder %q:\n%s", faviconVersionPlaceholder, html)
	}
	if wantLink := "/favicon.png?v=" + faviconDigest(); !strings.Contains(html, wantLink) {
		t.Fatalf("dashboard HTML missing favicon link %q:\n%s", wantLink, html)
	}

	if assetPath, ok := firstScriptAssetPath(html); ok && strings.HasPrefix(assetPath, "/assets/") {
		assetReq := newLoopbackRequest(http.MethodGet, "http://127.0.0.1:4000"+assetPath, nil)
		assetW := httptest.NewRecorder()
		server.Handler.ServeHTTP(assetW, assetReq)
		if assetW.Code != http.StatusOK {
			t.Fatalf("asset %s status code = %d, want %d; body=%s", assetPath, assetW.Code, http.StatusOK, assetW.Body.String())
		}
		asset := assetW.Body.String()
		for _, want := range []string{"/api/v1/state", "Running sessions", "Retrying sessions", "Blocked claims", "Total tokens", "Rate limits", "Delivered", "agent_handoff_reconcile_stopped", "reconcile_stopped_with_progress", "active_success_no_handoff"} {
			if !strings.Contains(asset, want) {
				t.Fatalf("dashboard asset missing state surface label %q", want)
			}
		}
	} else {
		if !strings.Contains(html, "Delivered") ||
			!strings.Contains(html, "agent_handoff_reconcile_stopped") ||
			!strings.Contains(html, "active_success_no_handoff") {
			t.Fatalf("dashboard fallback missing delivered handoff KPI wiring:\n%s", html)
		}
		if strings.Contains(html, "reconcile_stopped_with_progress") {
			t.Fatalf("dashboard fallback folds progress into Delivered KPI: %s", html)
		}
		if strings.Contains(html, "<span class=\"label\">Completed</span>") {
			t.Fatalf("dashboard fallback still labels the handoff KPI as Completed: %s", html)
		}
	}
}

func firstScriptAssetPath(html string) (string, bool) {
	re := regexp.MustCompile(`<script[^>]+src="([^"]+)"`)
	match := re.FindStringSubmatch(html)
	if len(match) != 2 {
		return "", false
	}
	return match[1], true
}

func TestDashboardServesReferencedFonts(t *testing.T) {
	server := newStateHTTPServer("127.0.0.1", 0, func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, nil
	})
	serve := func(path string) *httptest.ResponseRecorder {
		req := newLoopbackRequest(http.MethodGet, "http://127.0.0.1:4000"+path, nil)
		w := httptest.NewRecorder()
		server.Handler.ServeHTTP(w, req)
		return w
	}

	// The Vite build emits public/ fonts at the dist root (/fonts/*), not under
	// /assets/. Pull the served stylesheet and confirm every /fonts/* face it
	// references actually resolves through the server — otherwise the self-hosted
	// brand fonts 404 and the UI silently falls back to system faces.
	html := serve("/").Body.String()
	cssHref := regexp.MustCompile(`href="(/assets/[^"]+\.css)"`).FindStringSubmatch(html)
	if cssHref == nil {
		// Without `npm run build` the embedded dist has no index/stylesheet and
		// the worker serves fallback.html instead — there are no /fonts/ faces to
		// check. Skip rather than fail so `go test ./...` does not require a
		// dashboard build (mirrors the asset-label guard in the test above; dist/
		// is gitignored and CI builds it before the Go tests run).
		t.Skip("dashboard dist not built; served fallback has no /assets stylesheet")
	}
	cssResp := serve(cssHref[1])
	if cssResp.Code != http.StatusOK {
		t.Fatalf("GET %s = %d; want %d", cssHref[1], cssResp.Code, http.StatusOK)
	}
	fontRefs := regexp.MustCompile(`/fonts/[\w.-]+\.woff2`).FindAllString(cssResp.Body.String(), -1)
	if len(fontRefs) == 0 {
		t.Fatalf("stylesheet %s references no /fonts/*.woff2 faces", cssHref[1])
	}
	seen := map[string]bool{}
	for _, ref := range fontRefs {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		resp := serve(ref)
		if resp.Code != http.StatusOK {
			t.Errorf("GET %s = %d; want %d (referenced by %s)", ref, resp.Code, http.StatusOK, cssHref[1])
			continue
		}
		// wOF2 magic proves the font bytes were served, not an HTML fallback.
		if got := resp.Body.String(); !strings.HasPrefix(got, "wOF2") {
			t.Errorf("GET %s served non-woff2 body (first bytes %q); want the embedded font", ref, got[:min(4, len(got))])
		}
	}

	// A missing font path must still 404 rather than fall through to the index.
	if got := serve("/fonts/does-not-exist.woff2").Code; got != http.StatusNotFound {
		t.Errorf("GET /fonts/does-not-exist.woff2 = %d; want %d", got, http.StatusNotFound)
	}
}

func TestRootDashboardAllowsHeadProbes(t *testing.T) {
	server := newStateHTTPServer("127.0.0.1", 0, func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, nil
	})

	req := newLoopbackRequest(http.MethodHead, "http://127.0.0.1:4000/", nil)
	w := httptest.NewRecorder()
	server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("HEAD / status code = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if got := w.Body.Len(); got != 0 {
		t.Fatalf("HEAD / body length = %d, want 0", got)
	}
}

func TestRootDashboardRejectsUnsafeMethodsWithHeadInAllow(t *testing.T) {
	server := newStateHTTPServer("127.0.0.1", 0, func(context.Context) (orchestrator.StateView, error) {
		t.Fatal("snapshot function should not be called for unsafe root methods")
		return orchestrator.StateView{}, nil
	})

	req := newLoopbackRequest(http.MethodPost, "http://127.0.0.1:4000/", nil)
	w := httptest.NewRecorder()
	server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST / status code = %d, want %d; body=%s", w.Code, http.StatusMethodNotAllowed, w.Body.String())
	}
	if got, want := w.Header().Get("Allow"), "GET, HEAD"; got != want {
		t.Fatalf("Allow = %q, want %q", got, want)
	}
}

func TestHealthProbeEndpointsBypassStateHTTPAccessGuard(t *testing.T) {
	called := false
	server := newStateHTTPServerWithAuthToken("0.0.0.0", 0, "state-token", func(context.Context) (orchestrator.StateView, error) {
		called = true
		return orchestrator.StateView{}, nil
	})

	for _, path := range []string{"/livez", "/readyz"} {
		req := httptest.NewRequest(http.MethodGet, "http://worker.example:4000"+path, nil)
		req.RemoteAddr = "172.18.0.1:54321"
		w := httptest.NewRecorder()
		server.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("%s status code = %d, want %d; body=%s", path, w.Code, http.StatusOK, w.Body.String())
		}
		if got := w.Body.String(); got != "ok\n" {
			t.Fatalf("%s body = %q, want ok newline", path, got)
		}
	}
	if called {
		t.Fatal("snapshot function should not be called for health probes")
	}
}

func TestHealthProbeEndpointsRejectUnsafeMethods(t *testing.T) {
	server := newStateHTTPServer("127.0.0.1", 0, func(context.Context) (orchestrator.StateView, error) {
		t.Fatal("snapshot function should not be called for health probes")
		return orchestrator.StateView{}, nil
	})
	for _, path := range []string{"/livez", "/readyz"} {
		req := newLoopbackRequest(http.MethodPost, "http://127.0.0.1:4000"+path, nil)
		w := httptest.NewRecorder()
		server.Handler.ServeHTTP(w, req)
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodGet {
			t.Fatalf("POST %s status/Allow = %d/%q, want 405/GET; body=%s", path, w.Code, w.Header().Get("Allow"), w.Body.String())
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
	server := newStateHTTPServer("127.0.0.1", 0, func(context.Context) (orchestrator.StateView, error) {
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

	req := newLoopbackRequest(http.MethodGet, "http://127.0.0.1:4000/api/v1/mt-649", nil)
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
	server := newStateHTTPServer("127.0.0.1", 0, func(context.Context) (orchestrator.StateView, error) {
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

	req := newLoopbackRequest(http.MethodGet, "http://127.0.0.1:4000/api/v1/MT-650", nil)
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
	server := newStateHTTPServer("127.0.0.1", 0, func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{
			Running: []orchestrator.RunningView{{
				IssueID:    "issue-3",
				Identifier: "MT-651",
				StartedAt:  time.Date(2026, 5, 21, 9, 30, 0, 0, time.UTC),
				// RetryAttempt is nil: the issue has never been retried.
			}},
		}, nil
	})

	req := newLoopbackRequest(http.MethodGet, "http://127.0.0.1:4000/api/v1/MT-651", nil)
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
	server := newStateHTTPServer("127.0.0.1", 0, func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, nil
	})

	req := newLoopbackRequest(http.MethodGet, "http://127.0.0.1:4000/api/v1/NOPE-1", nil)
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
	server := newStateHTTPServer("127.0.0.1", 0, func(context.Context) (orchestrator.StateView, error) {
		called = true
		return orchestrator.StateView{}, nil
	})

	req := newLoopbackRequest(http.MethodPost, "http://127.0.0.1:4000/api/v1/MT-649", nil)
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
	server := newStateHTTPServer("127.0.0.1", 0, func(context.Context) (orchestrator.StateView, error) {
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
	firstReq := newLoopbackRequest(http.MethodPost, "http://127.0.0.1:4000/api/v1/refresh", nil)
	firstReq.Header.Set(refreshRequestHeader, refreshRequestHeaderValue)
	server.Handler.ServeHTTP(first, firstReq)
	second := httptest.NewRecorder()
	secondReq := newLoopbackRequest(http.MethodPost, "http://127.0.0.1:4000/api/v1/refresh", strings.NewReader("{}"))
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
	server := newStateHTTPServer("127.0.0.1", 0, func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, nil
	}, func(context.Context) (orchestrator.RefreshRequestResult, error) {
		called = true
		return orchestrator.RefreshRequestResult{}, nil
	})

	resp := httptest.NewRecorder()
	server.Handler.ServeHTTP(resp, newLoopbackRequest(http.MethodPost, "http://127.0.0.1:4000/api/v1/refresh", nil))

	if resp.Code != http.StatusForbidden {
		t.Fatalf("status code = %d, want %d; body=%s", resp.Code, http.StatusForbidden, resp.Body.String())
	}
	if called {
		t.Fatal("refresh function should not be called without refresh header")
	}
}

func TestRefreshHTTPHandlerRejectsUnsupportedMethods(t *testing.T) {
	called := false
	server := newStateHTTPServer("127.0.0.1", 0, func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, nil
	}, func(context.Context) (orchestrator.RefreshRequestResult, error) {
		called = true
		return orchestrator.RefreshRequestResult{}, nil
	})

	getResp := httptest.NewRecorder()
	server.Handler.ServeHTTP(getResp, newLoopbackRequest(http.MethodGet, "http://127.0.0.1:4000/api/v1/refresh", nil))
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
	server := newStateHTTPServer("127.0.0.1", 0, func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, nil
	}, func(context.Context) (orchestrator.RefreshRequestResult, error) {
		called = true
		return orchestrator.RefreshRequestResult{}, nil
	})

	for _, body := range []string{`[]`, `null`, `"refresh"`, `{"force":true}`} {
		req := newLoopbackRequest(http.MethodPost, "http://127.0.0.1:4000/api/v1/refresh", strings.NewReader(body))
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

func TestRefreshHTTPHandlerAcceptsLimitSizedBodies(t *testing.T) {
	calls := 0
	server := newStateHTTPServer("127.0.0.1", 0, func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, nil
	}, func(context.Context) (orchestrator.RefreshRequestResult, error) {
		calls++
		return orchestrator.RefreshRequestResult{Queued: true, RequestedAt: time.Date(2026, 5, 25, 12, 30, 0, 0, time.UTC)}, nil
	})

	for _, tc := range []struct {
		name               string
		body               string
		forceUnknownLength bool
		wantContentLength  int64
	}{
		{
			name:              "known length",
			body:              "{}" + strings.Repeat(" ", refreshBodyLimitBytes-2),
			wantContentLength: int64(refreshBodyLimitBytes),
		},
		{
			name:               "unknown length",
			body:               "{}" + strings.Repeat(" ", refreshBodyLimitBytes-2),
			forceUnknownLength: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := newLoopbackRequest(http.MethodPost, "http://127.0.0.1:4000/api/v1/refresh", strings.NewReader(tc.body))
			req.Header.Set(refreshRequestHeader, refreshRequestHeaderValue)
			if tc.forceUnknownLength {
				req.ContentLength = -1
			} else if req.ContentLength != tc.wantContentLength {
				t.Fatalf("ContentLength = %d, want %d", req.ContentLength, tc.wantContentLength)
			}
			resp := httptest.NewRecorder()
			server.Handler.ServeHTTP(resp, req)
			if resp.Code != http.StatusAccepted {
				t.Fatalf("status code = %d, want %d; response=%s", resp.Code, http.StatusAccepted, resp.Body.String())
			}
		})
	}
	if calls != 2 {
		t.Fatalf("refresh calls = %d, want 2", calls)
	}
}

func TestRefreshHTTPHandlerRejectsOversizedBodies(t *testing.T) {
	called := false
	server := newStateHTTPServer("127.0.0.1", 0, func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, nil
	}, func(context.Context) (orchestrator.RefreshRequestResult, error) {
		called = true
		return orchestrator.RefreshRequestResult{}, nil
	})

	for _, tc := range []struct {
		name               string
		body               string
		forceUnknownLength bool
		wantContentLength  int64
	}{
		{
			name:              "known length",
			body:              "{}" + strings.Repeat(" ", refreshBodyLimitBytes-1),
			wantContentLength: int64(refreshBodyLimitBytes + 1),
		},
		{
			name:               "unknown length",
			body:               "{}" + strings.Repeat(" ", refreshBodyLimitBytes-1),
			forceUnknownLength: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := newLoopbackRequest(http.MethodPost, "http://127.0.0.1:4000/api/v1/refresh", strings.NewReader(tc.body))
			req.Header.Set(refreshRequestHeader, refreshRequestHeaderValue)
			if tc.forceUnknownLength {
				req.ContentLength = -1
			} else if req.ContentLength != tc.wantContentLength {
				t.Fatalf("ContentLength = %d, want %d", req.ContentLength, tc.wantContentLength)
			}
			resp := httptest.NewRecorder()
			server.Handler.ServeHTTP(resp, req)
			if resp.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status code = %d, want %d; response=%s", resp.Code, http.StatusRequestEntityTooLarge, resp.Body.String())
			}
			var payload apiErrorResponse
			if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode error response: %v; body=%s", err, resp.Body.String())
			}
			if payload.Error.Code != "refresh_body_too_large" {
				t.Fatalf("error code = %q, want refresh_body_too_large", payload.Error.Code)
			}
		})
	}
	if called {
		t.Fatal("refresh function should not be called for oversized bodies")
	}
}

func TestStateHTTPServerRejectsNonLoopbackHost(t *testing.T) {
	called := false
	server := newStateHTTPServer("127.0.0.1", 0, func(context.Context) (orchestrator.StateView, error) {
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

func TestStateHTTPServerRejectsSpoofedLoopbackHostFromNonLoopbackPeer(t *testing.T) {
	called := false
	server := newStateHTTPServer("0.0.0.0", 0, func(context.Context) (orchestrator.StateView, error) {
		called = true
		return orchestrator.StateView{}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4000/api/v1/state", nil)
	req.RemoteAddr = "203.0.113.10:54321"
	w := httptest.NewRecorder()
	server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status code = %d, want %d", w.Code, http.StatusForbidden)
	}
	if called {
		t.Fatal("snapshot function should not be called for spoofed loopback Host")
	}
}

func TestStateHTTPServerRequiresAuthForNonLoopbackPeerWhenTokenConfigured(t *testing.T) {
	called := false
	server := newStateHTTPServerWithAuthToken("0.0.0.0", 0, "state-token", func(context.Context) (orchestrator.StateView, error) {
		called = true
		return orchestrator.StateView{}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4000/api/v1/state", nil)
	req.RemoteAddr = "172.18.0.1:54321"
	w := httptest.NewRecorder()
	server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status code = %d, want %d; body=%s", w.Code, http.StatusUnauthorized, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("WWW-Authenticate = %q, want Basic challenge", w.Header().Get("WWW-Authenticate"))
	}
	if called {
		t.Fatal("snapshot function should not be called without auth from non-loopback peer")
	}
}

func TestStateHTTPServerRequiresAuthForLoopbackPeerWhenTokenConfigured(t *testing.T) {
	called := false
	server := newStateHTTPServerWithAuthToken("127.0.0.1", 0, "state-token", func(context.Context) (orchestrator.StateView, error) {
		called = true
		return orchestrator.StateView{}, nil
	})

	req := newLoopbackRequest(http.MethodGet, "http://127.0.0.1:4000/api/v1/state", nil)
	w := httptest.NewRecorder()
	server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status code = %d, want %d; body=%s", w.Code, http.StatusUnauthorized, w.Body.String())
	}
	if called {
		t.Fatal("snapshot function should not be called without auth when token is configured")
	}
}

func TestStateHTTPServerRejectsWrongToken(t *testing.T) {
	called := false
	server := newStateHTTPServerWithAuthToken("0.0.0.0", 0, "state-token", func(context.Context) (orchestrator.StateView, error) {
		called = true
		return orchestrator.StateView{}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "http://worker.example:4000/api/v1/state", nil)
	req.RemoteAddr = "172.18.0.1:54321"
	req.Header.Set("Authorization", "Bearer wrong-token")
	w := httptest.NewRecorder()
	server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status code = %d, want %d; body=%s", w.Code, http.StatusUnauthorized, w.Body.String())
	}
	if called {
		t.Fatal("snapshot function should not be called for wrong auth token")
	}
}

func TestStateHTTPServerAllowsBearerAuthFromNonLoopbackPeer(t *testing.T) {
	called := false
	server := newStateHTTPServerWithAuthToken("0.0.0.0", 0, "state-token", func(context.Context) (orchestrator.StateView, error) {
		called = true
		return orchestrator.StateView{}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "http://worker.example:4000/api/v1/state", nil)
	req.RemoteAddr = "172.18.0.1:54321"
	req.Header.Set("Authorization", "Bearer state-token")
	w := httptest.NewRecorder()
	server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if !called {
		t.Fatal("snapshot function was not called for authenticated non-loopback peer")
	}
}

func TestStateHTTPServerAllowsBasicAuthFromNonLoopbackPeer(t *testing.T) {
	called := false
	server := newStateHTTPServerWithAuthToken("0.0.0.0", 0, "state-token", func(context.Context) (orchestrator.StateView, error) {
		called = true
		return orchestrator.StateView{}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4000/api/v1/state", nil)
	req.RemoteAddr = "172.18.0.1:54321"
	req.SetBasicAuth("aiops", "state-token")
	w := httptest.NewRecorder()
	server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if !called {
		t.Fatal("snapshot function was not called for authenticated dashboard request")
	}
}

func TestStateHTTPServerAllowsLoopbackHost(t *testing.T) {
	called := false
	server := newStateHTTPServer("127.0.0.1", 0, func(context.Context) (orchestrator.StateView, error) {
		called = true
		return orchestrator.StateView{}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4000/api/v1/state", nil)
	req.RemoteAddr = "127.0.0.1:54321"
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
	server := newStateHTTPServer("127.0.0.1", 0, func(context.Context) (orchestrator.StateView, error) {
		called = true
		return orchestrator.StateView{}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "http://[::1]:4000/api/v1/state", nil)
	req.RemoteAddr = "[::1]:54321"
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
		{"[::1", false},
		{"::1]", false},
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

func TestNormalizeHostFromHostport(t *testing.T) {
	cases := []struct {
		in       string
		wantHost string
		wantOK   bool
	}{
		{"127.0.0.1:4000", "127.0.0.1", true},
		{"127.0.0.1", "127.0.0.1", true},
		{"localhost:4000", "localhost", true},
		{"localhost", "localhost", true},
		{"[::1]:4000", "::1", true},
		{"[::1]", "::1", true}, // bracketed IPv6, no port — strip-without-port path
		{"evil.example", "evil.example", true},
		{"evil.example:4000", "evil.example", true},
		// Fail-closed: a host the gate cannot parse unambiguously must report ok=false.
		{"", "", false},
		{"::1", "", false},  // unbracketed IPv6 "host:port" that fails to split
		{"[::1", "", false}, // opening bracket without a closing one
		{"::1]", "", false}, // closing bracket without an opening one
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			gotHost, gotOK := normalizeHostFromHostport(c.in)
			if gotHost != c.wantHost || gotOK != c.wantOK {
				t.Fatalf("normalizeHostFromHostport(%q) = (%q, %v), want (%q, %v)", c.in, gotHost, gotOK, c.wantHost, c.wantOK)
			}
		})
	}
}

func TestStartStateHTTPServerSkipsDisabledPort(t *testing.T) {
	handle := startStateHTTPServer(context.Background(), "127.0.0.1", -1, func(context.Context) (orchestrator.StateView, error) {
		t.Fatal("disabled state server must not evaluate snapshot")
		return orchestrator.StateView{}, nil
	}, stateHTTPAlwaysReady)
	if handle != nil {
		t.Fatalf("disabled state server handle = %v, want nil", handle)
	}
}

func TestStartStateHTTPServerDoesNotFailWorkerWhenPortInUse(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer func() { _ = listener.Close() }()
	port := listener.Addr().(*net.TCPAddr).Port

	handle := startStateHTTPServer(context.Background(), "127.0.0.1", port, func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, nil
	}, stateHTTPAlwaysReady)
	if handle != nil {
		t.Fatalf("occupied port state server handle = %v, want nil", handle)
	}
}

func TestStartStateHTTPServerBindsPrivateLoopback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handle := startStateHTTPServer(ctx, "127.0.0.1", 0, func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, nil
	}, stateHTTPAlwaysReady)
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

	controller.apply(ctx, "127.0.0.1", 0)
	if controller.cancel == nil || controller.addr == nil {
		t.Fatalf("controller after start = cancel:%v addr:%v, want running server", controller.cancel, controller.addr)
	}
	controller.apply(ctx, "127.0.0.1", -1)
	if controller.cancel != nil || controller.addr != nil {
		t.Fatalf("controller after disable = cancel:%v addr:%v, want stopped server", controller.cancel, controller.addr)
	}
}

func TestStateHTTPServerControllerServesReadyzBeforeReadiness(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := &stateHTTPReadiness{}
	controller := newStateHTTPServerController(func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, nil
	})
	controller.readiness = ready.Status

	controller.apply(ctx, "127.0.0.1", 0)
	defer controller.stop()
	if controller.addr == nil {
		t.Fatal("controller addr = nil, want running server")
	}
	client := http.Client{Timeout: 2 * time.Second}
	url := "http://" + controller.addr.String() + "/readyz"

	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET /readyz before readiness: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if closeErr := resp.Body.Close(); closeErr != nil {
		t.Fatalf("close /readyz before readiness body: %v", closeErr)
	}
	if err != nil {
		t.Fatalf("read /readyz before readiness: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz before readiness status = %d, want %d; body=%s", resp.StatusCode, http.StatusServiceUnavailable, string(body))
	}

	ready.MarkReady()
	resp, err = client.Get(url)
	if err != nil {
		t.Fatalf("GET /readyz after readiness: %v", err)
	}
	body, err = io.ReadAll(resp.Body)
	if closeErr := resp.Body.Close(); closeErr != nil {
		t.Fatalf("close /readyz after readiness body: %v", closeErr)
	}
	if err != nil {
		t.Fatalf("read /readyz after readiness: %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "ok\n" {
		t.Fatalf("GET /readyz after readiness status/body = %d/%q, want 200/ok", resp.StatusCode, string(body))
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
	controller.apply(ctx, "127.0.0.1", port)
	if controller.cancel != nil || controller.addr != nil || controller.desiredSet {
		t.Fatalf("controller after failed listen = cancel:%v addr:%v desiredSet:%v, want retryable idle state", controller.cancel, controller.addr, controller.desiredSet)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}
	controller.apply(ctx, "127.0.0.1", port)
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
	defer func() { _ = blockedListener.Close() }()
	blockedPort := blockedListener.Addr().(*net.TCPAddr).Port

	controller := newStateHTTPServerController(func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, nil
	})
	controller.apply(ctx, "127.0.0.1", oldPort)
	if controller.cancel == nil || controller.addr == nil {
		t.Fatalf("controller after old port start = cancel:%v addr:%v, want running server", controller.cancel, controller.addr)
	}
	controller.apply(ctx, "127.0.0.1", blockedPort)
	if controller.cancel != nil || controller.addr != nil || controller.desiredSet {
		t.Fatalf("controller after blocked reload = cancel:%v addr:%v desiredSet:%v, want retryable idle state", controller.cancel, controller.addr, controller.desiredSet)
	}
	controller.apply(ctx, "127.0.0.1", oldPort)
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

	controller.apply(ctx, "127.0.0.1", 0)
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
	for _, required := range []string{"orchestrator.NewOrchestratorState", "orchestrator.NewWorkflowRuntime", "orchestrator.NewRuntimeDispatcher", "orchestrator.NewRuntimePollerWithTrackerFactory", "orchestrator.RunPollLoopWithRuntime", "orchestrator.RunWorkflowReloadLoop"} {
		if !strings.Contains(string(src), required) {
			t.Fatalf("cmd/worker/main.go missing %q; worker startup must poll tracker issues through dynamically reloaded reconciled orchestrator runtime state", required)
		}
	}
}

func TestLoadWorkflowForStartupReconcileUsesConfiguredWorkflowPath(t *testing.T) {
	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "linear-workflow.md")
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\ntracker:\n  kind: linear\n  project_slug: platform\n  active_states: [\"Todo\"]\n  terminal_states: [\"Done\"]\n---\nprompt\n"
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
	cfg.Tracker.APIKey = "tracker-secret"
	cfg.Hooks = workflow.WorkspaceHooks{
		BeforeRemove:   workflow.WorkspaceHook{Commands: []string{"printf top-level"}},
		TimeoutMs:      1234,
		EnvPassthrough: []string{"AIOPS_TRACKER_SECRET"},
	}

	reconcile := startupReconcileConfigForWorkflow(cfg, nil)
	if !reflect.DeepEqual(reconcile.BeforeRemoveHook.Commands, []string{"printf top-level"}) {
		t.Fatalf("BeforeRemoveHook.Commands = %#v, want top-level effective hook", reconcile.BeforeRemoveHook.Commands)
	}
	if reconcile.HookTimeoutMillis != 1234 {
		t.Fatalf("HookTimeoutMillis = %d, want top-level effective timeout", reconcile.HookTimeoutMillis)
	}
	if !reflect.DeepEqual(reconcile.HookEnvPassthrough, []string{"AIOPS_TRACKER_SECRET"}) {
		t.Fatalf("HookEnvPassthrough = %#v, want top-level effective passthrough", reconcile.HookEnvPassthrough)
	}
	if reconcile.WorkflowConfig.Tracker.APIKey != cfg.Tracker.APIKey {
		t.Fatalf("WorkflowConfig.Tracker.APIKey = %q, want startup workflow config to feed before_remove env deny", reconcile.WorkflowConfig.Tracker.APIKey)
	}
}

func TestStartupReconcileConfigHonorsWorkflowWorkspaceRoot(t *testing.T) {
	yamlRoot := filepath.Join(t.TempDir(), "yaml-workspaces")
	workflowPath := writeWorkflowForStartupReconcileTest(t, fmt.Sprintf("workspace:\n  root: %s\n", yamlRoot))
	wf, err := workflow.Load(workflowPath)
	if err != nil {
		t.Fatalf("Load workflow: %v", err)
	}
	t.Setenv("AIOPS_WORKSPACE_ROOT", filepath.Join(t.TempDir(), "env-workspaces"))

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
	t.Setenv("AIOPS_WORKSPACE_ROOT", envRoot)

	reconcile := startupReconcileConfigForWorkflow(wf.Config, nil)
	if reconcile.WorkspaceRoot != envRoot {
		t.Fatalf("WorkspaceRoot = %q, want env workspace root %q", reconcile.WorkspaceRoot, envRoot)
	}
}

func TestStartupReconcileConfigHonorsExplicitDefaultWorkflowWorkspaceRoot(t *testing.T) {
	// Pin SPEC §6.4 precedence: an explicit `workspace.root` in
	// WORKFLOW.md wins over AIOPS_WORKSPACE_ROOT env even when its value
	// equals the SPEC default itself. Pre-#319 this test used the
	// personal-profile legacy `~/aiops-workspaces` literal — the same
	// literal PR #316 retired at the workflow-loader floor — which kept
	// the SPEC drift alive at the worker layer.
	yamlRoot := filepath.Join(os.TempDir(), "symphony_workspaces")
	workflowPath := writeWorkflowForStartupReconcileTest(t, fmt.Sprintf("workspace:\n  root: %s\n", yamlRoot))
	wf, err := workflow.Load(workflowPath)
	if err != nil {
		t.Fatalf("Load workflow: %v", err)
	}
	envRoot := filepath.Join(t.TempDir(), "env-workspaces")
	t.Setenv("AIOPS_WORKSPACE_ROOT", envRoot)

	reconcile := startupReconcileConfigForWorkflow(wf.Config, nil)
	if reconcile.WorkspaceRoot != yamlRoot {
		t.Fatalf("WorkspaceRoot = %q, want explicit workflow default root %q", reconcile.WorkspaceRoot, yamlRoot)
	}
}

// TestStartupReconcileConfigFallsBackToSPECWorkspaceRootDefault covers
// the #319 fix: when WORKFLOW.md omits `workspace.root` and
// AIOPS_WORKSPACE_ROOT is unset, the startup reconcile resolves to the SPEC
// §6.4 default (`<system-temp>/symphony_workspaces`) that
// workflow.DefaultConfig seeds. Pre-#319 the env loader's
// `/tmp/aiops-workspaces` literal shadowed the SPEC default in this
// case; `worker --print-config` reported one path while the runtime
// used another.
func TestStartupReconcileConfigFallsBackToSPECWorkspaceRootDefault(t *testing.T) {
	workflowPath := writeWorkflowForStartupReconcileTest(t, "")
	wf, err := workflow.Load(workflowPath)
	if err != nil {
		t.Fatalf("Load workflow: %v", err)
	}
	t.Setenv("AIOPS_WORKSPACE_ROOT", "")

	reconcile := startupReconcileConfigForWorkflow(wf.Config, nil)
	want := filepath.Join(os.TempDir(), "symphony_workspaces")
	if reconcile.WorkspaceRoot != want {
		t.Fatalf("WorkspaceRoot = %q, want SPEC §6.4 default %q", reconcile.WorkspaceRoot, want)
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
tracker:
  kind: gitea
` + "---\nPrompt body\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	return path
}

func TestTrackerClientForWorkflowUsesGiteaEndpointBeforeEnvBaseURL(t *testing.T) {
	t.Setenv("GITEA_BASE_URL", "https://gitea-env.example.test/")
	cfg := workflow.DefaultConfig()
	cfg.Tracker.Kind = "gitea"
	cfg.Tracker.Endpoint = "https://gitea-endpoint.example.test/"
	cfg.Tracker.ProjectSlug = "https://gitea-legacy.example.test/"
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
	if giteaClient.BaseURL != "https://gitea-endpoint.example.test" {
		t.Fatalf("base URL = %q, want tracker.endpoint without trailing slash", giteaClient.BaseURL)
	}
}

func TestTrackerClientForWorkflowIgnoresGiteaProjectSlugBaseURL(t *testing.T) {
	t.Setenv("GITEA_BASE_URL", "https://gitea-env.example.test/")
	cfg := workflow.DefaultConfig()
	cfg.Tracker.Kind = "gitea"
	cfg.Tracker.ProjectSlug = "https://gitea-legacy.example.test/"
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
	if giteaClient.BaseURL != "https://gitea-env.example.test" {
		t.Fatalf("base URL = %q, want GITEA_BASE_URL fallback when tracker.endpoint is empty", giteaClient.BaseURL)
	}
}

func TestTrackerClientForWorkflowUsesGiteaEnvFallbackWhenEndpointEmpty(t *testing.T) {
	t.Setenv("GITEA_BASE_URL", "https://gitea-env.example.test/")
	cfg := workflow.DefaultConfig()
	cfg.Tracker.Kind = "gitea"
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
	if giteaClient.BaseURL != "https://gitea-env.example.test" {
		t.Fatalf("base URL = %q, want GITEA_BASE_URL without trailing slash", giteaClient.BaseURL)
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

func TestWorkerReconciliationConfigIncludesInactiveStates(t *testing.T) {
	cfg := workflow.DefaultConfig()
	cfg.Repo.CloneURL = "git@example.com:o/r.git"
	cfg.Tracker.Kind = "linear"
	cfg.Tracker.ActiveStates = []string{"Todo", "In Progress", "Rework"}
	cfg.Tracker.TerminalStates = []string{"Done", "Canceled"}

	reconcile := reconciliationConfigForWorkflow(cfg)
	if len(reconcile.InactiveStates) == 0 {
		t.Fatalf("inactive reconciliation states = %v, want non-empty states for explicit inactive tracker observations", reconcile.InactiveStates)
	}
	if containsState(reconcile.InactiveStates, "Todo") || containsState(reconcile.InactiveStates, "In Progress") || containsState(reconcile.InactiveStates, "Rework") {
		t.Fatalf("inactive reconciliation states = %v, must not include configured active states", reconcile.InactiveStates)
	}
	if containsState(reconcile.InactiveStates, "Done") || containsState(reconcile.InactiveStates, "Canceled") {
		t.Fatalf("inactive reconciliation states = %v, must not duplicate terminal states", reconcile.InactiveStates)
	}
	if !containsState(reconcile.InactiveStates, "Backlog") || !containsState(reconcile.InactiveStates, "Human Review") {
		t.Fatalf("inactive reconciliation states = %v, want Backlog and Human Review", reconcile.InactiveStates)
	}
}

// TestTrackerClientForWorkflowUsesGiteaLocalhostDefaultWhenNoEnvAndNoConfig
// pins the all-empty arm of the shared gitea.BaseURLFromEnv resolution: with
// no endpoint and no GITEA_BASE_URL, both the worker and doctor must land on
// the local-dev default.
func TestTrackerClientForWorkflowUsesGiteaLocalhostDefaultWhenNoEnvAndNoConfig(t *testing.T) {
	t.Setenv("GITEA_BASE_URL", "")
	cfg := workflow.DefaultConfig()
	cfg.Tracker.Kind = "gitea"
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
	if giteaClient.BaseURL != "http://localhost:3000" {
		t.Fatalf("base URL = %q, want the http://localhost:3000 default with no endpoint/env", giteaClient.BaseURL)
	}
}

// TestTrackerClientForWorkflowUsesGitHubDefaultWhenNoEnvAndNoEndpoint pins
// the all-empty arm of the shared tracker.NewGitHubClientFromEnv resolution:
// with no endpoint and no GITHUB_API_BASE_URL, both the worker and doctor
// must land on the constructor's api.github.com default.
func TestTrackerClientForWorkflowUsesGitHubDefaultWhenNoEnvAndNoEndpoint(t *testing.T) {
	t.Setenv("GITHUB_API_BASE_URL", "")
	cfg := workflow.DefaultConfig()
	cfg.Repo.Owner = "owner"
	cfg.Repo.Name = "repo"
	cfg.Tracker.Kind = "github"
	cfg.Tracker.APIKey = "github-token"

	client, err := trackerClientForWorkflow(cfg)
	if err != nil {
		t.Fatalf("trackerClientForWorkflow: %v", err)
	}
	githubClient, ok := client.(*tracker.GitHubClient)
	if !ok {
		t.Fatalf("client type = %T, want *tracker.GitHubClient", client)
	}
	if githubClient.BaseURL != "https://api.github.com" {
		t.Fatalf("github base URL = %q, want the https://api.github.com default with no endpoint/env", githubClient.BaseURL)
	}
}

func TestTrackerClientForWorkflowBuildsGitHubClient(t *testing.T) {
	cfg := workflow.DefaultConfig()
	cfg.Repo.Owner = "xrf9268-hue"
	cfg.Repo.Name = "aiops-platform"
	cfg.Tracker.Kind = "github"
	cfg.Tracker.APIKey = "github-token"
	cfg.Tracker.Endpoint = "https://api.github.test"

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
	cfg.Tracker.ActiveStates = []string{"Todo", "In Progress", "Rework"}
	cfg.Tracker.TerminalStates = []string{"Done", "Canceled"}

	reconcile := reconciliationConfigForWorkflow(cfg)
	if containsState(reconcile.InactiveStates, "Backlog") {
		t.Fatalf("inactive reconciliation states = %v, must not include unmapped Gitea Backlog state", reconcile.InactiveStates)
	}
	if !containsState(reconcile.InactiveStates, "Human Review") {
		t.Fatalf("inactive reconciliation states = %v, want mapped Gitea Human Review state", reconcile.InactiveStates)
	}
	if !containsState(reconcile.InactiveStates, "Merging") {
		t.Fatalf("inactive reconciliation states = %v, want mapped Gitea Merging state", reconcile.InactiveStates)
	}
}

func TestWorkerReconciliationConfigFiltersActiveGiteaMergingState(t *testing.T) {
	cfg := workflow.DefaultConfig()
	cfg.Repo.CloneURL = "git@example.com:o/r.git"
	cfg.Tracker.Kind = "gitea"
	cfg.Tracker.ActiveStates = []string{"Todo", "In Progress", "Merging"}
	cfg.Tracker.TerminalStates = []string{"Done", "Canceled"}

	reconcile := reconciliationConfigForWorkflow(cfg)
	if containsState(reconcile.InactiveStates, "Merging") {
		t.Fatalf("inactive reconciliation states = %v, must not include active Merging state", reconcile.InactiveStates)
	}
	if !containsState(reconcile.InactiveStates, "Human Review") {
		t.Fatalf("inactive reconciliation states = %v, want mapped Gitea Human Review state", reconcile.InactiveStates)
	}
}

func TestWorkerReconciliationConfigUsesWorkflowInactiveStates(t *testing.T) {
	cfg := workflow.DefaultConfig()
	cfg.Repo.CloneURL = "git@example.com:o/r.git"
	cfg.Tracker.ActiveStates = []string{"Todo", "In Progress"}
	cfg.Tracker.TerminalStates = []string{"Done", "Canceled"}
	cfg.Tracker.InactiveStates = []string{"Paused", "Blocked", "Done", "Todo"}

	reconcile := reconciliationConfigForWorkflow(cfg)
	if !containsState(reconcile.InactiveStates, "Paused") || !containsState(reconcile.InactiveStates, "Blocked") {
		t.Fatalf("inactive reconciliation states = %v, want workflow-configured inactive states", reconcile.InactiveStates)
	}
	if containsState(reconcile.InactiveStates, "Done") || containsState(reconcile.InactiveStates, "Todo") {
		t.Fatalf("inactive reconciliation states = %v, must exclude configured active/terminal states", reconcile.InactiveStates)
	}
}

func TestWorkerReconciliationConfigThreadsRequiredLabels(t *testing.T) {
	cfg := workflow.DefaultConfig()
	cfg.Repo.CloneURL = "git@example.com:o/r.git"
	cfg.Tracker.ActiveStates = []string{"Todo", "In Progress"}
	cfg.Tracker.TerminalStates = []string{"Done", "Canceled"}
	cfg.Tracker.RequiredLabels = []string{"aiops-ready", "triaged"}

	reconcile := reconciliationConfigForWorkflow(cfg)
	if !reflect.DeepEqual(reconcile.RequiredLabels, cfg.Tracker.RequiredLabels) {
		t.Fatalf("reconciliationConfigForWorkflow(required_labels=%v).RequiredLabels = %v; want %v", cfg.Tracker.RequiredLabels, reconcile.RequiredLabels, cfg.Tracker.RequiredLabels)
	}
}

func TestWorkerReconciliationConfigDefaultsRequiredLabelsEmpty(t *testing.T) {
	cfg := workflow.DefaultConfig()
	cfg.Repo.CloneURL = "git@example.com:o/r.git"

	reconcile := reconciliationConfigForWorkflow(cfg)
	if len(reconcile.RequiredLabels) != 0 {
		t.Fatalf("reconciliationConfigForWorkflow(no required_labels).RequiredLabels = %v; want empty (gate disabled by default)", reconcile.RequiredLabels)
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
	// SPEC §6.4 marks tracker.kind REQUIRED; DefaultConfig leaves it
	// empty so a prompt-only WORKFLOW.md (which cannot declare front
	// matter) surfaces an empty kind in the startup log. Operators
	// must add a `tracker.kind:` line for tracker integration to
	// dispatch. See DEVIATIONS D28 / #244.
	//
	// Assert the trailing-newline form so a regression that re-introduces
	// any silent default (gitea / linear / github / any future kind)
	// fails this test — `strings.Contains(s, "tracker.kind=")` alone
	// would still match `"tracker.kind=gitea\n"`. The regex below catches
	// any non-whitespace character following `tracker.kind=`, so adding a
	// new supported kind does not silently bypass the guard.
	gotLog := logs.String()
	for _, want := range []string{"startup reconciliation: workflow source=prompt_only", "path=" + workflowPath, "tracker.kind=\n"} {
		if !strings.Contains(gotLog, want) {
			t.Fatalf("startup reconciliation log = %q, want substring %q", gotLog, want)
		}
	}
	if regexp.MustCompile(`tracker\.kind=\S`).MatchString(gotLog) {
		t.Fatalf("startup reconciliation log = %q, must not log a non-empty tracker.kind (regression: SPEC §6.4 REQUIRED was bypassed by a silent default)", gotLog)
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
	// SPEC §6.4 marks tracker.kind REQUIRED; DefaultConfig leaves it
	// empty so a worker that starts without any WORKFLOW.md surfaces
	// an empty kind in the startup log. See DEVIATIONS D28 / #244.
	//
	// Assert the trailing-newline form so a regression that re-introduces
	// any silent default (gitea / linear / github / any future kind)
	// fails this test — `strings.Contains(s, "tracker.kind=")` alone
	// would still match `"tracker.kind=gitea\n"`. The regex below catches
	// any non-whitespace character following `tracker.kind=`, so adding a
	// new supported kind does not silently bypass the guard.
	gotLog := logs.String()
	for _, want := range []string{"startup reconciliation: workflow source=default", "tracker.kind=\n"} {
		if !strings.Contains(gotLog, want) {
			t.Fatalf("startup reconciliation log = %q, want substring %q", gotLog, want)
		}
	}
	if regexp.MustCompile(`tracker\.kind=\S`).MatchString(gotLog) {
		t.Fatalf("startup reconciliation log = %q, must not log a non-empty tracker.kind (regression: SPEC §6.4 REQUIRED was bypassed by a silent default)", gotLog)
	}
	if strings.Contains(gotLog, "reconciliation will be skipped") {
		t.Fatalf("startup reconciliation log = %q, did not expect skip diagnostic", gotLog)
	}
}

// TestNewStateHTTPServer_SetsMaxHeaderBytes pins the cap value itself
// (=N) so a future refactor that drops the line is loud. Go's
// http.Server adds an internal 4 KiB slop on top of MaxHeaderBytes for
// the bufio reader, so a precise =N+1 byte-level boundary against the
// header reader is not testable without depending on Go-internal
// constants; the structural assertion locks the contract that the
// field is set, and TestNewStateHTTPServer_RejectsOversizedHeader
// covers behavior on the over-cap side.
func TestNewStateHTTPServer_SetsMaxHeaderBytes(t *testing.T) {
	server := newStateHTTPServer("127.0.0.1", 0, func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, nil
	})
	if got, want := server.MaxHeaderBytes, 64<<10; got != want {
		t.Fatalf("MaxHeaderBytes = %d, want %d", got, want)
	}
}

// TestNewStateHTTPServer_RejectsOversizedHeader confirms the cap fires
// in practice: a request with header bytes well above MaxHeaderBytes +
// Go's 4 KiB slop must not produce a 200. Using 256 KiB (4x the cap)
// is decisively over either side of the slop, so the test is stable
// across Go versions that adjust the slop. (The cap-2 / cap/2 / no-cap
// matrix is not a viable boundary for this field — see the comment on
// SetsMaxHeaderBytes.)
func TestNewStateHTTPServer_RejectsOversizedHeader(t *testing.T) {
	server := newStateHTTPServer("127.0.0.1", 0, func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, nil
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()
	go func() { _ = server.Serve(listener) }()
	defer func() { _ = server.Close() }()

	addr := listener.Addr().String()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	huge := strings.Repeat("a", 256<<10)
	req := "GET /api/v1/state HTTP/1.1\r\nHost: 127.0.0.1\r\nX-Bloat: " + huge + "\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		// Server may close the connection during write; either way the
		// failure path is being exercised. Surface unexpected I/O.
		if !errors.Is(err, net.ErrClosed) {
			t.Logf("write request (server likely already closed): %v", err)
		}
	}
	respBytes, _ := io.ReadAll(conn)
	resp := string(respBytes)
	if strings.HasPrefix(resp, "HTTP/1.1 200") {
		t.Fatalf("oversized header produced 200 OK, want failure response or closed connection. Got prefix:\n%s", firstLine(resp))
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// TestStateHTTPHandler_DoesNotLeakErrorTextInBody asserts the safe-
// constant contract added by #198: the catch-all snapshot error path
// must never echo the wrapped err.Error() text into the response body,
// because that text is operator-internal state (orchestrator paths,
// timings, traces). Each handler is tested with a canary string in the
// error message; the handler still serves the typed code + a fixed
// safe message, and the canary stays server-side.
func TestStateHTTPHandler_DoesNotLeakErrorTextInBody(t *testing.T) {
	const canary = "CANARY_internal_state_/srv/aiops/orchestrator.go:42"
	handler := stateHTTPHandler(func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, errors.New(canary)
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/state", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
	if strings.Contains(w.Body.String(), canary) {
		t.Fatalf("canary leaked into response body:\n%s", w.Body.String())
	}
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode envelope: %v; body=%s", err, w.Body.String())
	}
	if payload.Error.Code == "" || payload.Error.Message == "" {
		t.Fatalf("envelope = %+v, want code and message set", payload.Error)
	}
}

// TestStateHTTPHandler_DoesNotLeakErrorTextOnCancellation extends the
// no-leak contract to the request_cancelled branch. Even though Go's
// context.Canceled message is stable stdlib text, the principle is the
// same: response body carries safe constants, details land in the log.
func TestStateHTTPHandler_DoesNotLeakErrorTextOnCancellation(t *testing.T) {
	const canary = "CANARY_canceled_with_path_/internal/orchestrator"
	handler := stateHTTPHandler(func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, fmt.Errorf("%s: %w", canary, context.Canceled)
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/state", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	if strings.Contains(w.Body.String(), canary) {
		t.Fatalf("canary leaked into cancelled response body:\n%s", w.Body.String())
	}
}

func TestIssueHTTPHandler_DoesNotLeakErrorTextInBody(t *testing.T) {
	const canary = "CANARY_issue_/srv/aiops/orchestrator.go:99"
	handler := issueHTTPHandler(func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, errors.New(canary)
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/issue-x", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
	if strings.Contains(w.Body.String(), canary) {
		t.Fatalf("canary leaked into response body:\n%s", w.Body.String())
	}
}

func TestRefreshHTTPHandler_DoesNotLeakErrorTextInBody(t *testing.T) {
	const canary = "CANARY_refresh_/srv/aiops/runtime.go:11"
	handler := refreshHTTPHandler(func(context.Context) (orchestrator.RefreshRequestResult, error) {
		return orchestrator.RefreshRequestResult{}, errors.New(canary)
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/refresh", strings.NewReader("{}"))
	req.Header.Set(refreshRequestHeader, refreshRequestHeaderValue)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusInternalServerError, w.Body.String())
	}
	if strings.Contains(w.Body.String(), canary) {
		t.Fatalf("canary leaked into response body:\n%s", w.Body.String())
	}
}

// TestParseRunArgs_PortFlagBoundaries covers SPEC §13.7 --port range.
// The acceptable values are {-1 (disable), 0 (ephemeral), 1..65535}.
// Boundary rule (=N + =N+1) is applied at each cap edge:
//   - lower cap: -1 accepted, -2 rejected
//   - upper cap: 65535 accepted, 65536 rejected
//
// The interior values 0, 1, 4000 confirm the band; 0 is the SPEC-
// blessed "ephemeral" sentinel that the workflow loader rejects (port
// 0 is not allowed in WORKFLOW.md) — CLI is the legitimate path for
// ephemeral. The error message must name the flag so test harnesses
// can distinguish CLI parsing failures from workflow-load failures.
func TestParseRunArgs_PortFlagBoundaries(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantPort *int
		wantErr  string // substring, "" = no error
	}{
		{name: "unset_no_override", args: []string{}, wantPort: nil},
		{name: "below_lower_cap_minus_two", args: []string{"--port=-2"}, wantErr: "--port"},
		{name: "lower_cap_minus_one_disables", args: []string{"--port=-1"}, wantPort: intPtr(-1)},
		{name: "zero_ephemeral", args: []string{"--port=0"}, wantPort: intPtr(0)},
		{name: "one", args: []string{"--port=1"}, wantPort: intPtr(1)},
		{name: "interior_4001", args: []string{"--port=4001"}, wantPort: intPtr(4001)},
		{name: "upper_cap_65535", args: []string{"--port=65535"}, wantPort: intPtr(65535)},
		{name: "above_upper_cap_65536", args: []string{"--port=65536"}, wantErr: "--port"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, port, err := parseRunArgs(tc.args)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("err = nil, want substring %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			switch {
			case tc.wantPort == nil && port != nil:
				t.Fatalf("port = %d, want nil", *port)
			case tc.wantPort != nil && port == nil:
				t.Fatalf("port = nil, want %d", *tc.wantPort)
			case tc.wantPort != nil && *port != *tc.wantPort:
				t.Fatalf("port = %d, want %d", *port, *tc.wantPort)
			}
		})
	}
}

// TestParseRunArgs_PortAfterPositionalPathStillParses pins the
// Codex-flagged regression: `worker /path/WORKFLOW.md --port=4001`
// used to fail because stdlib flag.Parse stops at the first non-flag
// arg. The reorder helper now pulls flag tokens to the front.
// Boundary form: paired-edges on token ordering with the same value
// (=4001) shows up in both positions.
func TestParseRunArgs_PortAfterPositionalPathStillParses(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{name: "path_then_port_equals", args: []string{"/wf.md", "--port=4001"}},
		{name: "path_then_port_split", args: []string{"/wf.md", "--port", "4001"}},
		{name: "port_equals_then_path", args: []string{"--port=4001", "/wf.md"}},
		{name: "port_split_then_path", args: []string{"--port", "4001", "/wf.md"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, port, err := parseRunArgs(tc.args)
			if err != nil {
				t.Fatalf("parseRunArgs(%v): %v", tc.args, err)
			}
			if path != "/wf.md" {
				t.Fatalf("path = %q, want /wf.md", path)
			}
			if port == nil || *port != 4001 {
				t.Fatalf("port = %v, want &4001", port)
			}
		})
	}
}

// TestParseRunArgs_HelpReturnsFlagErrHelp confirms `--help` propagates
// the stdlib sentinel up to main, where normalizeRunError treats it as
// a clean exit. Without this, `worker --help` would be logged as a
// fatal error.
func TestParseRunArgs_HelpReturnsFlagErrHelp(t *testing.T) {
	_, _, err := parseRunArgs([]string{"--help"})
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("err = %v, want flag.ErrHelp", err)
	}
	if normalized := normalizeRunError(err, nil); normalized != nil {
		t.Fatalf("normalizeRunError(flag.ErrHelp) = %v, want nil", normalized)
	}
}

func TestParseDoctorArgs_AllowsTrailingFlags(t *testing.T) {
	opts, err := parseDoctorArgs([]string{"/wf.md", "--mode", "real", "--dashboard-url", "http://127.0.0.1:4000", "--go-test-dir", "/repo"})
	if err != nil {
		t.Fatalf("parseDoctorArgs returned error: %v", err)
	}
	if opts.WorkflowPath != "/wf.md" || opts.Mode != "real" || opts.DashboardURL != "http://127.0.0.1:4000" || opts.GoTestDir != "/repo" {
		t.Fatalf("parseDoctorArgs = %+v", opts)
	}
}

func TestParseDoctorArgs_DeployFlag(t *testing.T) {
	opts, err := parseDoctorArgs([]string{"/wf.md"})
	if err != nil {
		t.Fatalf("parseDoctorArgs(/wf.md) error: %v", err)
	}
	if opts.Deploy != "docker" {
		t.Fatalf("default Deploy = %q; want docker", opts.Deploy)
	}
	opts, err = parseDoctorArgs([]string{"/wf.md", "--deploy", "binary"})
	if err != nil {
		t.Fatalf("parseDoctorArgs(--deploy binary) error: %v", err)
	}
	if opts.Deploy != "binary" {
		t.Fatalf("Deploy = %q; want binary", opts.Deploy)
	}
	if _, err := parseDoctorArgs([]string{"--deploy", "k8s", "/wf.md"}); err == nil {
		t.Fatalf("parseDoctorArgs(--deploy k8s) = nil error; want a validation error")
	}
}

func TestParseDoctorArgs_HelpReturnsFlagErrHelp(t *testing.T) {
	_, err := parseDoctorArgs([]string{"--help"})
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("err = %v, want flag.ErrHelp", err)
	}
	if normalized := normalizeRunError(err, nil); normalized != nil {
		t.Fatalf("normalizeRunError(flag.ErrHelp) = %v, want nil", normalized)
	}
}

// TestParseRunArgs_WorkflowPathStillSupported confirms the existing
// positional contract (worker [path-to-WORKFLOW.md]) is preserved when
// --port is also present, in either order.
func TestParseRunArgs_WorkflowPathStillSupported(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "positional_only", args: []string{"/path/to/WORKFLOW.md"}, want: "/path/to/WORKFLOW.md"},
		{name: "flag_then_path", args: []string{"--port=4001", "/path/to/WORKFLOW.md"}, want: "/path/to/WORKFLOW.md"},
		{name: "path_then_flag_equals", args: []string{"/path/to/WORKFLOW.md", "--port=4001"}, want: "/path/to/WORKFLOW.md"},
		{name: "path_then_flag_split", args: []string{"/path/to/WORKFLOW.md", "--port", "4001"}, want: "/path/to/WORKFLOW.md"},
		{name: "flag_split_then_path", args: []string{"--port", "4001", "/path/to/WORKFLOW.md"}, want: "/path/to/WORKFLOW.md"},
		{name: "dash_dash_terminates_flags", args: []string{"--", "/path/to/WORKFLOW.md"}, want: "/path/to/WORKFLOW.md"},
		{name: "dash_dash_protects_dash_prefixed_path", args: []string{"--", "-workflow.md"}, want: "-workflow.md"},
		{name: "dash_dash_protects_help_as_path", args: []string{"--", "--help"}, want: "--help"},
		{name: "dash_dash_after_flag", args: []string{"--port=4001", "--", "-foo.md"}, want: "-foo.md"},
		{name: "no_args", args: []string{}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, _, err := parseRunArgs(tc.args)
			if err != nil {
				t.Fatalf("parseRunArgs(%v): %v", tc.args, err)
			}
			if path != tc.want {
				t.Fatalf("path = %q, want %q", path, tc.want)
			}
		})
	}
}

// TestParsePrintConfigArgs covers the #375 dispatch contract: a required
// workdir positional plus an optional --port override that shares run
// mode's accepted range, in either token order.
func TestParsePrintConfigArgs(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantWorkdir string
		wantPort    *int
		wantErr     string // substring, "" = no error
	}{
		{name: "workdir_only", args: []string{"/repo"}, wantWorkdir: "/repo", wantPort: nil},
		{name: "workdir_then_port_equals", args: []string{"/repo", "--port=4001"}, wantWorkdir: "/repo", wantPort: intPtr(4001)},
		{name: "port_then_workdir", args: []string{"--port=4001", "/repo"}, wantWorkdir: "/repo", wantPort: intPtr(4001)},
		{name: "port_split_then_workdir", args: []string{"--port", "4001", "/repo"}, wantWorkdir: "/repo", wantPort: intPtr(4001)},
		{name: "port_disable", args: []string{"/repo", "--port=-1"}, wantWorkdir: "/repo", wantPort: intPtr(-1)},
		{name: "port_out_of_range", args: []string{"/repo", "--port=65536"}, wantErr: "--port"},
		{name: "missing_workdir", args: []string{}, wantErr: "usage"},
		{name: "missing_workdir_with_port", args: []string{"--port=4001"}, wantErr: "usage"},
		{name: "too_many_positionals", args: []string{"/repo", "/other"}, wantErr: "usage"},
		{name: "dash_dash_protects_dash_prefixed_workdir", args: []string{"--", "-repo"}, wantWorkdir: "-repo", wantPort: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workdir, port, err := parsePrintConfigArgs(tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePrintConfigArgs(%v): %v", tc.args, err)
			}
			if workdir != tc.wantWorkdir {
				t.Fatalf("workdir = %q, want %q", workdir, tc.wantWorkdir)
			}
			if !reflect.DeepEqual(port, tc.wantPort) {
				t.Fatalf("port = %v, want %v", port, tc.wantPort)
			}
		})
	}
}

// TestDesiredPortForLoop_OverrideWinsOverWorkflow pins SPEC §13.7's
// "CLI --port overrides server.port when both are present" against the
// runStateHTTPServerLoop port-selection step.
func TestDesiredPortForLoop_OverrideWinsOverWorkflow(t *testing.T) {
	override := 4001
	got := desiredPortForLoop(stateHTTPServerLoopOptions{PortOverride: &override}, &workflow.Workflow{
		Config: workflow.Config{Server: workflow.ServerConfig{Port: 4000}},
	})
	if got != 4001 {
		t.Fatalf("desiredPortForLoop = %d, want 4001 (CLI override wins)", got)
	}
}

// TestDesiredPortForLoop_FallsBackToWorkflowWhenNoOverride covers the
// no-override path: workflow value is used verbatim.
func TestDesiredPortForLoop_FallsBackToWorkflowWhenNoOverride(t *testing.T) {
	got := desiredPortForLoop(stateHTTPServerLoopOptions{}, &workflow.Workflow{
		Config: workflow.Config{Server: workflow.ServerConfig{Port: 4000}},
	})
	if got != 4000 {
		t.Fatalf("desiredPortForLoop = %d, want 4000", got)
	}
}

// TestDesiredPortForLoop_OverrideMinusOneDisables — paired edge with
// OverrideWinsOverWorkflow: --port -1 must shut the server down even
// when workflow says enable.
func TestDesiredPortForLoop_OverrideMinusOneDisables(t *testing.T) {
	override := -1
	got := desiredPortForLoop(stateHTTPServerLoopOptions{PortOverride: &override}, &workflow.Workflow{
		Config: workflow.Config{Server: workflow.ServerConfig{Port: 4000}},
	})
	if got != -1 {
		t.Fatalf("desiredPortForLoop = %d, want -1 (CLI disable wins)", got)
	}
}

// TestDesiredPortForLoop_NoWorkflowYieldsDisable mirrors the existing
// runtime-level guard: when runtime.Current() has no workflow snapshot
// yet, the server should stay disabled.
func TestDesiredPortForLoop_NoWorkflowYieldsDisable(t *testing.T) {
	got := desiredPortForLoop(stateHTTPServerLoopOptions{}, nil)
	if got != -1 {
		t.Fatalf("desiredPortForLoop(nil workflow) = %d, want -1", got)
	}
}

// TestDesiredHostForLoop_OverrideWinsOverWorkflow pins AIOPS_SERVER_HOST
// precedence over the workflow snapshot's server.host, mirroring the --port
// override rule.
func TestDesiredHostForLoop_OverrideWinsOverWorkflow(t *testing.T) {
	override := "0.0.0.0"
	got := desiredHostForLoop(stateHTTPServerLoopOptions{HostOverride: &override}, &workflow.Workflow{
		Config: workflow.Config{Server: workflow.ServerConfig{Host: "127.0.0.1"}},
	})
	if got != "0.0.0.0" {
		t.Fatalf("desiredHostForLoop = %q, want 0.0.0.0 (env override wins)", got)
	}
}

// TestDesiredHostForLoop_FallsBackToWorkflow covers the no-override path: the
// workflow server.host is used verbatim.
func TestDesiredHostForLoop_FallsBackToWorkflow(t *testing.T) {
	got := desiredHostForLoop(stateHTTPServerLoopOptions{}, &workflow.Workflow{
		Config: workflow.Config{Server: workflow.ServerConfig{Host: "10.0.0.5"}},
	})
	if got != "10.0.0.5" {
		t.Fatalf("desiredHostForLoop = %q, want 10.0.0.5", got)
	}
}

// TestDesiredHostForLoop_NoWorkflowYieldsEmpty — no override and no snapshot
// yields the empty string, which normalizeServerHost maps to loopback.
func TestDesiredHostForLoop_NoWorkflowYieldsEmpty(t *testing.T) {
	if got := desiredHostForLoop(stateHTTPServerLoopOptions{}, nil); got != "" {
		t.Fatalf("desiredHostForLoop(nil) = %q, want empty", got)
	}
}

func TestServerHostOverrideFromEnv(t *testing.T) {
	t.Setenv("AIOPS_SERVER_HOST", "0.0.0.0")
	if got := serverHostOverrideFromEnv(); got == nil || *got != "0.0.0.0" {
		t.Fatalf("serverHostOverrideFromEnv() = %v, want 0.0.0.0", got)
	}
	// Set-but-empty is an explicit override: it must stay non-nil so
	// `AIOPS_SERVER_HOST=` forces the loopback default over any workflow value.
	t.Setenv("AIOPS_SERVER_HOST", "")
	if got := serverHostOverrideFromEnv(); got == nil || *got != "" {
		t.Fatalf("serverHostOverrideFromEnv() with empty env = %v, want non-nil empty override", got)
	}
}

// TestServerHostOverrideUnsetYieldsNil — the unset case must fall through to the
// workflow value (nil), distinct from the set-but-empty force-loopback case.
func TestServerHostOverrideUnsetYieldsNil(t *testing.T) {
	t.Setenv("AIOPS_SERVER_HOST", "x")
	if err := os.Unsetenv("AIOPS_SERVER_HOST"); err != nil {
		t.Fatalf("Unsetenv(%q) = %v; want nil", "AIOPS_SERVER_HOST", err)
	}
	if got := serverHostOverrideFromEnv(); got != nil {
		t.Fatalf("serverHostOverrideFromEnv() unset = %v, want nil", got)
	}
}

// TestNewStateHTTPServerBindsConfiguredHost guards that the configured host
// reaches the listen address, and that an empty host normalizes to loopback
// rather than net.Listen's bind-all wildcard.
func TestNewStateHTTPServerBindsConfiguredHost(t *testing.T) {
	snap := func(context.Context) (orchestrator.StateView, error) { return orchestrator.StateView{}, nil }
	cases := []struct {
		host string
		want string
	}{
		{"0.0.0.0", "0.0.0.0:4000"},
		{"127.0.0.1", "127.0.0.1:4000"},
		{"", "127.0.0.1:4000"},
		{"::1", "[::1]:4000"},
	}
	for _, c := range cases {
		if got := newStateHTTPServer(c.host, 4000, snap).Addr; got != c.want {
			t.Errorf("newStateHTTPServer(%q, 4000).Addr = %q, want %q", c.host, got, c.want)
		}
	}
}

// TestStartStateHTTPServerHonorsWiderBind proves AIOPS_SERVER_HOST=0.0.0.0
// actually binds the unspecified address (the Compose reachability fix), while
// the loopback Host-header guard still rejects non-loopback Hosts.
func TestStartStateHTTPServerHonorsWiderBind(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handle := startStateHTTPServer(ctx, "0.0.0.0", 0, func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, nil
	}, stateHTTPAlwaysReady)
	if handle == nil {
		t.Fatal("handle = nil, want running server bound to 0.0.0.0")
	}
	tcpAddr, ok := handle.Addr.(*net.TCPAddr)
	if !ok {
		t.Fatalf("addr = %T, want TCP", handle.Addr)
	}
	if !tcpAddr.IP.IsUnspecified() {
		t.Fatalf("bind IP = %s, want unspecified (0.0.0.0)", tcpAddr.IP)
	}
	cancel()
	select {
	case <-handle.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop after cancel")
	}
}

func intPtr(v int) *int { return &v }

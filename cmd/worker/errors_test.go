package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/orchestrator"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func TestTrackerClientForWorkflowSurfacesUnsupportedTrackerCategory(t *testing.T) {
	cfg := workflow.DefaultConfig()
	cfg.Tracker.Kind = "jira"
	_, err := trackerClientForWorkflow(cfg)
	if !errors.Is(err, tracker.ErrUnsupportedTrackerKind) {
		t.Fatalf("trackerClientForWorkflow error = %T %[1]v, want ErrUnsupportedTrackerKind", err)
	}
}

func TestStateHTTPHandlerUsesCategoryErrorEnvelope(t *testing.T) {
	handler := stateHTTPHandler(func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, tracker.NewError(tracker.CategoryLinearUnknownPayload, "bad tracker payload", nil)
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/state", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status code = %d, want %d; body=%s", w.Code, http.StatusInternalServerError, w.Body.String())
	}
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode error envelope: %v; body=%s", err, w.Body.String())
	}
	if payload.Error.Code != string(tracker.CategoryLinearUnknownPayload) || payload.Error.Message == "" {
		t.Fatalf("error envelope = %+v, want category code and message", payload.Error)
	}
}

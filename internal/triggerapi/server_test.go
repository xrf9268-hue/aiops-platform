package triggerapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
)

func TestTaskReadHandlers(t *testing.T) {
	now := time.Date(2026, 4, 28, 10, 0, 0, 0, time.UTC)
	store := &fakeTaskStore{
		tasks: []task.Task{
			{
				ID:            "tsk_queued",
				Status:        task.StatusQueued,
				SourceType:    "manual",
				SourceEventID: "manual-queued",
				RepoOwner:     "octo",
				RepoName:      "demo",
				CloneURL:      "git@example.com:octo/demo.git",
				BaseBranch:    "main",
				WorkBranch:    "ai/tsk_queued",
				Title:         "Queued task",
				Description:   "Queued description",
				Actor:         "tester",
				Model:         "mock",
				Priority:      50,
				MaxAttempts:   3,
				CreatedAt:     now,
				UpdatedAt:     now,
			},
			{
				ID:            "tsk_running",
				Status:        task.StatusRunning,
				SourceType:    "manual",
				SourceEventID: "manual-running",
				RepoOwner:     "octo",
				RepoName:      "demo",
				CloneURL:      "git@example.com:octo/demo.git",
				BaseBranch:    "main",
				WorkBranch:    "ai/tsk_running",
				Title:         "Running task",
				Description:   "Running description",
				Actor:         "tester",
				Model:         "mock",
				Priority:      50,
				MaxAttempts:   3,
				CreatedAt:     now,
				UpdatedAt:     now,
			},
		},
		events: map[string][]task.Event{
			"tsk_queued": {
				{ID: 1, TaskID: "tsk_queued", EventType: "enqueued", Message: "task accepted", CreatedAt: now},
				{ID: 2, TaskID: "tsk_queued", EventType: "claimed", Message: "worker claimed task", CreatedAt: now},
			},
		},
	}
	handler := Routes(&Server{store: store})

	t.Run("get task by id", func(t *testing.T) {
		res := requestJSON(t, handler, "GET", "/v1/tasks/tsk_queued")
		if res.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body=%s", res.Code, http.StatusOK, res.Body.String())
		}
		var got task.Task
		decodeJSON(t, res, &got)
		if got.ID != "tsk_queued" || got.RepoOwner != "octo" {
			t.Fatalf("task = %+v, want queued task with repo owner", got)
		}
	})

	t.Run("get task events", func(t *testing.T) {
		res := requestJSON(t, handler, "GET", "/v1/tasks/tsk_queued/events")
		if res.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body=%s", res.Code, http.StatusOK, res.Body.String())
		}
		var got []task.Event
		decodeJSON(t, res, &got)
		if len(got) != 2 || got[0].EventType != "enqueued" {
			t.Fatalf("events = %+v, want enqueued and claimed", got)
		}
	})

	t.Run("list queued tasks", func(t *testing.T) {
		res := requestJSON(t, handler, "GET", "/v1/tasks?status=queued")
		if res.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body=%s", res.Code, http.StatusOK, res.Body.String())
		}
		var got []task.Task
		decodeJSON(t, res, &got)
		if len(got) != 1 || got[0].ID != "tsk_queued" {
			t.Fatalf("tasks = %+v, want only queued task", got)
		}
	})

	t.Run("missing task returns 404", func(t *testing.T) {
		res := requestJSON(t, handler, "GET", "/v1/tasks/missing")
		if res.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d; body=%s", res.Code, http.StatusNotFound, res.Body.String())
		}
	})
}

type fakeTaskStore struct {
	tasks  []task.Task
	events map[string][]task.Event
}

func (f *fakeTaskStore) Enqueue(context.Context, task.Task) (task.Task, bool, error) {
	panic("not used")
}

func (f *fakeTaskStore) GetTask(_ context.Context, id string) (task.Task, error) {
	for _, t := range f.tasks {
		if t.ID == id {
			return t, nil
		}
	}
	return task.Task{}, task.ErrNotFound
}

func (f *fakeTaskStore) ListTasks(_ context.Context, status task.Status) ([]task.Task, error) {
	var out []task.Task
	for _, t := range f.tasks {
		if status == "" || t.Status == status {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *fakeTaskStore) TaskEvents(_ context.Context, id string) ([]task.Event, error) {
	return f.events[id], nil
}

func requestJSON(t *testing.T, handler http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	return res
}

func decodeJSON(t *testing.T, res *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.Unmarshal(res.Body.Bytes(), out); err != nil {
		t.Fatalf("decode response %q: %v", res.Body.String(), err)
	}
}

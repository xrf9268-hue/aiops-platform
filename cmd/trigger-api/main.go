package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xrf9268-hue/aiops-platform/internal/gitea"
	"github.com/xrf9268-hue/aiops-platform/internal/queue"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
)

type server struct {
	store  taskStore
	secret string
}

type taskStore interface {
	Enqueue(context.Context, task.Task) (task.Task, bool, error)
	GetTask(context.Context, string) (task.Task, error)
	ListTasks(context.Context, task.Status) ([]task.Task, error)
	TaskEvents(context.Context, string) ([]task.Event, error)
}

func main() {
	ctx := context.Background()
	dsn := env("DATABASE_URL", "postgres://aiops:aiops@localhost:5432/aiops?sslmode=disable")
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	s := &server{store: queue.New(pool), secret: os.Getenv("GITEA_WEBHOOK_SECRET")}

	addr := env("ADDR", ":8080")
	log.Printf("trigger-api listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, routes(s)))
}

func routes(s *server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]any{"ok": true}) })
	mux.HandleFunc("POST /v1/events/gitea", s.handleGitea)
	mux.HandleFunc("POST /v1/tasks", s.handleManualTask)
	mux.HandleFunc("GET /v1/tasks", s.handleListTasks)
	mux.HandleFunc("GET /v1/tasks/{id}", s.handleGetTask)
	mux.HandleFunc("GET /v1/tasks/{id}/events", s.handleTaskEvents)
	return mux
}

func (s *server) handleGitea(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if !gitea.VerifySignature(s.secret, r.Header.Get("X-Gitea-Signature"), body) {
		http.Error(w, "bad signature", 401)
		return
	}
	if r.Header.Get("X-Gitea-Event") != "issue_comment" {
		writeJSON(w, 202, map[string]any{"accepted": true, "ignored": true, "reason": "not issue_comment"})
		return
	}
	p, err := gitea.ParseIssueComment(body)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if !gitea.HasAIRunCommand(p.Comment.Body) {
		writeJSON(w, 202, map[string]any{"accepted": true, "ignored": true, "reason": "no /ai-run"})
		return
	}
	owner, repo := gitea.SplitOwnerRepo(p.Repository.FullName)
	clone := p.Repository.SSHURL
	if clone == "" {
		clone = p.Repository.CloneURL
	}
	base := p.Repository.DefaultBranch
	if base == "" {
		base = "main"
	}
	delivery := r.Header.Get("X-Gitea-Delivery")
	if delivery == "" {
		delivery = fmt.Sprintf("comment-%d", p.Comment.ID)
	}

	out, deduped, err := s.store.Enqueue(r.Context(), task.Task{
		SourceType:    "gitea_issue_comment",
		SourceEventID: delivery,
		RepoOwner:     owner,
		RepoName:      repo,
		CloneURL:      clone,
		BaseBranch:    base,
		Title:         fmt.Sprintf("AI run for issue #%d: %s", p.Issue.Number, p.Issue.Title),
		Description:   fmt.Sprintf("Issue #%d requested by %s\n\n%s", p.Issue.Number, p.Sender.Login, p.Issue.Body),
		Actor:         p.Sender.Login,
		Model:         "mock",
		Priority:      50,
	})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, 202, map[string]any{"accepted": true, "task_id": out.ID, "deduped": deduped})
}

func (s *server) handleManualTask(w http.ResponseWriter, r *http.Request) {
	var in task.Task
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if in.SourceType == "" {
		in.SourceType = "manual"
	}
	if in.SourceEventID == "" {
		in.SourceEventID = fmt.Sprintf("manual-%d", time.Now().UnixNano())
	}
	out, deduped, err := s.store.Enqueue(r.Context(), in)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, 202, map[string]any{"task_id": out.ID, "deduped": deduped})
}

func (s *server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.GetTask(r.Context(), r.PathValue("id"))
	if errors.Is(err, task.ErrNotFound) {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleTaskEvents(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.TaskEvents(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.ListTasks(r.Context(), task.Status(r.URL.Query().Get("status")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

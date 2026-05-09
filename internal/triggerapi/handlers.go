package triggerapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/gitea"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
)

func (s *Server) handleGitea(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) handleManualTask(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) handleTaskEvents(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.TaskEvents(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.ListTasks(r.Context(), task.Status(r.URL.Query().Get("status")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

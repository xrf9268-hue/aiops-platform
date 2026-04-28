package queue

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
)

type Store struct{ db *pgxpool.Pool }

func New(db *pgxpool.Pool) *Store { return &Store{db: db} }

func (s *Store) Enqueue(ctx context.Context, t task.Task) (task.Task, bool, error) {
	if t.ID == "" {
		t.ID = fmt.Sprintf("tsk_%d", time.Now().UnixNano())
	}
	if t.WorkBranch == "" {
		t.WorkBranch = "ai/" + t.ID
	}
	if t.Model == "" {
		t.Model = "mock"
	}
	if t.Priority == 0 {
		t.Priority = 50
	}
	if t.MaxAttempts == 0 {
		t.MaxAttempts = 3
	}
	row := s.db.QueryRow(ctx, `
INSERT INTO tasks (id,status,source_type,source_event_id,repo_owner,repo_name,clone_url,base_branch,work_branch,title,description,actor,model,priority,max_attempts)
VALUES ($1,'queued',$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
ON CONFLICT (source_type, source_event_id) DO UPDATE SET updated_at=tasks.updated_at
RETURNING id,status,source_type,source_event_id,repo_owner,repo_name,clone_url,base_branch,work_branch,title,description,actor,model,priority,attempts,max_attempts,available_at,created_at,updated_at, (xmax <> 0) AS deduped`,
		t.ID, t.SourceType, t.SourceEventID, t.RepoOwner, t.RepoName, t.CloneURL, t.BaseBranch, t.WorkBranch, t.Title, t.Description, t.Actor, t.Model, t.Priority, t.MaxAttempts)
	var out task.Task
	var deduped bool
	err := row.Scan(&out.ID, &out.Status, &out.SourceType, &out.SourceEventID, &out.RepoOwner, &out.RepoName, &out.CloneURL, &out.BaseBranch, &out.WorkBranch, &out.Title, &out.Description, &out.Actor, &out.Model, &out.Priority, &out.Attempts, &out.MaxAttempts, &out.AvailableAt, &out.CreatedAt, &out.UpdatedAt, &deduped)
	if err != nil {
		return task.Task{}, false, err
	}
	_ = s.AddEvent(ctx, out.ID, "enqueued", "task accepted")
	return out, deduped, nil
}

func (s *Store) Claim(ctx context.Context) (*task.Task, error) {
	row := s.db.QueryRow(ctx, `
WITH cte AS (
  SELECT id FROM tasks
  WHERE status='queued' AND available_at <= now()
  ORDER BY priority DESC, created_at ASC
  FOR UPDATE SKIP LOCKED LIMIT 1
)
UPDATE tasks t SET status='running', attempts=attempts+1, updated_at=now()
FROM cte WHERE t.id=cte.id
RETURNING t.id,t.status,t.source_type,t.source_event_id,t.repo_owner,t.repo_name,t.clone_url,t.base_branch,t.work_branch,t.title,t.description,t.actor,t.model,t.priority,t.attempts,t.max_attempts,t.available_at,t.created_at,t.updated_at`)
	var out task.Task
	err := row.Scan(&out.ID, &out.Status, &out.SourceType, &out.SourceEventID, &out.RepoOwner, &out.RepoName, &out.CloneURL, &out.BaseBranch, &out.WorkBranch, &out.Title, &out.Description, &out.Actor, &out.Model, &out.Priority, &out.Attempts, &out.MaxAttempts, &out.AvailableAt, &out.CreatedAt, &out.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = s.AddEvent(ctx, out.ID, "claimed", "worker claimed task")
	return &out, nil
}

func (s *Store) GetTask(ctx context.Context, id string) (task.Task, error) {
	row := s.db.QueryRow(ctx, `
	SELECT id,status,source_type,source_event_id,repo_owner,repo_name,clone_url,base_branch,work_branch,title,description,actor,model,priority,attempts,max_attempts,available_at,created_at,updated_at
	FROM tasks WHERE id=$1`, id)
	out, err := scanTask(row)
	if err == pgx.ErrNoRows {
		return task.Task{}, task.ErrNotFound
	}
	return out, err
}

func (s *Store) ListTasks(ctx context.Context, status task.Status) ([]task.Task, error) {
	var rows pgx.Rows
	var err error
	if status == "" {
		rows, err = s.db.Query(ctx, `
	SELECT id,status,source_type,source_event_id,repo_owner,repo_name,clone_url,base_branch,work_branch,title,description,actor,model,priority,attempts,max_attempts,available_at,created_at,updated_at
	FROM tasks ORDER BY created_at DESC LIMIT 100`)
	} else {
		rows, err = s.db.Query(ctx, `
	SELECT id,status,source_type,source_event_id,repo_owner,repo_name,clone_url,base_branch,work_branch,title,description,actor,model,priority,attempts,max_attempts,available_at,created_at,updated_at
	FROM tasks WHERE status=$1 ORDER BY created_at DESC LIMIT 100`, status)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []task.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) TaskEvents(ctx context.Context, taskID string) ([]task.Event, error) {
	rows, err := s.db.Query(ctx, `
	SELECT id,task_id,event_type,coalesce(message,''),coalesce(payload,'{}'::jsonb),created_at
	FROM task_events WHERE task_id=$1 ORDER BY id ASC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []task.Event
	for rows.Next() {
		var event task.Event
		if err := rows.Scan(&event.ID, &event.TaskID, &event.EventType, &event.Message, &event.Payload, &event.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

func (s *Store) Complete(ctx context.Context, id string) error {
	_, err := s.db.Exec(ctx, "UPDATE tasks SET status='succeeded', updated_at=now() WHERE id=$1", id)
	if err == nil {
		_ = s.AddEvent(ctx, id, "succeeded", "task completed")
	}
	return err
}

func (s *Store) Fail(ctx context.Context, id string, msg string) error {
	_, err := s.db.Exec(ctx, `UPDATE tasks SET status=CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'queued' END, available_at=now()+interval '60 seconds', updated_at=now() WHERE id=$1`, id)
	if err == nil {
		_ = s.AddEvent(ctx, id, "failed_attempt", msg)
	}
	return err
}

func (s *Store) AddEvent(ctx context.Context, taskID, typ, msg string) error {
	_, err := s.db.Exec(ctx, "INSERT INTO task_events(task_id,event_type,message) VALUES($1,$2,$3)", taskID, typ, msg)
	return err
}

type taskScanner interface {
	Scan(dest ...any) error
}

func scanTask(row taskScanner) (task.Task, error) {
	var out task.Task
	err := row.Scan(&out.ID, &out.Status, &out.SourceType, &out.SourceEventID, &out.RepoOwner, &out.RepoName, &out.CloneURL, &out.BaseBranch, &out.WorkBranch, &out.Title, &out.Description, &out.Actor, &out.Model, &out.Priority, &out.Attempts, &out.MaxAttempts, &out.AvailableAt, &out.CreatedAt, &out.UpdatedAt)
	return out, err
}

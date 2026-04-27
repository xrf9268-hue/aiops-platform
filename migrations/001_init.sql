CREATE TABLE IF NOT EXISTS tasks (
  id TEXT PRIMARY KEY,
  status TEXT NOT NULL,
  source_type TEXT NOT NULL,
  source_event_id TEXT NOT NULL,
  repo_owner TEXT NOT NULL,
  repo_name TEXT NOT NULL,
  clone_url TEXT NOT NULL,
  base_branch TEXT NOT NULL,
  work_branch TEXT NOT NULL,
  title TEXT NOT NULL,
  description TEXT NOT NULL,
  actor TEXT NOT NULL,
  model TEXT NOT NULL DEFAULT 'mock',
  priority INT NOT NULL DEFAULT 50,
  attempts INT NOT NULL DEFAULT 0,
  max_attempts INT NOT NULL DEFAULT 3,
  available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(source_type, source_event_id)
);

CREATE INDEX IF NOT EXISTS idx_tasks_claim
  ON tasks(status, priority DESC, created_at ASC)
  WHERE status = 'queued';

CREATE TABLE IF NOT EXISTS task_events (
  id BIGSERIAL PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(id),
  event_type TEXT NOT NULL,
  message TEXT,
  payload JSONB,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

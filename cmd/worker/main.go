package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xrf9268-hue/aiops-platform/internal/queue"
	"github.com/xrf9268-hue/aiops-platform/internal/runner"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
)

func main() {
	ctx := context.Background()
	dsn := env("DATABASE_URL", "postgres://aiops:aiops@localhost:5432/aiops?sslmode=disable")
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil { log.Fatal(err) }
	defer pool.Close()
	store := queue.New(pool)

	for {
		t, err := store.Claim(ctx)
		if err != nil { log.Printf("claim error: %v", err); time.Sleep(5*time.Second); continue }
		if t == nil { time.Sleep(3*time.Second); continue }
		if err := runTask(ctx, *t); err != nil {
			log.Printf("task %s failed: %v", t.ID, err)
			_ = store.Fail(ctx, t.ID, err.Error())
			continue
		}
		_ = store.Complete(ctx, t.ID)
	}
}

func runTask(ctx context.Context, t task.Task) error {
	root := env("WORKSPACE_ROOT", "/tmp/aiops-workspaces")
	workdir := filepath.Join(root, t.ID)
	_ = os.RemoveAll(workdir)
	if err := os.MkdirAll(root, 0o755); err != nil { return err }
	if err := run(ctx, root, "git", "clone", t.CloneURL, workdir); err != nil { return err }
	if err := run(ctx, workdir, "git", "checkout", t.BaseBranch); err != nil { return err }
	if err := run(ctx, workdir, "git", "checkout", "-b", t.WorkBranch); err != nil { return err }
	if err := writeTaskFiles(workdir, t); err != nil { return err }

	switch t.Model {
	case "mock", "":
		if err := runner.RunMock(workdir, t); err != nil { return err }
	default:
		return fmt.Errorf("unsupported model in Day 1 worker: %s", t.Model)
	}

	if err := run(ctx, workdir, "git", "add", "."); err != nil { return err }
	if err := run(ctx, workdir, "git", "commit", "-m", "chore(ai): "+t.Title); err != nil { return err }
	if err := run(ctx, workdir, "git", "push", "origin", t.WorkBranch); err != nil { return err }
	log.Printf("task %s pushed branch %s; create PR manually or enable Gitea PR adapter next", t.ID, t.WorkBranch)
	return nil
}

func writeTaskFiles(workdir string, t task.Task) error {
	dir := filepath.Join(workdir, ".aiops")
	if err := os.MkdirAll(dir, 0o755); err != nil { return err }
	body := fmt.Sprintf("# Task %s\n\n%s\n\n## Acceptance\n- Keep changes minimal.\n- Do not touch protected paths.\n- Create a reviewable PR.\n", t.ID, t.Description)
	return os.WriteFile(filepath.Join(dir, "TASK.md"), []byte(body), 0o644)
}

func run(ctx context.Context, dir string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func env(k, d string) string { if v := os.Getenv(k); v != "" { return v }; return d }

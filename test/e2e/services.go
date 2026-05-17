//go:build e2e

package e2e

import (
	"context"
	"os"
	"testing"
	"time"
)

type testbed struct {
	pg     *pgEnv
	gitea  *giteaEnv
	cancel context.CancelFunc
}

func setupTestbed(ctx context.Context) (*testbed, error) {
	pg, err := startPostgres(ctx)
	if err != nil {
		return nil, err
	}

	g, err := startGitea(ctx)
	if err != nil {
		pg.close(context.Background())
		return nil, err
	}

	_, cancel := context.WithCancel(ctx)
	return &testbed{
		pg:     pg,
		gitea:  g,
		cancel: cancel,
	}, nil
}

func (b *testbed) close(ctx context.Context) {
	b.cancel()
	b.gitea.close(ctx)
	b.pg.close(ctx)
}

// resetState deletes only rows produced after testStart, leaving rows from
// earlier tests (which their own cleanup should have handled) untouched.
// Uses DELETE rather than TRUNCATE to avoid ACCESS EXCLUSIVE deadlocks.
//
// Test isolation here assumes sequential execution. Do NOT add t.Parallel()
// to any test in this package without rethinking the row-window strategy.
func (b *testbed) resetState(t *testing.T, testStart time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := b.pg.pool.Exec(ctx,
		`DELETE FROM task_events WHERE task_id IN (SELECT id FROM tasks WHERE created_at >= $1)`,
		testStart); err != nil {
		t.Fatalf("reset task_events: %v", err)
	}
	if _, err := b.pg.pool.Exec(ctx,
		`DELETE FROM tasks WHERE created_at >= $1`, testStart); err != nil {
		t.Fatalf("reset tasks: %v", err)
	}
}

func tmpDir() string {
	d, err := os.MkdirTemp("", "aiops-e2e-*")
	if err != nil {
		panic(err)
	}
	return d
}

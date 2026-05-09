//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

type pgEnv struct {
	pool      *pgxpool.Pool
	dsn       string
	container testcontainers.Container
}

func startPostgres(ctx context.Context) (*pgEnv, error) {
	migration, err := os.ReadFile("../../migrations/001_init.sql")
	if err != nil {
		return nil, fmt.Errorf("read migration: %w", err)
	}

	c, err := tcpostgres.Run(ctx,
		"postgres:16",
		tcpostgres.WithDatabase("aiops"),
		tcpostgres.WithUsername("aiops"),
		tcpostgres.WithPassword("aiops"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("ready to accept connections").WithOccurrence(2),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("start postgres: %w", err)
	}

	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, err
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, err
	}

	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		pool.Close()
		_ = c.Terminate(ctx)
		return nil, fmt.Errorf("apply migration: %w", err)
	}

	return &pgEnv{pool: pool, dsn: dsn, container: c}, nil
}

func (p *pgEnv) close(ctx context.Context) {
	p.pool.Close()
	_ = p.container.Terminate(ctx)
}

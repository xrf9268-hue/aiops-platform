package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xrf9268-hue/aiops-platform/internal/queue"
	"github.com/xrf9268-hue/aiops-platform/internal/worker"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "--print-config" {
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: worker --print-config <workdir>")
			os.Exit(2)
		}
		os.Exit(worker.PrintConfig(os.Args[2], os.Stdout, os.Stderr))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := worker.LoadConfigFromEnv()
	pool, err := pgxpool.New(ctx, cfg.DSN)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	worker.Run(ctx, queue.New(pool), cfg)
}

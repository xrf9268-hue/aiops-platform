package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xrf9268-hue/aiops-platform/internal/queue"
	"github.com/xrf9268-hue/aiops-platform/internal/triggerapi"
)

func main() {
	ctx := context.Background()
	dsn := env("DATABASE_URL", "postgres://aiops:aiops@localhost:5432/aiops?sslmode=disable")
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	s := triggerapi.NewServer(queue.New(pool), os.Getenv("GITEA_WEBHOOK_SECRET"))

	addr := env("ADDR", ":8080")
	log.Printf("trigger-api listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, triggerapi.Routes(s)))
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

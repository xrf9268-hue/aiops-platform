package fixtures_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComposeHasNoLegacyPostgresOrPollerCredentials(t *testing.T) {
	compose := readCompose(t)
	services, ok := compose["services"].(map[string]any)
	if !ok {
		t.Fatal("compose file missing services map")
	}
	for _, name := range []string{"postgres", "linear-poller", "gitea-poller"} {
		if _, ok := services[name]; ok {
			t.Fatalf("compose still defines removed legacy queue service %q", name)
		}
	}
	worker := service(t, compose, "worker")
	env, ok := worker["environment"].(map[string]any)
	if !ok {
		t.Fatalf("worker.environment = %#v, want map", worker["environment"])
	}
	for _, key := range []string{"DATABASE_URL", "POSTGRES_PASSWORD", "PGPASSWORD"} {
		if _, ok := env[key]; ok {
			t.Fatalf("worker environment still contains legacy queue credential %s", key)
		}
	}

	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "docker-compose.yml"))
	if err != nil {
		t.Fatalf("read compose: %v", err)
	}
	normalized := strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToLower(string(raw)))
	for _, token := range []string{"linearpoller", "giteapoller", "postgres", "databaseurl", "postgrespassword", "pgpassword"} {
		if strings.Contains(normalized, token) {
			t.Fatalf("compose still contains legacy queue token variant %q", token)
		}
	}
}

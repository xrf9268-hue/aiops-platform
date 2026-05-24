package fixtures_test

import (
	"strings"
	"testing"
)

// TestLegacyPostgresHasNoHardcodedPassword guards #371: the legacy-queue
// postgres service must not ship a hardcoded password and must source it from
// the environment, so there is no weak default to leak.
func TestLegacyPostgresHasNoHardcodedPassword(t *testing.T) {
	pg := service(t, readCompose(t), "postgres")
	env, ok := pg["environment"].(map[string]any)
	if !ok {
		t.Fatalf("postgres.environment = %#v, want map", pg["environment"])
	}
	pw, _ := env["POSTGRES_PASSWORD"].(string)
	if pw == "" {
		t.Fatal("postgres has no POSTGRES_PASSWORD entry")
	}
	if pw == "aiops" {
		t.Error("postgres POSTGRES_PASSWORD is the hardcoded default 'aiops'")
	}
	if !strings.Contains(pw, "POSTGRES_PASSWORD") {
		t.Errorf("postgres POSTGRES_PASSWORD = %q, want an env-sourced ${POSTGRES_PASSWORD...} value", pw)
	}
}

// TestLegacyPostgresBindsLoopbackOnly guards that the postgres host port is
// published only on loopback, never all interfaces (#371).
func TestLegacyPostgresBindsLoopbackOnly(t *testing.T) {
	pg := service(t, readCompose(t), "postgres")
	ports, ok := pg["ports"].([]any)
	if !ok || len(ports) == 0 {
		t.Fatalf("postgres.ports = %#v, want non-empty list", pg["ports"])
	}
	for _, raw := range ports {
		mapping, ok := raw.(string)
		if !ok {
			t.Fatalf("port mapping %#v is not a string", raw)
		}
		parts := strings.Split(mapping, ":")
		if len(parts) != 3 {
			t.Errorf("port mapping %q must pin a host IP (HOST_IP:HOST_PORT:CONTAINER_PORT)", mapping)
			continue
		}
		if parts[0] != "127.0.0.1" && parts[0] != "::1" {
			t.Errorf("postgres port %q publishes on %q; expected loopback", mapping, parts[0])
		}
	}
}

// TestLegacyPollerDBURLsHaveNoHardcodedPassword guards that the poller
// DATABASE_URLs no longer embed the weak aiops:aiops credentials (#371).
func TestLegacyPollerDBURLsHaveNoHardcodedPassword(t *testing.T) {
	compose := readCompose(t)
	for _, name := range []string{"linear-poller", "gitea-poller"} {
		env, ok := service(t, compose, name)["environment"].(map[string]any)
		if !ok {
			t.Fatalf("%s.environment = %#v, want map", name, service(t, compose, name)["environment"])
		}
		url, _ := env["DATABASE_URL"].(string)
		if strings.Contains(url, ":aiops@") {
			t.Errorf("%s DATABASE_URL embeds the hardcoded aiops password: %q", name, url)
		}
		if !strings.Contains(url, "POSTGRES_PASSWORD") {
			t.Errorf("%s DATABASE_URL = %q, want the password sourced from ${POSTGRES_PASSWORD...}", name, url)
		}
	}
}

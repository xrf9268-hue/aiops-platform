package fixtures_test

import (
	"net"
	"strings"
	"testing"
)

// publishedHostIP (Compose port-mapping host-IP parser) is shared from
// dashboard_overlay_test.go in this package.

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
		hostIP, parsed := publishedHostIP(mapping)
		if !parsed {
			t.Fatalf("could not parse port mapping %q", mapping)
		}
		// An empty host IP means the bare HOST_PORT:CONTAINER_PORT form, which
		// publishes on every host interface — exactly what we must forbid.
		ip := net.ParseIP(hostIP)
		if ip == nil || !ip.IsLoopback() {
			t.Errorf("postgres port %q publishes on host IP %q; expected a loopback address", mapping, hostIP)
		}
	}
}

// TestLegacyPollerDBURLsHaveNoEmbeddedPassword guards that the pollers no
// longer embed a password in the DSN (#371): the weak aiops password is gone,
// and the password is supplied via PGPASSWORD (libpq env) so URL-reserved
// characters in a strong secret are not mangled.
func TestLegacyPollerDBURLsHaveNoEmbeddedPassword(t *testing.T) {
	compose := readCompose(t)
	for _, name := range []string{"linear-poller", "gitea-poller"} {
		env, ok := service(t, compose, name)["environment"].(map[string]any)
		if !ok {
			t.Fatalf("%s.environment = %#v, want map", name, service(t, compose, name)["environment"])
		}
		url, _ := env["DATABASE_URL"].(string)
		// userinfo must be the bare user with no ":password@" component.
		if strings.Contains(url, ":aiops@") {
			t.Errorf("%s DATABASE_URL embeds the hardcoded aiops password: %q", name, url)
		}
		if at := strings.Index(url, "@"); at >= 0 {
			if scheme := strings.Index(url, "://"); scheme >= 0 && strings.Contains(url[scheme+3:at], ":") {
				t.Errorf("%s DATABASE_URL embeds a password in the DSN: %q (use PGPASSWORD)", name, url)
			}
		}
		pgpw, _ := env["PGPASSWORD"].(string)
		if !strings.Contains(pgpw, "POSTGRES_PASSWORD") {
			t.Errorf("%s PGPASSWORD = %q, want the password sourced from ${POSTGRES_PASSWORD...}", name, pgpw)
		}
	}
}

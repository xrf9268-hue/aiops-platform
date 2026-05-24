package fixtures_test

import (
	"net"
	"path/filepath"
	"strings"
	"testing"
)

// publishedHostIP extracts the host-side IP of a Compose short-syntax port
// mapping ("[HOST_IP:]HOST_PORT:CONTAINER_PORT[/proto]"). It returns ("", true)
// for the bare "HOST_PORT:CONTAINER_PORT" form, which binds every host
// interface, and handles bracketed/unbracketed IPv6 host IPs.
func publishedHostIP(mapping string) (ip string, ok bool) {
	m := mapping
	if i := strings.IndexByte(m, '/'); i >= 0 {
		m = m[:i]
	}
	if strings.HasPrefix(m, "[") {
		end := strings.Index(m, "]")
		if end < 0 {
			return "", false
		}
		return m[1:end], true
	}
	parts := strings.Split(m, ":")
	switch {
	case len(parts) < 2:
		return "", false
	case len(parts) == 2:
		// HOST_PORT:CONTAINER_PORT — no host IP pinned (binds all interfaces).
		return "", true
	default:
		// HOST_IP:HOST_PORT:CONTAINER_PORT; the trailing two fields are ports,
		// any leading fields are an unbracketed IPv6 host IP.
		return strings.Join(parts[:len(parts)-2], ":"), true
	}
}

func readDashboardOverlay(t *testing.T) map[string]any {
	t.Helper()
	return loadYAML(t, filepath.Join("..", "..", "..", "deploy", "docker-compose.dashboard.yml"))
}

// TestDashboardOverlayBindsContainerWide guards that the opt-in overlay sets
// AIOPS_SERVER_HOST=0.0.0.0 so Docker's published port reaches the in-container
// server (#366). Without it the dashboard stays unreachable under Compose.
func TestDashboardOverlayBindsContainerWide(t *testing.T) {
	worker := service(t, readDashboardOverlay(t), "worker")
	env, ok := worker["environment"].(map[string]any)
	if !ok {
		t.Fatalf("worker.environment = %#v, want map", worker["environment"])
	}
	if got := env["AIOPS_SERVER_HOST"]; got != "0.0.0.0" {
		t.Fatalf("AIOPS_SERVER_HOST = %v, want 0.0.0.0", got)
	}
}

// TestDashboardOverlayPublishesToHostLoopbackOnly guards the security property:
// the host side of every published port must bind loopback, so the dashboard
// (unauthenticated, SPEC §15.3) is never exposed on a routable host interface.
func TestDashboardOverlayPublishesToHostLoopbackOnly(t *testing.T) {
	worker := service(t, readDashboardOverlay(t), "worker")
	ports, ok := worker["ports"].([]any)
	if !ok || len(ports) == 0 {
		t.Fatalf("worker.ports = %#v, want non-empty list", worker["ports"])
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
			t.Errorf("port mapping %q publishes on host IP %q; expected a loopback address", mapping, hostIP)
		}
	}
}

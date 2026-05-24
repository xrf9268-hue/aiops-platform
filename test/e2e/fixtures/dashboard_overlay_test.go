package fixtures_test

import (
	"path/filepath"
	"strings"
	"testing"
)

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
		// Long-form "HOST_IP:HOST_PORT:CONTAINER_PORT". A two-field
		// "HOST_PORT:CONTAINER_PORT" (no host IP) binds all host interfaces,
		// which is exactly what we must forbid.
		parts := strings.Split(mapping, ":")
		if len(parts) != 3 {
			t.Fatalf("port mapping %q must pin a host IP (HOST_IP:HOST_PORT:CONTAINER_PORT)", mapping)
		}
		hostIP := parts[0]
		if hostIP != "127.0.0.1" && hostIP != "::1" {
			t.Errorf("port mapping %q publishes on host IP %q; expected loopback (127.0.0.1)", mapping, hostIP)
		}
	}
}

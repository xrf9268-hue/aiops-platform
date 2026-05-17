package fixtures_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGiteaPollerComposeUsesGiteaWorkflowFixture(t *testing.T) {
	compose := readCompose(t)
	poller := service(t, compose, "gitea-poller")
	command, ok := poller["command"].([]any)
	if !ok || len(command) != 2 {
		t.Fatalf("gitea-poller command = %#v, want two-item command", poller["command"])
	}
	workflowPath, ok := command[1].(string)
	if !ok || workflowPath == "" {
		t.Fatalf("gitea-poller workflow command argument = %#v, want non-empty string", command[1])
	}

	volumes, ok := poller["volumes"].([]any)
	if !ok {
		t.Fatalf("gitea-poller volumes = %#v, want list", poller["volumes"])
	}
	var mountedFixture string
	for _, raw := range volumes {
		volume, ok := raw.(string)
		if !ok {
			continue
		}
		parts := bytes.Split([]byte(volume), []byte(":"))
		if len(parts) < 2 {
			continue
		}
		host, container := string(parts[0]), string(parts[1])
		if workflowPath == container || strings.HasPrefix(workflowPath, container+"/") {
			relativeWorkflow := strings.TrimPrefix(strings.TrimPrefix(workflowPath, container), "/")
			mountedFixture = filepath.Clean(filepath.Join("..", "..", "..", "deploy", host, relativeWorkflow))
			break
		}
	}
	if mountedFixture == "" {
		t.Fatalf("gitea-poller workflow path %q is not mounted by a compose volume", workflowPath)
	}

	fixture := loadYAML(t, mountedFixture)
	tracker, ok := fixture["tracker"].(map[string]any)
	if !ok {
		t.Fatalf("%s missing tracker config", mountedFixture)
	}
	if got := tracker["kind"]; got != "gitea" {
		t.Fatalf("gitea-poller workflow tracker.kind = %v, want gitea", got)
	}
}

func readCompose(t *testing.T) map[string]any {
	t.Helper()
	return loadYAML(t, filepath.Join("..", "..", "..", "deploy", "docker-compose.yml"))
}

func loadYAML(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if bytes.HasPrefix(data, []byte("---\n")) {
		data = bytes.TrimPrefix(data, []byte("---\n"))
		if idx := bytes.Index(data, []byte("\n---")); idx >= 0 {
			data = data[:idx]
		}
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return doc
}

func service(t *testing.T, compose map[string]any, name string) map[string]any {
	t.Helper()
	services, ok := compose["services"].(map[string]any)
	if !ok {
		t.Fatal("compose file missing services map")
	}
	svc, ok := services[name].(map[string]any)
	if !ok {
		t.Fatalf("compose file missing %s service", name)
	}
	return svc
}

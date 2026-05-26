package fixtures_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestWorkerComposeDefinesHealthcheck(t *testing.T) {
	compose := readCompose(t)
	worker := service(t, compose, "worker")
	environment, ok := worker["environment"].(map[string]any)
	if !ok {
		t.Fatal("worker service missing environment map")
	}
	if got := environment["AIOPS_HEALTHCHECK_PORT"]; got != "${AIOPS_HEALTHCHECK_PORT:-4000}" {
		t.Fatalf("worker AIOPS_HEALTHCHECK_PORT = %#v, want defaulted environment override", got)
	}
	workflowPath, ok := environment["AIOPS_WORKFLOW_PATH"].(string)
	if !ok {
		t.Fatalf("worker AIOPS_WORKFLOW_PATH = %#v, want string", environment["AIOPS_WORKFLOW_PATH"])
	}
	workflow := loadYAML(t, filepath.Join("..", "..", "..", strings.TrimPrefix(workflowPath, "/app/")))
	server, ok := workflow["server"].(map[string]any)
	if !ok || server["port"] != 4000 {
		t.Fatalf("%s server.port = %#v, want 4000", workflowPath, server["port"])
	}
	healthcheck, ok := worker["healthcheck"].(map[string]any)
	if !ok {
		t.Fatal("worker service missing healthcheck")
	}
	testCommand, ok := healthcheck["test"].([]any)
	if !ok || len(testCommand) != 2 {
		t.Fatalf("worker healthcheck test = %#v, want CMD-SHELL command", healthcheck["test"])
	}
	if testCommand[0] != "CMD-SHELL" {
		t.Fatalf("worker healthcheck test[0] = %#v, want CMD-SHELL", testCommand[0])
	}
	command, ok := testCommand[1].(string)
	if !ok || !strings.Contains(command, "wget -qO- http://127.0.0.1:$${AIOPS_HEALTHCHECK_PORT:-4000}/livez") {
		t.Fatalf("worker healthcheck command = %#v, want /livez wget probe", testCommand[1])
	}
	for key, want := range map[string]string{
		"interval":     "30s",
		"timeout":      "5s",
		"start_period": "20s",
	} {
		if got := healthcheck[key]; got != want {
			t.Fatalf("worker healthcheck %s = %#v, want %q", key, got, want)
		}
	}
	if got := healthcheck["retries"]; got != 3 {
		t.Fatalf("worker healthcheck retries = %#v, want 3", got)
	}
}

func TestCodexComposeOverlayUsesSecrets(t *testing.T) {
	compose := loadYAML(t, filepath.Join("..", "..", "..", "deploy", "docker-compose.codex.yml"))
	worker := service(t, compose, "worker")
	build, ok := worker["build"].(map[string]any)
	if !ok || build["target"] != "codex-worker" {
		t.Fatalf("codex overlay build = %#v, want codex-worker target", worker["build"])
	}
	secrets, ok := worker["secrets"].([]any)
	if !ok || len(secrets) == 0 {
		t.Fatalf("codex overlay worker secrets = %#v, want service-scoped secrets", worker["secrets"])
	}
	environment := worker["environment"].(map[string]any)
	if got := environment["AIOPS_WORKFLOW_PATH"]; got != "/app/WORKFLOW.md" {
		t.Fatalf("codex overlay AIOPS_WORKFLOW_PATH = %#v, want /app/WORKFLOW.md", got)
	}
	for _, name := range []string{"AIOPS_STATE_API_TOKEN", "GITEA_TOKEN", "LINEAR_API_KEY"} {
		if got := environment[name]; got != "" {
			t.Fatalf("codex overlay %s = %#v, want blank because secret files feed production tokens", name, got)
		}
	}
	volumes := worker["volumes"].([]any)
	if !sliceContainsString(volumes, "/app/WORKFLOW.md:ro") {
		t.Fatalf("codex overlay volumes = %#v, want mounted workflow file", volumes)
	}
	declared, ok := compose["secrets"].(map[string]any)
	if !ok {
		t.Fatal("codex overlay missing top-level secrets")
	}
	for _, name := range []string{"linear_api_key", "gitea_token", "aiops_state_api_token"} {
		if _, ok := declared[name]; !ok {
			t.Fatalf("codex overlay missing secret %s", name)
		}
	}
	entrypoint := worker["entrypoint"].([]any)
	if len(entrypoint) != 1 || entrypoint[0] != "/usr/local/bin/aiops-secret-entrypoint" {
		t.Fatalf("codex overlay entrypoint = %#v, want aiops-secret-entrypoint", entrypoint)
	}
	command := worker["command"].([]any)
	if len(command) != 1 || command[0] != "worker" {
		t.Fatalf("codex overlay command = %#v, want worker default", command)
	}
	entrypointFile, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "aiops-secret-entrypoint.sh"))
	if err != nil {
		t.Fatalf("read secret entrypoint: %v", err)
	}
	if !strings.Contains(string(entrypointFile), "/run/secrets/linear_api_key") {
		t.Fatalf("secret entrypoint does not read Linear key from /run/secrets")
	}
	if !strings.Contains(string(entrypointFile), "/run/secrets/gitea_token") {
		t.Fatalf("secret entrypoint does not read optional Gitea token from /run/secrets")
	}
	if !strings.Contains(string(entrypointFile), "set -- worker \"$@\"") {
		t.Fatalf("secret entrypoint does not prepend worker for flag-only docker compose run commands")
	}
}

func TestFirstRunRunbookWritesAbsoluteComposePaths(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "runbooks", "first-run-docker-linear-codex.md"))
	if err != nil {
		t.Fatalf("read first-run runbook: %v", err)
	}
	text := string(body)
	for _, want := range []string{
		"AIOPS_WORKFLOW_PATH=$PWD/.aiops/WORKFLOW.md",
		"AIOPS_CODEX_HOME_PATH=$PWD/.aiops/codex-home",
		"LINEAR_API_KEY_FILE=$PWD/.aiops/secrets/linear_api_key",
		"AIOPS_STATE_API_TOKEN_FILE=$PWD/.aiops/secrets/state_api_token",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("first-run runbook missing absolute Compose path example %q", want)
		}
	}
}

func sliceContainsString(values []any, want string) bool {
	for _, value := range values {
		if strings.Contains(fmt.Sprint(value), want) {
			return true
		}
	}
	return false
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

//nolint:unparam // Shared test helper keeps call sites explicit about the service under inspection.
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

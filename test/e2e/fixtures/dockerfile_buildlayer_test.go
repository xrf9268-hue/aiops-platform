package fixtures_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func dockerfileContents(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "..", "Dockerfile")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func dockerignoreContents(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "..", ".dockerignore")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// TestDockerfileCopiesGoSumBeforeDownload guards the reproducible dependency
// layer (#369): go.sum must be copied alongside go.mod before `go mod
// download`, so module checksums are verifiable and the cache layer is keyed on
// both files. A go.mod-only copy regresses this.
func TestDockerfileCopiesGoSumBeforeDownload(t *testing.T) {
	df := dockerfileContents(t)

	copyGoSum := strings.Index(df, "COPY go.mod go.sum")
	if copyGoSum < 0 {
		t.Fatal("Dockerfile does not COPY go.sum alongside go.mod before downloading modules")
	}
	download := strings.Index(df, "RUN go mod download")
	if download < 0 {
		t.Fatal("Dockerfile has no `RUN go mod download`")
	}
	if copyGoSum > download {
		t.Errorf("`COPY go.mod go.sum` (at %d) must precede `go mod download` (at %d)", copyGoSum, download)
	}
	if copyAll := strings.Index(df, "COPY . ."); copyAll >= 0 && download > copyAll {
		t.Errorf("`go mod download` (at %d) must run before `COPY . .` (at %d) to keep the cached dependency layer", download, copyAll)
	}
}

func TestDockerfileBuildsDashboardBeforeWorker(t *testing.T) {
	df := dockerfileContents(t)

	dashboardStage := strings.Index(df, "FROM node:22-bookworm AS dashboard")
	buildStage := strings.Index(df, "FROM golang:${GO_VERSION}-bookworm AS build")
	copyDashboard := strings.Index(df, "COPY --from=dashboard /src/cmd/worker/dashboard/dist ./cmd/worker/dashboard/dist")
	// Anchor on the output target, not the full command, so build-flag changes
	// (e.g. the #796 -ldflags -X main.version stamp) don't break the
	// layer-ordering assertion.
	buildWorker := strings.Index(df, "-o /out/worker ./cmd/worker")
	for name, idx := range map[string]int{
		"dashboard stage": dashboardStage,
		"go build stage":  buildStage,
		"dashboard copy":  copyDashboard,
		"worker build":    buildWorker,
	} {
		if idx < 0 {
			t.Fatalf("Dockerfile missing %s", name)
		}
	}
	if dashboardStage > buildStage || buildStage > copyDashboard || copyDashboard > buildWorker {
		t.Fatalf("Dockerfile must generate dashboard dist before building worker: dashboard=%d build=%d copy=%d worker=%d", dashboardStage, buildStage, copyDashboard, buildWorker)
	}
	for _, want := range []string{"RUN npm ci", "RUN npm run build"} {
		if !strings.Contains(df, want) {
			t.Fatalf("Dockerfile dashboard stage missing %q", want)
		}
	}
}

func TestDockerignoreExcludesLocalDashboardBuildArtifacts(t *testing.T) {
	body := dockerignoreContents(t)
	for _, want := range []string{
		"cmd/worker/dashboard/node_modules",
		"cmd/worker/dashboard/dist",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf(".dockerignore missing %q", want)
		}
	}
}

func TestDockerfileAppliesRuntimeSecurityUpdatesBeforeInstallingTools(t *testing.T) {
	df := dockerfileContents(t)

	runtimeStage := strings.Index(df, "FROM debian:bookworm-slim")
	if runtimeStage < 0 {
		t.Fatal("Dockerfile missing debian runtime stage")
	}
	upgrade := strings.Index(df[runtimeStage:], "apt-get upgrade -y")
	if upgrade < 0 {
		t.Fatal("runtime stage must apply Debian security updates before installing tools")
	}
	install := strings.Index(df[runtimeStage:], "apt-get install -y --no-install-recommends ca-certificates git openssh-client ripgrep wget")
	if install < 0 {
		t.Fatal("runtime stage missing expected tool installation command")
	}
	if upgrade > install {
		t.Fatalf("runtime apt-get upgrade must precede tool installation: upgrade=%d install=%d", upgrade, install)
	}
}

func TestDockerfileDefinesCodexWorkerTarget(t *testing.T) {
	df := dockerfileContents(t)

	for _, want := range []string{
		"FROM worker AS codex-worker",
		"ARG CODEX_CLI_VERSION=0.142.0",
		"x86_64-unknown-linux-musl",
		"aarch64-unknown-linux-musl",
		"sha256sum -c -",
		"codex --version",
	} {
		if !strings.Contains(df, want) {
			t.Fatalf("Dockerfile codex-worker target missing %q", want)
		}
	}
}

// TestDockerfileCodexWorkerInstallsGhCLI pins the codex-worker image's gh CLI
// install. Without gh on PATH the documented aiops-secret-entrypoint silently
// skips `gh auth setup-git`, leaving git push without GitHub credentials and
// breaking `worker --doctor --github-issue` inside the container.
func TestDockerfileCodexWorkerInstallsGhCLI(t *testing.T) {
	df := dockerfileContents(t)

	codexStart := strings.Index(df, "FROM worker AS codex-worker")
	if codexStart < 0 {
		t.Fatal("Dockerfile missing codex-worker target")
	}
	nextStage := strings.Index(df[codexStart+1:], "\nFROM ")
	end := len(df)
	if nextStage >= 0 {
		end = codexStart + 1 + nextStage
	}
	stage := df[codexStart:end]

	for _, want := range []string{
		"ARG GH_CLI_VERSION=",
		"github.com/cli/cli/releases/download/",
		"gh_${GH_CLI_VERSION}_${gh_arch}.tar.gz",
		"sha256sum -c -",
		"install -m 0755",
		"gh --version",
	} {
		if !strings.Contains(stage, want) {
			t.Fatalf("Dockerfile codex-worker gh install missing %q", want)
		}
	}
}

func TestDockerfileKeepsWorkerAsDefaultTarget(t *testing.T) {
	df := dockerfileContents(t)

	codexTarget := strings.Index(df, "FROM worker AS codex-worker")
	if codexTarget < 0 {
		t.Fatal("Dockerfile missing codex-worker target")
	}
	defaultTarget := strings.LastIndex(df, "FROM worker AS default")
	if defaultTarget < 0 {
		t.Fatal("Dockerfile must end with a worker default target so `docker build .` does not install Codex")
	}
	if defaultTarget < codexTarget {
		t.Fatalf("Dockerfile default target must follow codex-worker: default=%d codex=%d", defaultTarget, codexTarget)
	}
	if strings.TrimSpace(df[defaultTarget:]) != "FROM worker AS default" {
		t.Fatalf("Dockerfile final target must be the worker default alias; got trailing content %q", strings.TrimSpace(df[defaultTarget:]))
	}
}

func TestDockerfileDefinesWorkerHealthcheck(t *testing.T) {
	df := dockerfileContents(t)

	if !strings.Contains(df, "HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3") {
		t.Fatal("Dockerfile missing worker healthcheck timing")
	}
	if !strings.Contains(df, `wget -qO- "http://127.0.0.1:${AIOPS_HEALTHCHECK_PORT:-4000}/livez"`) {
		t.Fatal("Dockerfile healthcheck must probe the unauthenticated /livez endpoint on the configured healthcheck port")
	}
	for _, removed := range []string{"linear-poller", "gitea-poller"} {
		if strings.Contains(df, removed) {
			t.Fatalf("Dockerfile still references removed binary %q", removed)
		}
	}
}

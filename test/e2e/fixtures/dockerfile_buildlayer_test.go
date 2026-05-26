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
	install := strings.Index(df[runtimeStage:], "apt-get install -y --no-install-recommends ca-certificates git openssh-client wget")
	if install < 0 {
		t.Fatal("runtime stage missing expected tool installation command")
	}
	if upgrade > install {
		t.Fatalf("runtime apt-get upgrade must precede tool installation: upgrade=%d install=%d", upgrade, install)
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

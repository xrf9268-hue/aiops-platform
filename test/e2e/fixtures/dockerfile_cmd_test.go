package fixtures_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestDockerfileDefaultsToWorkerCmd guards #370: the runtime image must define
// a default `CMD ["worker"]` so `docker run <image>` is useful. CMD (not
// ENTRYPOINT) is required so Compose's `command:` overrides replace it cleanly
// without argument duplication.
func TestDockerfileDefaultsToWorkerCmd(t *testing.T) {
	path := filepath.Join("..", "..", "..", "Dockerfile")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	df := string(data)

	cmdRE := regexp.MustCompile(`(?m)^CMD \["worker"\]`)
	if !cmdRE.MatchString(df) {
		t.Error(`Dockerfile is missing a default CMD ["worker"]`)
	}
	// ENTRYPOINT ["worker"] would make Compose's `command: ["worker"]` run
	// `worker worker`; guard against that regression.
	entryRE := regexp.MustCompile(`(?m)^ENTRYPOINT \["worker"\]`)
	if entryRE.MatchString(df) {
		t.Error(`Dockerfile uses ENTRYPOINT ["worker"]; use CMD so Compose command overrides do not duplicate the argument`)
	}
}

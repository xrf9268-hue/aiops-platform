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

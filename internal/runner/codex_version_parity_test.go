package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestCodexVersionPinParity is the author-time guard that CodexProtocolVersion
// (the single source of truth) agrees with every other place the codex version
// is pinned: the vendored schema filename, the vendored bundle itself, and the
// Dockerfile build arg. CI runs an equivalent text check, but this fails
// `go test` locally before a push so the pins cannot silently diverge — the
// exact failure mode that let the #446 schema snapshot rot to 0.133 while the
// runtime moved on.
func TestCodexVersionPinParity(t *testing.T) {
	m := regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)$`).FindStringSubmatch(CodexProtocolVersion)
	if m == nil {
		t.Fatalf("CodexProtocolVersion = %q; want major.minor.patch", CodexProtocolVersion)
	}

	// The vendored schema filename carries the FULL v<major>_<minor>_<patch>
	// stamp. The patch must be included: codex generates the schema per exact
	// version, so a patch bump (x.y.0 -> x.y.1) that forgets to regenerate
	// would otherwise keep the same major/minor filename, let this parity check
	// pass, and leave the contract test validating a stale patch schema (#629 P2).
	stamp := "v" + m[1] + "_" + m[2] + "_" + m[3]
	if !strings.Contains(codexProtocolSchemaFile, stamp) {
		t.Errorf("codexProtocolSchemaFile = %q; want it to contain stamp %q derived from CodexProtocolVersion %q (regenerate via scripts/refresh-codex-schema.sh)", codexProtocolSchemaFile, stamp, CodexProtocolVersion)
	}

	// The vendored bundle exists and is the real generated artifact (title is
	// emitted by `generate-json-schema`, not by a hand edit).
	raw, err := os.ReadFile(filepath.Join("testdata", codexProtocolSchemaFile))
	if err != nil {
		t.Fatalf("vendored schema %q missing: %v", codexProtocolSchemaFile, err)
	}
	var bundle struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(raw, &bundle); err != nil {
		t.Fatalf("decode vendored schema %q: %v", codexProtocolSchemaFile, err)
	}
	if bundle.Title != "CodexAppServerProtocolV2" {
		t.Errorf("vendored schema title = %q; want CodexAppServerProtocolV2 (regenerate via scripts/refresh-codex-schema.sh)", bundle.Title)
	}

	// The Dockerfile downloads the matching codex release.
	dockerfile, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dm := regexp.MustCompile(`(?m)^ARG CODEX_CLI_VERSION=(\S+)`).FindStringSubmatch(string(dockerfile))
	if dm == nil {
		t.Fatalf("Dockerfile has no `ARG CODEX_CLI_VERSION=` line")
	}
	if dm[1] != CodexProtocolVersion {
		t.Errorf("Dockerfile ARG CODEX_CLI_VERSION = %q; want %q (CodexProtocolVersion). Bump them together via scripts/refresh-codex-schema.sh.", dm[1], CodexProtocolVersion)
	}
}

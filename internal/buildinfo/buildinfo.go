// Package buildinfo resolves the version a binary reports (#796): the link-time
// -ldflags "-X main.version=<tag>" stamp when set, else the VCS revision the Go
// toolchain records for a source-tree build, else the stamp unchanged. It is
// shared by cmd/worker and cmd/tui so the resolution rule has one definition;
// each binary keeps its own main.version var because -X targets main.version.
package buildinfo

import (
	"runtime/debug"
	"strings"
)

// Resolve returns the human-facing version. stamped is the caller's
// main.version var. A meaningful stamp wins; otherwise the VCS revision (short,
// suffixed "-dirty" when the build tree was modified); otherwise the stamp
// unchanged (typically "devel").
func Resolve(stamped string) string {
	if v := strings.TrimSpace(stamped); v != "" && v != "devel" {
		return v
	}
	if rev := vcsRevision(); rev != "" {
		return rev
	}
	return stamped
}

func vcsRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var revision string
	var modified bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	return formatRevision(revision, modified)
}

// formatRevision shortens a VCS revision to 12 chars and appends "-dirty" when
// the build tree was modified. An empty revision (no VCS info) stays empty so
// Resolve can fall through to the stamp. Kept separate from ReadBuildInfo so
// the formatting is unit-testable.
func formatRevision(revision string, modified bool) string {
	if revision == "" {
		return ""
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if modified {
		revision += "-dirty"
	}
	return revision
}

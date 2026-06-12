package gitea

import (
	"os"
	"strings"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// BaseURLFromTrackerConfig applies the Gitea tracker base URL precedence used
// by the worker, legacy poller, and agent-side Gitea tools.
func BaseURLFromTrackerConfig(cfg workflow.TrackerConfig, fallback string) string {
	if endpoint := strings.TrimSpace(cfg.Endpoint); endpoint != "" {
		return strings.TrimRight(endpoint, "/")
	}
	if legacy := strings.TrimSpace(cfg.ProjectSlug); legacy != "" {
		return strings.TrimRight(legacy, "/")
	}
	return strings.TrimRight(strings.TrimSpace(fallback), "/")
}

// BaseURLFromEnv resolves the Gitea tracker base URL exactly as the worker
// dispatch does: tracker config first (endpoint, then the legacy
// project_slug), then the GITEA_BASE_URL environment variable, then the
// local-dev default. Shared by cmd/worker and internal/doctor so the doctor
// preflight can never drift from the poll loop's resolution (PR #801 drift
// class).
func BaseURLFromEnv(cfg workflow.TrackerConfig) string {
	fallback := os.Getenv("GITEA_BASE_URL")
	if fallback == "" {
		fallback = "http://localhost:3000"
	}
	return BaseURLFromTrackerConfig(cfg, fallback)
}

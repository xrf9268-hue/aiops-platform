package gitea

import (
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

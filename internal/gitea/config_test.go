package gitea

import (
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func TestBaseURLFromTrackerConfigUsesEndpointBeforeFallback(t *testing.T) {
	got := BaseURLFromTrackerConfig(workflow.TrackerConfig{
		Endpoint:    " https://gitea-endpoint.example.test/ ",
		ProjectSlug: "https://gitea-legacy.example.test/",
	}, "https://gitea-env.example.test/")
	if got != "https://gitea-endpoint.example.test" {
		t.Fatalf("BaseURLFromTrackerConfig = %q, want tracker.endpoint without trailing slash", got)
	}
}

func TestBaseURLFromTrackerConfigIgnoresProjectSlugAsFallback(t *testing.T) {
	got := BaseURLFromTrackerConfig(workflow.TrackerConfig{
		ProjectSlug: " https://gitea-legacy.example.test/ ",
	}, "https://gitea-env.example.test/")
	if got != "https://gitea-env.example.test" {
		t.Fatalf("BaseURLFromTrackerConfig = %q, want fallback without Gitea tracker.project_slug", got)
	}
}

func TestBaseURLFromTrackerConfigUsesFallbackWhenTrackerURLIsEmpty(t *testing.T) {
	got := BaseURLFromTrackerConfig(workflow.TrackerConfig{}, " https://gitea-env.example.test/ ")
	if got != "https://gitea-env.example.test" {
		t.Fatalf("BaseURLFromTrackerConfig = %q, want fallback without trailing slash", got)
	}
}

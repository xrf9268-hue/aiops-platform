package gitea

import (
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func TestBaseURLFromTrackerConfigUsesEndpointBeforeProjectSlugAndFallback(t *testing.T) {
	got := BaseURLFromTrackerConfig(workflow.TrackerConfig{
		Endpoint:    " https://gitea-endpoint.example.test/ ",
		ProjectSlug: "https://gitea-legacy.example.test/",
	}, "https://gitea-env.example.test/")
	if got != "https://gitea-endpoint.example.test" {
		t.Fatalf("BaseURLFromTrackerConfig = %q, want tracker.endpoint without trailing slash", got)
	}
}

func TestBaseURLFromTrackerConfigUsesLegacyProjectSlugBeforeFallback(t *testing.T) {
	got := BaseURLFromTrackerConfig(workflow.TrackerConfig{
		ProjectSlug: " https://gitea-legacy.example.test/ ",
	}, "https://gitea-env.example.test/")
	if got != "https://gitea-legacy.example.test" {
		t.Fatalf("BaseURLFromTrackerConfig = %q, want legacy tracker.project_slug without trailing slash", got)
	}
}

func TestBaseURLFromTrackerConfigUsesFallbackWhenTrackerURLIsEmpty(t *testing.T) {
	got := BaseURLFromTrackerConfig(workflow.TrackerConfig{}, " https://gitea-env.example.test/ ")
	if got != "https://gitea-env.example.test" {
		t.Fatalf("BaseURLFromTrackerConfig = %q, want fallback without trailing slash", got)
	}
}

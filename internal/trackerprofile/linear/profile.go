package linear

import (
	"fmt"
	"strings"

	"github.com/xrf9268-hue/aiops-platform/internal/trackerprofile"
)

const (
	DefaultEndpoint        = "https://api.linear.app/graphql"
	DefaultPaginationPages = 200
)

var SecretEnvironmentNames = []string{"LINEAR_API_KEY", "LINEAR_TOKEN"}

type Profile struct {
	trackerprofile.Base
	Endpoint           string
	APIKey             string
	TeamKey            string
	ProjectSlug        string
	PaginationMaxPages int
}

func Resolve(raw map[string]any, lookup func(string) (string, bool)) (*Profile, error) {
	profile, err := Parse(raw, lookup)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(profile.APIKey) == "" {
		return nil, fmt.Errorf("missing_tracker_secret: tracker.provider.api_key is required")
	}
	if strings.TrimSpace(profile.ProjectSlug) == "" {
		return nil, fmt.Errorf("tracker.provider.project_slug is required when tracker.kind is linear")
	}
	return profile, nil
}

// Parse constructs the Linear runtime profile while leaving required-field
// admission to Resolve. Tracker clients use it to retain their typed missing
// credential/scope errors for programmatically constructed configs.
func Parse(raw map[string]any, lookup func(string) (string, bool)) (*Profile, error) {
	provider := trackerprofile.CloneProvider(raw)
	endpoint, _, _, err := trackerprofile.ResolveDocumentedString(provider, "endpoint", lookup)
	if err != nil {
		return nil, err
	}
	endpoint = strings.TrimSpace(endpoint)
	if strings.TrimSpace(endpoint) == "" {
		endpoint = DefaultEndpoint
	}
	apiKey, secretEnvs, _, err := trackerprofile.ResolveOptionalSecret(provider, "api_key", SecretEnvironmentNames, lookup)
	if err != nil {
		return nil, err
	}
	teamKey, _, _, err := trackerprofile.ResolveDocumentedString(provider, "team_key", lookup)
	if err != nil {
		return nil, err
	}
	projectSlug, _, _, err := trackerprofile.ResolveDocumentedString(provider, "project_slug", lookup)
	if err != nil {
		return nil, err
	}
	pages, err := trackerprofile.PaginationPages(provider, "pagination_max_pages", DefaultPaginationPages)
	if err != nil {
		return nil, err
	}
	provider["endpoint"] = endpoint
	provider["pagination_max_pages"] = pages
	return &Profile{
		Base: trackerprofile.Base{
			AdapterKind:  "linear",
			ProviderMap:  provider,
			Secrets:      []string{apiKey},
			SecretEnvs:   secretEnvs,
			SecretFields: []string{"api_key"},
		},
		Endpoint: endpoint, APIKey: apiKey, TeamKey: teamKey,
		ProjectSlug: projectSlug, PaginationMaxPages: pages,
	}, nil
}

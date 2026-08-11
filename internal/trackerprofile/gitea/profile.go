package gitea

import (
	"fmt"
	"strings"

	"github.com/xrf9268-hue/aiops-platform/internal/trackerprofile"
)

const (
	DefaultBaseURL         = "http://localhost:3000"
	DefaultPaginationPages = 20
)

var SecretEnvironmentNames = []string{"GITEA_TOKEN", "GITEA_API_TOKEN"}

type Profile struct {
	trackerprofile.Base
	BaseURL            string
	Token              string
	Owner              string
	Repo               string
	PaginationMaxPages int
	BaseURLConfigured  bool
}

func Resolve(raw map[string]any, fallbackOwner, fallbackRepo string, lookup func(string) (string, bool)) (*Profile, error) {
	profile, err := Parse(raw, fallbackOwner, fallbackRepo, lookup)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(profile.Token) == "" {
		return nil, fmt.Errorf("missing_tracker_secret: tracker.provider.token is required")
	}
	if strings.TrimSpace(profile.Owner) == "" || strings.TrimSpace(profile.Repo) == "" {
		return nil, fmt.Errorf("tracker.provider.repo is required when repo.owner or repo.name is missing")
	}
	return profile, nil
}

func Parse(raw map[string]any, fallbackOwner, fallbackRepo string, lookup func(string) (string, bool)) (*Profile, error) {
	provider := trackerprofile.CloneProvider(raw)
	baseURL, _, present, err := trackerprofile.ResolveDocumentedString(provider, "base_url", lookup)
	if err != nil {
		return nil, err
	}
	baseURL = strings.TrimSpace(baseURL)
	envBaseURL, envBaseURLPresent := lookup("GITEA_BASE_URL")
	baseURLConfigured := (present && baseURL != "") ||
		(envBaseURLPresent && strings.TrimSpace(envBaseURL) != "")
	if !present || strings.TrimSpace(baseURL) == "" {
		baseURL = strings.TrimSpace(trackerprofile.EnvironmentValue(lookup, "GITEA_BASE_URL", DefaultBaseURL))
	}
	token, secretEnvs, _, err := trackerprofile.ResolveOptionalSecret(provider, "token", SecretEnvironmentNames, lookup)
	if err != nil {
		return nil, err
	}
	owner, repo, err := parseRepo(provider, fallbackOwner, fallbackRepo, lookup)
	if err != nil {
		return nil, err
	}
	pages, err := trackerprofile.PaginationPages(provider, "pagination_max_pages", DefaultPaginationPages)
	if err != nil {
		return nil, err
	}
	provider["base_url"] = baseURL
	provider["repo"] = owner + "/" + repo
	provider["pagination_max_pages"] = pages
	return &Profile{
		Base:    trackerprofile.Base{AdapterKind: "gitea", ProviderMap: provider, Secrets: []string{token}, SecretEnvs: secretEnvs, SecretFields: []string{"token"}},
		BaseURL: baseURL, Token: token, Owner: owner, Repo: repo, PaginationMaxPages: pages, BaseURLConfigured: baseURLConfigured,
	}, nil
}

func parseRepo(provider map[string]any, fallbackOwner, fallbackRepo string, lookup func(string) (string, bool)) (string, string, error) {
	repoValue, _, present, err := trackerprofile.ResolveDocumentedString(provider, "repo", lookup)
	if err != nil {
		return "", "", err
	}
	if !present || strings.TrimSpace(repoValue) == "" {
		return strings.TrimSpace(fallbackOwner), strings.TrimSpace(fallbackRepo), nil
	}
	parts := strings.Split(strings.Trim(repoValue, "/"), "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", fmt.Errorf("tracker.provider.repo must be in owner/name form")
	}
	return parts[0], parts[1], nil
}

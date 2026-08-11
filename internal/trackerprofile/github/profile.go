package github

import (
	"fmt"
	"strings"

	"github.com/xrf9268-hue/aiops-platform/internal/trackerprofile"
)

const (
	DefaultAPIURL          = "https://api.github.com"
	DefaultPaginationPages = 10
)

var SecretEnvironmentNames = []string{"GITHUB_TOKEN", "GH_TOKEN", "GITHUB_PAT"}

type Profile struct {
	trackerprofile.Base
	APIURL             string
	Token              string
	Owner              string
	Repo               string
	PaginationMaxPages int
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
	apiURL, _, present, err := trackerprofile.ResolveDocumentedString(provider, "api_url", lookup)
	if err != nil {
		return nil, err
	}
	if !present || strings.TrimSpace(apiURL) == "" {
		apiURL = trackerprofile.EnvironmentValue(lookup, "GITHUB_API_BASE_URL", DefaultAPIURL)
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
	provider["api_url"] = apiURL
	provider["repo"] = owner + "/" + repo
	provider["pagination_max_pages"] = pages
	return &Profile{
		Base:   trackerprofile.Base{AdapterKind: "github", ProviderMap: provider, Secrets: []string{token}, SecretEnvs: secretEnvs, SecretFields: []string{"token"}},
		APIURL: apiURL, Token: token, Owner: owner, Repo: repo, PaginationMaxPages: pages,
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

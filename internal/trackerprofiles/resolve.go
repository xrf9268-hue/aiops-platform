package trackerprofiles

import (
	"fmt"
	"strings"

	"github.com/xrf9268-hue/aiops-platform/internal/trackerprofile"
	giteaprofile "github.com/xrf9268-hue/aiops-platform/internal/trackerprofile/gitea"
	githubprofile "github.com/xrf9268-hue/aiops-platform/internal/trackerprofile/github"
	linearprofile "github.com/xrf9268-hue/aiops-platform/internal/trackerprofile/linear"
)

// Resolve delegates the opaque provider object to the selected adapter.
func Resolve(kind string, provider map[string]any, repoOwner, repoName string, lookup func(string) (string, bool)) (trackerprofile.Profile, error) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "linear":
		return linearprofile.Resolve(provider, lookup)
	case "gitea":
		return giteaprofile.Resolve(provider, repoOwner, repoName, lookup)
	case "github":
		return githubprofile.Resolve(provider, repoOwner, repoName, lookup)
	default:
		return nil, fmt.Errorf("tracker.kind %q is not supported (allowed: gitea, github, linear)", kind)
	}
}

// SecretEnvironmentNames returns the union declared by supported adapters.
// The registry owns this cross-adapter aggregation; core workflow code does
// not carry credential-name knowledge.
func SecretEnvironmentNames() []string {
	names := make([]string, 0, len(linearprofile.SecretEnvironmentNames)+len(giteaprofile.SecretEnvironmentNames)+len(githubprofile.SecretEnvironmentNames))
	names = append(names, linearprofile.SecretEnvironmentNames...)
	names = append(names, giteaprofile.SecretEnvironmentNames...)
	names = append(names, githubprofile.SecretEnvironmentNames...)
	return names
}

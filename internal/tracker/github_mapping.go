package tracker

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// githubIssueLabels returns the issue's label names lowercased and trimmed
// (SPEC §11.3 normalization), the form the required_labels gate matches against.
func githubIssueLabels(issue githubIssue) []string {
	labels := make([]string, 0, len(issue.Labels))
	for _, label := range issue.Labels {
		if name := strings.ToLower(strings.TrimSpace(label.Name)); name != "" {
			labels = append(labels, name)
		}
	}
	return labels
}

func mapGitHubIssue(issue githubIssue, mappedState string) (Issue, error) {
	id := strconv.FormatInt(issue.ID, 10)
	if id == "0" && issue.Number != 0 {
		id = strconv.Itoa(issue.Number)
	}
	createdAt, err := parseGitHubIssueTime("created_at", issue.CreatedAt)
	if err != nil {
		return Issue{}, err
	}
	updatedAt, err := parseGitHubIssueTime("updated_at", issue.UpdatedAt)
	if err != nil {
		return Issue{}, err
	}
	labels := githubIssueLabels(issue)
	state := strings.TrimSpace(mappedState)
	if state == "" {
		state = strings.TrimSpace(issue.State)
	}
	return Issue{
		ID:          id,
		Identifier:  fmt.Sprintf("#%d", issue.Number),
		Title:       issue.Title,
		Description: issue.Body,
		URL:         issue.HTMLURL,
		State:       state,
		Labels:      labels,
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
	}, nil
}

func parseGitHubIssueTime(field, value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse GitHub issue %s %q: %w", field, value, err)
	}
	return parsed, nil
}

func githubIssueQueryForState(state string) (issueState, label, mappedState string) {
	state = strings.TrimSpace(state)
	switch strings.ToLower(state) {
	case "open", "closed", "all":
		return strings.ToLower(state), "", strings.ToLower(state)
	default:
		return "open", state, state
	}
}

func githubStatesMayIncludeOpenIssues(states []string) bool {
	for _, state := range states {
		issueState, _, _ := githubIssueQueryForState(state)
		if issueState == "open" || issueState == "all" {
			return true
		}
	}
	return false
}

func githubIssueQueryRequiresCompleteClaims(issueState string) bool {
	issueState = strings.ToLower(strings.TrimSpace(issueState))
	return issueState == "open" || issueState == "all"
}

func githubIssueCollectionScope(issueState, label string) string {
	if strings.TrimSpace(label) != "" {
		return label
	}
	return issueState
}

func githubClaimedIssueNumbers(text string) []int {
	matches := githubClaimedIssueRE.FindAllStringSubmatch(text, -1)
	out := make([]int, 0, len(matches))
	seen := map[int]struct{}{}
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		number, err := strconv.Atoi(match[1])
		if err != nil || number == 0 {
			continue
		}
		if _, ok := seen[number]; ok {
			continue
		}
		seen[number] = struct{}{}
		out = append(out, number)
	}
	return out
}

func nonEmptyGitHubStates(states []string) []string {
	out := make([]string, 0, len(states))
	seen := map[string]struct{}{}
	for _, state := range states {
		state = strings.TrimSpace(state)
		if state == "" {
			continue
		}
		key := strings.ToLower(state)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, state)
	}
	return out
}

func githubHasNextPage(linkHeaders []string) bool {
	for _, header := range linkHeaders {
		for _, part := range strings.Split(header, ",") {
			if strings.Contains(part, `rel="next"`) {
				return true
			}
		}
	}
	return false
}

func (c *GitHubClient) recordPaginationCapHit(label string, maxPages int) {
	c.paginationCapHits.Add(1)
	if c.Logf != nil {
		c.Logf("github pagination exceeded %d pages for label/state %q; failing this tracker listing so reconcile does not treat partial results as authoritative", maxPages, label)
	}
}

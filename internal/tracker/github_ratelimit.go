package tracker

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// githubRateLimited reports whether resp is one of GitHub's documented
// rate-limit responses. Primary and secondary limits surface as 403 as well
// as 429 (REST API docs), distinguished from ordinary permission 403s by an
// exhausted X-RateLimit-Remaining or a Retry-After header; headerless
// secondary limits carry only the documented body message — a plain 403
// stays a generic status error so auth misconfiguration is not misreported
// as throttling.
func githubRateLimited(resp *http.Response) bool {
	if resp.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if resp.StatusCode != http.StatusForbidden {
		return false
	}
	if strings.TrimSpace(resp.Header.Get("X-RateLimit-Remaining")) == "0" ||
		strings.TrimSpace(resp.Header.Get("Retry-After")) != "" {
		return true
	}
	return githubSecondaryLimitBody(resp.Body)
}

// githubSecondaryLimitBody reports whether a 403 body carries GitHub's
// documented headerless secondary-rate-limit shape. The discriminator is the
// API's documented error payload (message / documentation_url), the same one
// GitHub's own octokit throttling plugin matches — this parses an upstream
// wire contract, not our error strings, so clean-code rule 8 does not apply.
// It consumes a bounded prefix of the body, which the caller's generic-403
// path never reads.
func githubSecondaryLimitBody(body io.Reader) bool {
	payload, err := io.ReadAll(io.LimitReader(body, 8<<10))
	if err != nil {
		return false
	}
	var apiErr struct {
		Message          string `json:"message"`
		DocumentationURL string `json:"documentation_url"`
	}
	if json.Unmarshal(payload, &apiErr) != nil {
		return false
	}
	return strings.Contains(strings.ToLower(apiErr.Message), "secondary rate limit") ||
		strings.Contains(strings.ToLower(apiErr.DocumentationURL), "secondary-rate-limit")
}

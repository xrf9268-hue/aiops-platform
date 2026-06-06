package workflow

import (
	"log"
	"strings"
)

// migrateTrackerEndpoint reconciles the SPEC §5.3.1 `tracker.endpoint`
// field with the pre-#242 `tracker.base_url` alias: prefer endpoint when
// set, fall back to base_url with a deprecation log, and finally clear
// BaseURL on the resolved config so downstream code reads only Endpoint.
func migrateTrackerEndpoint(frontBytes []byte, cfg *Config) {
	endpointPresent := hasNestedKey(frontBytes, "tracker", "endpoint")
	legacyPresent := hasNestedKey(frontBytes, "tracker", "base_url")
	switch {
	case endpointPresent:
		if legacyPresent {
			log.Printf("workflow: tracker.base_url is deprecated and ignored because tracker.endpoint is set")
		}
	case legacyPresent:
		log.Printf("workflow: tracker.base_url is deprecated; use tracker.endpoint (SPEC §5.3.1)")
		cfg.Tracker.Endpoint = cfg.Tracker.BaseURL
	}
	cfg.Tracker.BaseURL = ""
}

func migrateGiteaTrackerProjectSlug(frontBytes []byte, cfg *Config) {
	if !strings.EqualFold(cfg.Tracker.Kind, "gitea") {
		return
	}
	legacyPresent := hasNestedKey(frontBytes, "tracker", "project_slug")
	if !legacyPresent {
		return
	}
	switch {
	case strings.TrimSpace(cfg.Tracker.Endpoint) != "":
		log.Printf("workflow: tracker.project_slug is ignored for Gitea because tracker.endpoint is already set")
	case strings.TrimSpace(cfg.Tracker.ProjectSlug) != "":
		log.Printf("workflow: tracker.project_slug is deprecated as a Gitea base URL; use tracker.endpoint")
		cfg.Tracker.Endpoint = cfg.Tracker.ProjectSlug
	}
	cfg.Tracker.ProjectSlug = ""
}

func migratePollingInterval(frontBytes []byte, cfg *Config) {
	pollingPresent := hasNestedKey(frontBytes, "polling", "interval_ms")
	legacyPresent := hasNestedKey(frontBytes, "tracker", "poll_interval_ms")
	switch {
	case pollingPresent:
		if legacyPresent {
			log.Printf("workflow: tracker.poll_interval_ms is deprecated and ignored because polling.interval_ms is set")
		}
		cfg.Tracker.PollIntervalMs = cfg.Polling.IntervalMs
	case legacyPresent:
		log.Printf("workflow: tracker.poll_interval_ms is deprecated; use polling.interval_ms")
		cfg.Polling.IntervalMs = cfg.Tracker.PollIntervalMs
	default:
		cfg.Tracker.PollIntervalMs = cfg.Polling.IntervalMs
	}
}

// defaultAgentMaxContinuationTurns mirrors agent.max_turns into
// agent.max_continuation_turns unless the operator set the continuation cap
// explicitly in front matter, so the two SPEC §16.5 budgets default together.
func defaultAgentMaxContinuationTurns(cfg *Config, hasFrontMatter bool, front []byte) {
	if cfg == nil {
		return
	}
	if hasFrontMatter && hasNestedKey(front, "agent", "max_continuation_turns") {
		return
	}
	cfg.Agent.MaxContinuationTurns = cfg.Agent.MaxTurns
}

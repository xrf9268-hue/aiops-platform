// cmd/tui state-API client: the /api/v1/state response DTOs, the
// authenticated fetch, and the rolling token-throughput sampling. The poll
// loop that drives it and the terminal lifecycle live in main.go; the frame
// rendering lives in render.go.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const stateAPIAuthTokenEnv = "AIOPS_STATE_API_TOKEN"

// throughputWindowMs is the rolling window for TPS calculation (5 s).
const throughputWindowMs = 5_000

// ── API response types ────────────────────────────────────────────────────────
// Field names match the /api/v1/state JSON contract (SPEC §13.7.2).

type stateResponse struct {
	GeneratedAt         time.Time              `json:"generated_at"`
	PollIntervalMs      int64                  `json:"poll_interval_ms"`
	MaxConcurrentAgents int                    `json:"max_concurrent_agents"`
	Counts              stateCounts            `json:"counts"`
	Running             []runningEntry         `json:"running"`
	Retrying            []retryEntry           `json:"retrying"`
	CodexTotals         codexTotals            `json:"codex_totals"`
	RateLimits          map[string]interface{} `json:"rate_limits"`
}

type stateCounts struct {
	Running                            int   `json:"running"`
	Blocked                            int   `json:"blocked"`
	Retrying                           int   `json:"retrying"`
	CompletedTotal                     int64 `json:"completed_total"`
	AgentHandoffReconcileStoppedTotal  int64 `json:"agent_handoff_reconcile_stopped_total"`
	AgentHandoffReconcileStoppedRecent int   `json:"agent_handoff_reconcile_stopped"`
}

type runningEntry struct {
	IssueID           string     `json:"issue_id"`
	Identifier        string     `json:"issue_identifier"`
	State             string     `json:"state"`
	SessionID         string     `json:"session_id"`
	TurnCount         int        `json:"turn_count"`
	LastEvent         string     `json:"last_event"`
	LastMessage       string     `json:"last_message"`
	StartedAt         *time.Time `json:"started_at"`
	LastEventAt       *time.Time `json:"last_event_at"`
	Tokens            tokenInfo  `json:"tokens"`
	CodexAppServerPID int        `json:"codex_app_server_pid"`
}

type tokenInfo struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

type retryEntry struct {
	IssueID    string     `json:"issue_id"`
	Identifier string     `json:"issue_identifier"`
	Attempt    int        `json:"attempt"`
	DueAt      *time.Time `json:"due_at"`
	Error      string     `json:"error"`
	Kind       string     `json:"kind"`
}

type codexTotals struct {
	InputTokens    int64   `json:"input_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
	TotalTokens    int64   `json:"total_tokens"`
	SecondsRunning float64 `json:"seconds_running"`
}

// ── TPS tracking ──────────────────────────────────────────────────────────────

type tokenSample struct {
	at     time.Time
	tokens int64
}

func pruneSamples(samples []tokenSample, now time.Time) []tokenSample {
	cutoff := now.Add(-throughputWindowMs * time.Millisecond)
	out := samples[:0]
	for _, s := range samples {
		// Use >= (not >) to match Elixir: timestamp >= min_timestamp.
		if !s.at.Before(cutoff) {
			out = append(out, s)
		}
	}
	return out
}

func rollingTPS(samples []tokenSample, now time.Time, currentTokens int64) float64 {
	all := pruneSamples(append([]tokenSample{{at: now, tokens: currentTokens}}, samples...), now)
	if len(all) < 2 {
		return 0
	}
	oldest := all[len(all)-1]
	elapsed := now.Sub(oldest.at).Seconds()
	if elapsed <= 0 {
		return 0
	}
	delta := currentTokens - oldest.tokens
	if delta <= 0 {
		return 0
	}
	return float64(delta) / elapsed
}

func fetchState(ctx context.Context, client *http.Client, url string) (*stateResponse, error) {
	return fetchStateWithAuth(ctx, client, url, "")
}

func fetchStateWithAuth(ctx context.Context, client *http.Client, url string, authToken string) (*stateResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var s stateResponse
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return nil, err
	}
	return &s, nil
}

func stateAPIAuthTokenFromEnv() string {
	return strings.TrimSpace(os.Getenv(stateAPIAuthTokenEnv))
}

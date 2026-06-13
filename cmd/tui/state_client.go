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

	"github.com/xrf9268-hue/aiops-platform/internal/stateapi"
)

const stateAPIAuthTokenEnv = "AIOPS_STATE_API_TOKEN"

// throughputWindowMs is the rolling window for TPS calculation (5 s).
const throughputWindowMs = 5_000

// The /api/v1/state response DTOs live in internal/stateapi, shared verbatim
// with the worker (producer) so the JSON contract has one definition (#793).
// This file keeps only the TUI-side fetch and the rolling token-throughput
// sampling.

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

func fetchState(ctx context.Context, client *http.Client, url string) (*stateapi.StateResponse, error) {
	return fetchStateWithAuth(ctx, client, url, "")
}

func fetchStateWithAuth(ctx context.Context, client *http.Client, url string, authToken string) (*stateapi.StateResponse, error) {
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
	var s stateapi.StateResponse
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return nil, err
	}
	return &s, nil
}

func stateAPIAuthTokenFromEnv() string {
	return strings.TrimSpace(os.Getenv(stateAPIAuthTokenEnv))
}

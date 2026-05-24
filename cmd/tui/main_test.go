package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ── formatCount / groupThousands ──────────────────────────────────────────────

func TestFormatCount(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1,000"},
		{1234567, "1,234,567"},
		{-1234, "-1,234"},
	}
	for _, c := range cases {
		if got := formatCount(c.in); got != c.want {
			t.Errorf("formatCount(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ── sanitize (P2: ANSI/control byte stripping) ─────────────────────────────

func TestSanitize_StripsANSIEscapes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello world", "hello world"},
		{"csi_color", "\033[31mred\033[0m", "red"},
		{"csi_cursor", "\033[2Jclear", "clear"},
		{"bare_escape", "\033Xfoo", "foo"},
		{"control_bytes", "a\x00b\x1fc", "a b c"},
		{"newline_to_space", "a\nb\rc", "a b c"},
		{"mixed", "\033[1mbold\033[0m\nand plain", "bold and plain"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sanitize(c.in); got != c.want {
				t.Errorf("sanitize(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// ── compactSession ────────────────────────────────────────────────────────────

func TestCompactSession(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "n/a"},
		{"short", "short"},
		{"1234567890", "1234567890"},          // exactly 10 runes → no compact
		{"12345678901", "1234...678901"},      // 11 runes → compact
		{"abcdefghijklmnop", "abcd...klmnop"}, // long → first 4 + last 6
	}
	for _, c := range cases {
		if got := compactSession(c.in); got != c.want {
			t.Errorf("compactSession(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ── statusDotColor ────────────────────────────────────────────────────────────

func TestStatusDotColor(t *testing.T) {
	// "" and "none" both map to red (Elixir :none atom → "none" over JSON,
	// Go zero value is "").
	for _, event := range []string{"", "none"} {
		if got := statusDotColor(event); got != ansiRed {
			t.Errorf("statusDotColor(%q) = %q, want ansiRed", event, got)
		}
	}
	if got := statusDotColor("codex/event/task_started"); got != ansiGreen {
		t.Errorf("statusDotColor(task_started) = %q, want ansiGreen", got)
	}
	if got := statusDotColor("codex/event/token_count"); got != ansiYellow {
		t.Errorf("statusDotColor(token_count) = %q, want ansiYellow", got)
	}
	if got := statusDotColor("turn_completed"); got != ansiMagenta {
		t.Errorf("statusDotColor(turn_completed) = %q, want ansiMagenta", got)
	}
	if got := statusDotColor("other/event"); got != ansiBlue {
		t.Errorf("statusDotColor(other) = %q, want ansiBlue", got)
	}
}

// ── pruneSamples / rollingTPS ─────────────────────────────────────────────────

func TestPruneSamples_KeepsBoundaryEntry(t *testing.T) {
	now := time.Now()
	cutoff := now.Add(-throughputWindowMs * time.Millisecond)
	samples := []tokenSample{
		{at: cutoff, tokens: 100},                       // exactly on boundary → must be kept (>= semantics)
		{at: cutoff.Add(-time.Millisecond), tokens: 50}, // just outside → must drop
	}
	got := pruneSamples(samples, now)
	if len(got) != 1 {
		t.Fatalf("pruneSamples kept %d samples, want 1 (boundary entry only)", len(got))
	}
	if got[0].tokens != 100 {
		t.Errorf("kept wrong sample: tokens=%d, want 100", got[0].tokens)
	}
}

func TestRollingTPS_ZeroWithSingleSample(t *testing.T) {
	now := time.Now()
	samples := []tokenSample{{at: now.Add(-time.Second), tokens: 1000}}
	if got := rollingTPS(samples, now, 1000); got != 0 {
		t.Errorf("rollingTPS with equal tokens = %f, want 0", got)
	}
}

func TestRollingTPS_PositiveRate(t *testing.T) {
	now := time.Now()
	samples := []tokenSample{{at: now.Add(-2 * time.Second), tokens: 0}}
	tps := rollingTPS(samples, now, 200)
	// 200 tokens / 2s = 100 tps (approx)
	if tps < 90 || tps > 110 {
		t.Errorf("rollingTPS = %f, want ~100", tps)
	}
}

// ── cell ─────────────────────────────────────────────────────────────────────

func TestCell_TruncatesLongStrings(t *testing.T) {
	got := cell("hello world", 8)
	if len(got) != 8 {
		t.Errorf("cell width = %d, want 8", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("cell(%q, 8) = %q, want truncation with ...", "hello world", got)
	}
}

func TestCell_PadsShortStrings(t *testing.T) {
	got := cell("hi", 8)
	if got != "hi      " {
		t.Errorf("cell(%q, 8) = %q, want left-padded to width", "hi", got)
	}
}

// ── formatRuntimeSecs ─────────────────────────────────────────────────────────

func TestFormatRuntimeSecs(t *testing.T) {
	cases := []struct {
		secs float64
		want string
	}{
		{0, "0m 0s"},
		{59, "0m 59s"},
		{60, "1m 0s"},
		{3661, "61m 1s"},
	}
	for _, c := range cases {
		if got := formatRuntimeSecs(c.secs); got != c.want {
			t.Errorf("formatRuntimeSecs(%v) = %q, want %q", c.secs, got, c.want)
		}
	}
}

// ── fetchState HTTP status check (P1 fix) ────────────────────────────────────

func TestFetchState_ReturnsErrorOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"unavailable","message":"snapshot temporarily unavailable"}}`))
	}))
	defer srv.Close()

	client := &http.Client{}
	_, err := fetchState(client, srv.URL)
	if err == nil {
		t.Fatal("fetchState returned nil error for 503 response, want error")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error = %q, want mention of status 503", err.Error())
	}
}

func TestFetchState_ParsesValidResponse(t *testing.T) {
	body := `{"poll_interval_ms":5000,"max_concurrent_agents":10,"counts":{"running":1},"running":[],"retrying":[],"codex_totals":{}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	client := &http.Client{}
	state, err := fetchState(client, srv.URL)
	if err != nil {
		t.Fatalf("fetchState error = %v", err)
	}
	if state.MaxConcurrentAgents != 10 {
		t.Errorf("MaxConcurrentAgents = %d, want 10", state.MaxConcurrentAgents)
	}
	if state.PollIntervalMs != 5000 {
		t.Errorf("PollIntervalMs = %d, want 5000", state.PollIntervalMs)
	}
}

// ── formatResetValue ──────────────────────────────────────────────────────────

func TestFormatResetValue_IntegerGetsSuffix(t *testing.T) {
	if got := formatResetValue(float64(30)); got != "30s" {
		t.Errorf("formatResetValue(30.0) = %q, want %q", got, "30s")
	}
	if got := formatResetValue("already-string"); got != "already-string" {
		t.Errorf("formatResetValue(string) = %q, want passthrough", got)
	}
}

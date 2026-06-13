package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/stateapi"
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

func TestFormatRetryRowIncludesQuotaBackoffKind(t *testing.T) {
	got := formatRetryRow(stateapi.Retry{
		IssueID:    "issue-1",
		Identifier: "ENG-1",
		Attempt:    1,
		Kind:       "quota_backoff",
		Error:      "quota backoff",
	})
	if !strings.Contains(got, "quota_backoff") {
		t.Fatalf("formatRetryRow missing quota_backoff kind: %q", got)
	}
	if strings.Count(got, "quota_backoff") != 1 {
		t.Fatalf("formatRetryRow duplicated quota_backoff kind: %q", got)
	}
}

func TestFormatRetryRowDefaultsKindToFailure(t *testing.T) {
	got := formatRetryRow(stateapi.Retry{IssueID: "issue-1", Identifier: "ENG-1", Attempt: 1})
	if !strings.Contains(got, "kind=failure") {
		t.Fatalf("formatRetryRow missing default failure kind: %q", got)
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
	_, err := fetchState(context.Background(), client, srv.URL)
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
	state, err := fetchState(context.Background(), client, srv.URL)
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

// TestFetchState_DecodesSharedContractFields is the consumer half of the #793
// anti-drift guard: the TUI now decodes /api/v1/state into the same
// stateapi types the worker marshals, so a JSON-tag change is observable here
// as a dropped value rather than a silently blank dashboard. It pins every
// field the dashboard renders — handoff counts, per-row token totals, retry
// kind, codex totals, and the nested rate-limit bucket — so a tag typo on any
// of them fails this test (and the worker-side runbook drift test), not the
// rendered UI.
func TestFetchState_DecodesSharedContractFields(t *testing.T) {
	body := `{
	  "poll_interval_ms": 5000,
	  "max_concurrent_agents": 4,
	  "counts": {"completed_total": 12, "agent_handoff_reconcile_stopped_total": 3, "agent_handoff_reconcile_stopped": 2},
	  "running": [{"issue_id": "i1", "issue_identifier": "ENG-1", "state": "In Progress", "session_id": "sess-1", "turn_count": 7, "last_event": "turn_completed", "last_message": "working", "started_at": "2026-05-21T09:09:55Z", "codex_app_server_pid": 12345, "tokens": {"total_tokens": 2000}}],
	  "retrying": [{"issue_id": "i2", "attempt": 2, "due_at": "2026-05-21T09:11:00Z", "error": "retry soon", "kind": "quota_backoff"}],
	  "codex_totals": {"total_tokens": 300, "seconds_running": 1.5},
	  "rate_limits": {"limit_id": "lim", "primary": {"remaining": 42, "limit": 100}}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	state, err := fetchState(context.Background(), &http.Client{}, srv.URL)
	if err != nil {
		t.Fatalf("fetchState() error = %v", err)
	}
	if got := state.Counts.AgentHandoffReconcileStopped; got != 2 {
		t.Errorf("Counts.AgentHandoffReconcileStopped = %d, want 2 (agent_handoff_reconcile_stopped tag)", got)
	}
	if got := state.Counts.CompletedTotal; got != 12 {
		t.Errorf("Counts.CompletedTotal = %d, want 12 (completed_total tag)", got)
	}
	// Pin every running-row field the dashboard renders (render.go formatRunningRow)
	// so a tag typo on any of them fails here rather than blanking a column.
	if len(state.Running) != 1 {
		t.Fatalf("Running = %+v, want one row", state.Running)
	}
	r := state.Running[0]
	if r.IssueID != "i1" || r.Identifier != "ENG-1" || r.State != "In Progress" || r.SessionID != "sess-1" ||
		r.TurnCount != 7 || r.LastEvent != "turn_completed" || r.LastMessage != "working" ||
		r.CodexAppServerPID != 12345 || r.Tokens.TotalTokens != 2000 {
		t.Errorf("Running[0] = %+v, want every rendered field decoded (issue_id/identifier/state/session_id/turn_count/last_event/last_message/codex_app_server_pid/tokens.total_tokens tags)", r)
	}
	if r.StartedAt == nil || !r.StartedAt.Equal(time.Date(2026, 5, 21, 9, 9, 55, 0, time.UTC)) {
		t.Errorf("Running[0].StartedAt = %v, want decoded started_at", r.StartedAt)
	}
	// Pin every retry-row field the dashboard renders (render.go formatRetryRow).
	if len(state.Retrying) != 1 {
		t.Fatalf("Retrying = %+v, want one row", state.Retrying)
	}
	rt := state.Retrying[0]
	if rt.Attempt != 2 || rt.Error != "retry soon" || rt.Kind != "quota_backoff" || rt.DueAt == nil {
		t.Errorf("Retrying[0] = %+v, want attempt/error/kind/due_at decoded", rt)
	}
	if state.CodexTotals.TotalTokens != 300 || state.CodexTotals.SecondsRunning != 1.5 {
		t.Errorf("CodexTotals = %+v, want total_tokens=300 seconds_running=1.5", state.CodexTotals)
	}
	// The dashboard renders the bucket via formatRateLimits, which reads the
	// decoded map; assert the nested object survived decoding into map[string]any.
	primary, ok := state.RateLimits["primary"].(map[string]any)
	if !ok || primary["remaining"] != float64(42) {
		t.Errorf("RateLimits[\"primary\"] = %#v, want decoded bucket with remaining=42", state.RateLimits["primary"])
	}
}

func TestFetchStateWithAuthSendsBearerToken(t *testing.T) {
	body := `{"poll_interval_ms":5000,"max_concurrent_agents":1,"counts":{},"running":[],"retrying":[],"codex_totals":{}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer state-token" {
			t.Fatalf("Authorization = %q, want Bearer token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	client := &http.Client{}
	if _, err := fetchStateWithAuth(context.Background(), client, srv.URL, "state-token"); err != nil {
		t.Fatalf("fetchStateWithAuth error = %v", err)
	}
}

func TestStateAPIAuthTokenFromEnvTrimsWhitespace(t *testing.T) {
	t.Setenv(stateAPIAuthTokenEnv, "  state-token\n")
	if got := stateAPIAuthTokenFromEnv(); got != "state-token" {
		t.Fatalf("stateAPIAuthTokenFromEnv() = %q, want trimmed token", got)
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

// ── terminalColumns precedence (live tty width → COLUMNS → default) ────────────
// Mirrors :io.columns() precedence: the live terminal width wins, then COLUMNS,
// then the @default_terminal_columns / computed-default fallbacks. The live
// width is stubbed because a non-tty test process can't be forced to report one.

func TestTerminalColumns_Precedence(t *testing.T) {
	orig := liveTerminalWidth
	t.Cleanup(func() { liveTerminalWidth = orig })

	t.Run("live width preferred over COLUMNS", func(t *testing.T) {
		t.Setenv("COLUMNS", "200")
		liveTerminalWidth = func() (int, bool) { return 137, true }
		if got := terminalColumns(); got != 137 {
			t.Errorf("terminalColumns() = %d, want 137 (live tty width must win over COLUMNS)", got)
		}
	})

	t.Run("COLUMNS used when live width unavailable", func(t *testing.T) {
		liveTerminalWidth = func() (int, bool) { return 0, false }
		t.Setenv("COLUMNS", "200")
		if got := terminalColumns(); got != 200 {
			t.Errorf("terminalColumns() = %d, want 200 (COLUMNS fallback)", got)
		}
	})

	t.Run("invalid COLUMNS falls back to default", func(t *testing.T) {
		liveTerminalWidth = func() (int, bool) { return 0, false }
		t.Setenv("COLUMNS", "not-a-number")
		if got := terminalColumns(); got != defaultTermCols {
			t.Errorf("terminalColumns() = %d, want %d (@default_terminal_columns)", got, defaultTermCols)
		}
	})

	t.Run("computed default when neither live width nor COLUMNS", func(t *testing.T) {
		liveTerminalWidth = func() (int, bool) { return 0, false }
		t.Setenv("COLUMNS", "")
		want := fixedRunningWidth() + chromeWidth + colEventDef
		if got := terminalColumns(); got != want {
			t.Errorf("terminalColumns() = %d, want %d (computed default)", got, want)
		}
	})
}

// ── screen lifecycle (alt-screen / cursor / non-TTY degrade) ──────────────────

// On a TTY the per-frame output must stay byte-for-byte identical to upstream:
// clear sequence + content + newline.
func TestScreen_Draw_TTYKeepsParityBytes(t *testing.T) {
	var buf bytes.Buffer
	scr := newScreen(&buf, true /* isTTY */, false /* raw */)
	scr.draw("BODY")
	if got, want := buf.String(), ansiClear+"BODY\n"; got != want {
		t.Errorf("draw on TTY = %q, want %q", got, want)
	}
}

// Non-TTY output must not contain the clear / alt-screen / cursor control bytes
// (item 5: piped/redirected output should be plain).
func TestScreen_Draw_NonTTYDropsControlBytes(t *testing.T) {
	var buf bytes.Buffer
	scr := newScreen(&buf, false /* isTTY */, false /* raw */)
	scr.enter()
	scr.draw("BODY")
	scr.restore()

	out := buf.String()
	if out != "BODY\n" {
		t.Errorf("non-TTY output = %q, want %q (plain, no control bytes)", out, "BODY\n")
	}
	if strings.Contains(out, "\033") {
		t.Errorf("non-TTY output contains an escape byte: %q", out)
	}
}

func TestScreen_EnterRestore_LifecycleSequences(t *testing.T) {
	var buf bytes.Buffer
	scr := newScreen(&buf, true /* isTTY */, false /* raw */)

	scr.enter()
	if got, want := buf.String(), ansiAltScreenEnter+ansiHideCursor; got != want {
		t.Errorf("enter() = %q, want %q", got, want)
	}

	buf.Reset()
	scr.restore()
	if got, want := buf.String(), ansiShowCursor+ansiReset+ansiAltScreenLeave; got != want {
		t.Errorf("restore() = %q, want %q", got, want)
	}
}

// --raw on a TTY is parity mode: no alt-screen / cursor management, and the
// per-frame body is exactly the upstream clear+content+newline.
func TestScreen_RawModeIsParity(t *testing.T) {
	var buf bytes.Buffer
	scr := newScreen(&buf, true /* isTTY */, true /* raw */)

	scr.enter()
	scr.restore()
	if buf.Len() != 0 {
		t.Errorf("raw mode enter/restore wrote %q, want nothing", buf.String())
	}

	scr.draw("BODY")
	if got, want := buf.String(), ansiClear+"BODY\n"; got != want {
		t.Errorf("raw draw = %q, want %q", got, want)
	}
}

// ── signal-driven graceful restore (item 2) ──────────────────────────────────

// run exits and restores the terminal when its context is cancelled — exactly
// what signal.NotifyContext delivers on SIGINT/SIGTERM.
func TestRun_RestoresTerminalOnSignal(t *testing.T) {
	var buf bytes.Buffer
	scr := newScreen(&buf, true /* isTTY */, false /* raw */)

	fetch := func(ctx context.Context) (*stateapi.StateResponse, error) {
		return &stateapi.StateResponse{MaxConcurrentAgents: 3}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		// A long interval keeps the ticker from firing; only the initial
		// frame is drawn before the context is cancelled.
		run(ctx, scr, fetch, time.Hour, "http://example")
		close(done)
	}()

	// Give the initial frame time to render, then simulate the signal.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after context cancellation")
	}

	out := buf.String()
	if !strings.HasPrefix(out, ansiAltScreenEnter+ansiHideCursor) {
		t.Errorf("output did not start by entering alt-screen / hiding cursor: %q", out[:min(40, len(out))])
	}
	if !strings.HasSuffix(out, ansiShowCursor+ansiReset+ansiAltScreenLeave) {
		t.Errorf("output did not end by restoring the terminal: tail=%q", out[max(0, len(out)-40):])
	}
	if !strings.Contains(out, "AIOPS STATUS") {
		t.Errorf("output did not contain a rendered frame: %q", out)
	}
}

// A signal arriving mid-fetch must abort the fetch and skip the final frame,
// so the only control bytes after the last frame are the restore sequence.
func TestRun_CancelDuringFetchAbortsAndRestores(t *testing.T) {
	var buf bytes.Buffer
	scr := newScreen(&buf, true /* isTTY */, false /* raw */)

	ctx, cancel := context.WithCancel(context.Background())
	fetchStarted := make(chan struct{})

	fetch := func(fctx context.Context) error {
		close(fetchStarted)
		// Block until the loop's context is cancelled (the fetchCtx derives
		// from it), mimicking a slow/hung poll interrupted by Ctrl-C.
		<-fctx.Done()
		return fctx.Err()
	}
	fetchState := func(fctx context.Context) (*stateapi.StateResponse, error) {
		return &stateapi.StateResponse{}, fetch(fctx)
	}

	done := make(chan struct{})
	go func() {
		run(ctx, scr, fetchState, time.Hour, "http://example")
		close(done)
	}()

	<-fetchStarted
	cancel() // signal arrives while the very first fetch is in flight

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after cancellation during fetch")
	}

	// No frame should have been drawn (fetch never returned a usable state
	// before cancellation), but the terminal must still be restored.
	out := buf.String()
	want := ansiAltScreenEnter + ansiHideCursor + ansiShowCursor + ansiReset + ansiAltScreenLeave
	if out != want {
		t.Errorf("output = %q, want enter+restore with no frame (%q)", out, want)
	}
}

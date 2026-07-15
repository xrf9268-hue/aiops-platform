package main

import (
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/xrf9268-hue/aiops-platform/internal/stateapi"
)

// visibleString strips ANSI escape sequences so tests can reason about the
// columns a terminal actually renders. It reuses the package's sanitize
// patterns but, unlike sanitize, leaves spacing and box-drawing runes intact.
func visibleString(s string) string {
	s = reCSISeq.ReplaceAllString(s, "")
	s = reEscapeSeq.ReplaceAllString(s, "")
	return s
}

func visibleWidth(s string) int { return utf8.RuneCountInString(visibleString(s)) }

// ── truncateToWidth ────────────────────────────────────────────────────────

// A line that already fits (counting only visible columns, so ANSI codes are
// free) is returned byte-for-byte; non-positive widths are a no-op.
func TestTruncateToWidth_Unchanged(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		width int
	}{
		{"plain shorter", "hello", 10},
		{"plain exact", "hello", 5},
		{"non-positive width", "hello", 0},
		{"ansi fits (codes are zero-width)", ansiRed + "hi" + ansiReset, 5},
	}
	for _, c := range cases {
		if got := truncateToWidth(c.in, c.width); got != c.in {
			t.Errorf("%s: truncateToWidth(%q, %d) = %q; want unchanged", c.name, c.in, c.width, got)
		}
	}
}

// An over-long line is cut to the width with an ellipsis; colour codes before
// the cut survive and a reset is appended so colour can't bleed past the cut.
func TestTruncateToWidth_Clips(t *testing.T) {
	cases := []struct {
		name, in, wantVisible, wantCode string
		width                           int
	}{
		{name: "plain overflow", in: "hello world", width: 5, wantVisible: "hell…"},
		{name: "ansi overflow", in: ansiRed + "hello world" + ansiReset, width: 5, wantVisible: "hell…", wantCode: ansiRed},
	}
	for _, c := range cases {
		got := truncateToWidth(c.in, c.width)
		if vis := visibleString(got); vis != c.wantVisible {
			t.Errorf("%s: truncateToWidth(%q, %d) visible = %q; want %q", c.name, c.in, c.width, vis, c.wantVisible)
		}
		if !strings.HasSuffix(got, "…"+ansiReset) {
			t.Errorf("%s: truncateToWidth(%q, %d) = %q; want an ellipsis+reset suffix", c.name, c.in, c.width, got)
		}
		if c.wantCode != "" && !strings.Contains(got, c.wantCode) {
			t.Errorf("%s: truncateToWidth(%q, %d) = %q; dropped colour code %q", c.name, c.in, c.width, got, c.wantCode)
		}
	}
}

// ── cellRight (TOKENS column parity with format_cell(.., :right)) ───────────

func TestCellRight_RightAlignsAndTruncates(t *testing.T) {
	t.Run("short value is right-aligned", func(t *testing.T) {
		if got := cellRight("42", 10); got != "        42" {
			t.Errorf("cellRight(%q, 10) = %q; want %q", "42", got, "        42")
		}
	})
	t.Run("long value is truncated to width", func(t *testing.T) {
		got := cellRight("1,000,000,000", 10) // 13 runes → must not exceed 10
		if utf8.RuneCountInString(got) != 10 {
			t.Errorf("cellRight(%q, 10) width = %d; want 10 (got %q)", "1,000,000,000", utf8.RuneCountInString(got), got)
		}
		if got != "1,000,0..." {
			t.Errorf("cellRight(%q, 10) = %q; want %q", "1,000,000,000", got, "1,000,0...")
		}
	})
}

// ── alignment: a long token count must not push EVENT out of column ─────────

// TestFormatRunningRow_TokensDoNotCorruptAlignment guards the port-parity fix:
// upstream renders TOKENS with format_cell(.., :right) (truncating), so a huge
// token value cannot widen the row. A regression to a non-truncating right pad
// makes the row wider than the fixed layout, which this asserts against.
func TestFormatRunningRow_TokensDoNotCorruptAlignment(t *testing.T) {
	now := time.Now()
	started := now.Add(-90 * time.Second)
	const eventWidth = 44
	wantWidth := fixedRunningWidth() + chromeWidth + eventWidth // 86 + 11 + 44 = 141

	r := stateapi.Running{
		Identifier:        "ENG-1234",
		State:             "running",
		SessionID:         "sess-1", // short → no "..." from the session cell
		TurnCount:         7,
		LastEvent:         "codex/event/token_count",
		StartedAt:         &started,
		Tokens:            stateapi.RunningTokens{TotalTokens: 1_000_000_000}, // "1,000,000,000" = 13 runes
		CodexAppServerPID: 12345,
	}

	row := formatRunningRow(r, now, eventWidth)
	if got := visibleWidth(row); got != wantWidth {
		t.Errorf("formatRunningRow visible width = %d; want %d (a long token count corrupted alignment): %q", got, wantWidth, visibleString(row))
	}
	if !strings.Contains(row, "1,000,0...") {
		t.Errorf("formatRunningRow TOKENS not truncated; row = %q", visibleString(row))
	}
}

// ── acceptance criteria: readable at 80 / 100 / 120 columns ─────────────────

func representativeState(now time.Time) *stateapi.StateResponse {
	started := now.Add(-3600 * time.Second)
	due := now.Add(30 * time.Second)
	return &stateapi.StateResponse{
		MaxConcurrentAgents: 10,
		AgentDefault:        "codex-app-server",
		Counts: stateapi.Counts{
			CompletedTotal:                    12,
			AgentHandoffReconcileStoppedTotal: 3,
			AgentHandoffReconcileStopped:      2,
			ActiveSuccessNoHandoffTotal:       5,
		},
		BudgetGuardrails: stateapi.BudgetGuardrails{MaxTokensPerClaim: 20_000_000, MaxRuntimeSecondsPerClaim: 7200},
		CodexTotals:      stateapi.CodexTotals{InputTokens: 123456, OutputTokens: 654321, TotalTokens: 1_000_000_000, SecondsRunning: 3600},
		Running: []stateapi.Running{{
			Identifier:        "ENG-1234",
			State:             "running",
			SessionID:         "01HXAYZ-very-long-session-identifier-0001",
			TurnCount:         7,
			LastEvent:         "codex/event/token_count",
			LastMessage:       "notification", // the word that wrapped (noti / fication) in #466
			StartedAt:         &started,
			LastEventAt:       &now,
			Tokens:            stateapi.RunningTokens{TotalTokens: 1_000_000_000},
			CodexAppServerPID: 12345,
			AgentProvider:     "codex-app-server",
			AgentModel:        "gpt-5.3-codex-spark",
			WorkflowSource:    "file",
			WorkflowPath:      "/srv/reviewer/WORKFLOW.md",
		}},
		Retrying: []stateapi.Retry{{
			Identifier: "ENG-9999",
			Attempt:    2,
			DueAt:      &due,
			Kind:       "quota_backoff",
			Error:      "a deliberately long error message that would overflow a narrow terminal by a wide margin indeed",
		}},
		RateLimits: map[string]interface{}{
			"limit_id":  "anthropic-opus-primary-secondary-credits-very-long-bucket-name",
			"primary":   map[string]interface{}{"remaining": float64(1234567), "limit": float64(2000000), "reset_in_seconds": float64(3600)},
			"secondary": map[string]interface{}{"remaining": float64(987654), "limit": float64(1000000), "reset_in_seconds": float64(60)},
			"credits":   map[string]interface{}{"has_credits": true, "balance": float64(123.45)},
		},
	}
}

func TestRenderFrameExplainsClaimTokenBudgetObservationScope(t *testing.T) {
	orig := liveTerminalWidth
	t.Cleanup(func() { liveTerminalWidth = orig })
	liveTerminalWidth = func() (int, bool) { return 160, true }

	now := time.Now()
	frame := visibleString(renderFrame(representativeState(now), nil, now, 0, "http://127.0.0.1:4001", 5*time.Second))
	for _, want := range []string{
		"Claim budget: 20,000,000 worker-observed Codex tokens | 2h 0m runtime",
		"Token scope: worker-observed, runner-reported Codex usage only",
		"Unmeasured: external GitHub @codex review usage",
		"Unmeasured: other reviewers outside the worker session",
		"Unmeasured: otherwise unreported nested or subagent usage",
	} {
		if !strings.Contains(frame, want) {
			t.Errorf("renderFrame missing %q:\n%s", want, frame)
		}
	}
}

func TestRenderFrame_StaysWithinTerminalWidth(t *testing.T) {
	orig := liveTerminalWidth
	t.Cleanup(func() { liveTerminalWidth = orig })

	now := time.Now()
	state := representativeState(now)

	// 80/100/120 are the issue's representative widths; 40 is an extreme where
	// the running-row event column floors at colEventMin and the whole row is
	// far wider than the terminal.
	for _, width := range []int{40, 80, 100, 120} {
		t.Run(strconv.Itoa(width)+"cols", func(t *testing.T) {
			liveTerminalWidth = func() (int, bool) { return width, true }
			assertFrameWithinWidth(t, state, now, width)
		})
	}
}

// assertFrameWithinWidth renders the frame and checks that no line exceeds the
// terminal width and that the (over-long) rate-limits line was clipped to fill
// it — proving the policy engaged at this width and the line did not wrap.
func assertFrameWithinWidth(t *testing.T, state *stateapi.StateResponse, now time.Time, width int) {
	t.Helper()
	lines := strings.Split(renderFrame(state, nil, now, 100, "http://127.0.0.1:4001", 5*time.Second), "\n")
	for _, line := range lines {
		if w := visibleWidth(line); w > width {
			t.Errorf("at %d cols a line is too wide (visible %d): %q", width, w, visibleString(line))
		}
	}
	rate := findLine(t, lines, "│ Rate Limits:")
	if w := visibleWidth(rate); w != width {
		t.Errorf("at %d cols rate-limits line visible width = %d; want %d (clipped to fill): %q", width, w, width, visibleString(rate))
	}
	if !strings.HasSuffix(rate, "…"+ansiReset) {
		t.Errorf("at %d cols rate-limits line was not clipped with an ellipsis: %q", width, visibleString(rate))
	}
}

// On a non-tty (no live width measured) the frame is emitted in full so piped
// or redirected output keeps every column for pagers / files.
func TestRenderFrame_NoClipWithoutLiveWidth(t *testing.T) {
	orig := liveTerminalWidth
	t.Cleanup(func() { liveTerminalWidth = orig })
	liveTerminalWidth = func() (int, bool) { return 0, false }
	t.Setenv("COLUMNS", "80")

	now := time.Now()
	frame := renderFrame(representativeState(now), nil, now, 0, "http://127.0.0.1:4001", 5*time.Second)

	rate := findLine(t, strings.Split(frame, "\n"), "│ Rate Limits:")
	if visibleWidth(rate) <= 80 {
		t.Errorf("rate-limits line was clipped without a live terminal width (width %d): %q", visibleWidth(rate), visibleString(rate))
	}
	if strings.HasSuffix(rate, "…"+ansiReset) {
		t.Errorf("rate-limits line should not be clipped when no live width is measured: %q", visibleString(rate))
	}
}

func TestRenderFrame_ShowsAgentHandoffCount(t *testing.T) {
	now := time.Now()
	frame := visibleString(renderFrame(representativeState(now), nil, now, 0, "http://127.0.0.1:4001", 5*time.Second))

	if !strings.Contains(frame, "Handoffs: completed 12 | agent 3 (recent 2) | no handoff 5") {
		t.Fatalf("renderFrame missing agent handoff count: %q", frame)
	}
}

// TestRenderFrame_ShowsAgentModel pins #977 on the TUI surface: the running
// table carries a MODEL column with the per-claim model, and the header shows
// the worker default provider. A wide terminal avoids truncating the column.
func TestRenderFrame_ShowsAgentModel(t *testing.T) {
	orig := liveTerminalWidth
	t.Cleanup(func() { liveTerminalWidth = orig })
	liveTerminalWidth = func() (int, bool) { return 160, true }

	now := time.Now()
	frame := visibleString(renderFrame(representativeState(now), nil, now, 0, "http://127.0.0.1:4001", 5*time.Second))

	if !strings.Contains(frame, "MODEL") {
		t.Fatalf("renderFrame missing MODEL running-table column: %q", frame)
	}
	if !strings.Contains(frame, "gpt-5.3-codex-spark") {
		t.Fatalf("renderFrame missing per-claim model: %q", frame)
	}
	// The worker default line pins the provider (agent.default) and renders the
	// model as "unknown" — it is resolved per-run, not configured (#977).
	if !strings.Contains(frame, "Default:    codex-app-server · model unknown") {
		t.Fatalf("renderFrame missing worker default provider/model line: %q", frame)
	}
}

// TestResolvedWorkflowLabel pins every branch of the #983 TUI profile summary:
// path is the discriminator, source is the fallback when no path was resolved
// (default workflow), an unreported row is skipped (continue, not break) so a
// later profile is still found, identical profiles dedupe, and distinct
// profiles (a hot reload mid-flight) are joined so the summary hides none.
func TestResolvedWorkflowLabel(t *testing.T) {
	cases := []struct {
		name string
		rows []stateapi.Running
		want string
	}{
		{"no rows", nil, "unknown"},
		{"row present but no profile reported yet", []stateapi.Running{{Identifier: "ENG-1"}}, "unknown"},
		{"path is the discriminator", []stateapi.Running{{WorkflowSource: "file", WorkflowPath: "/srv/reviewer/WORKFLOW.md"}}, "/srv/reviewer/WORKFLOW.md"},
		{"falls back to source when path absent", []stateapi.Running{{WorkflowSource: "default"}}, "default"},
		{"skips an unreported row and still finds a later profile", []stateapi.Running{{Identifier: "ENG-1"}, {WorkflowPath: "/srv/reviewer/WORKFLOW.md"}}, "/srv/reviewer/WORKFLOW.md"},
		{"dedupes identical profiles", []stateapi.Running{{WorkflowPath: "/srv/maker/WORKFLOW.md"}, {WorkflowPath: "/srv/maker/WORKFLOW.md"}}, "/srv/maker/WORKFLOW.md"},
		{"joins distinct profiles", []stateapi.Running{{WorkflowPath: "/srv/maker/WORKFLOW.md"}, {WorkflowPath: "/srv/reviewer/WORKFLOW.md"}}, "/srv/maker/WORKFLOW.md, /srv/reviewer/WORKFLOW.md"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolvedWorkflowLabel(tc.rows); got != tc.want {
				t.Errorf("resolvedWorkflowLabel(%+v) = %q; want %q", tc.rows, got, tc.want)
			}
		})
	}
}

// TestRenderFrame_ShowsWorkflowProfile pins #983 on the TUI surface: the header
// summarizes which WORKFLOW.md (the profile) produced the active runs, sourced
// from the per-claim workflow_path the worker reported.
func TestRenderFrame_ShowsWorkflowProfile(t *testing.T) {
	orig := liveTerminalWidth
	t.Cleanup(func() { liveTerminalWidth = orig })
	liveTerminalWidth = func() (int, bool) { return 160, true }

	now := time.Now()
	frame := visibleString(renderFrame(representativeState(now), nil, now, 0, "http://127.0.0.1:4001", 5*time.Second))

	if !strings.Contains(frame, "Workflow:   /srv/reviewer/WORKFLOW.md") {
		t.Fatalf("renderFrame missing resolved workflow profile line: %q", frame)
	}
}

// TestRenderFrame_RendersUnknownWorkflowWhenAbsent pins the "unknown, never
// blank" criterion for the profile: with no running claim reporting a workflow,
// the header line renders "unknown" rather than an empty value (#983).
func TestRenderFrame_RendersUnknownWorkflowWhenAbsent(t *testing.T) {
	orig := liveTerminalWidth
	t.Cleanup(func() { liveTerminalWidth = orig })
	liveTerminalWidth = func() (int, bool) { return 160, true }

	now := time.Now()
	state := representativeState(now)
	state.Running = nil
	frame := visibleString(renderFrame(state, nil, now, 0, "http://127.0.0.1:4001", 5*time.Second))

	if !strings.Contains(frame, "Workflow:   unknown") {
		t.Fatalf("renderFrame should render an absent workflow profile as \"unknown\": %q", frame)
	}
}

// TestRenderFrame_RendersUnknownModelWhenAbsent pins the "unknown, never blank"
// criterion: a running claim with no reported model still renders "unknown" in
// the MODEL column rather than an empty cell (#977).
func TestRenderFrame_RendersUnknownModelWhenAbsent(t *testing.T) {
	orig := liveTerminalWidth
	t.Cleanup(func() { liveTerminalWidth = orig })
	liveTerminalWidth = func() (int, bool) { return 160, true }

	now := time.Now()
	state := representativeState(now)
	state.Running[0].AgentModel = ""
	frame := visibleString(renderFrame(state, nil, now, 0, "http://127.0.0.1:4001", 5*time.Second))

	if !strings.Contains(frame, "unknown") {
		t.Fatalf("renderFrame should render an absent model as \"unknown\": %q", frame)
	}
}

// ── rate-limit values must not corrupt the row (#466) ───────────────────────

// A crafted limit_id / reset value with embedded control bytes, newlines or
// ANSI must be neutralised: clipping is per-line, so an unsanitized newline
// would split the row regardless of width.
func TestFormatRateLimits_SanitizesUntrustedValues(t *testing.T) {
	rl := map[string]interface{}{
		"limit_id": "lim\x1b[31mit\nid\x07x",
		"primary":  map[string]interface{}{"remaining": float64(5), "limit": float64(10), "reset_at": "2026\n05\x07"},
	}
	got := formatRateLimits(rl)

	for _, r := range visibleString(got) {
		if r < 0x20 || r == 0x7f {
			t.Errorf("formatRateLimits kept control byte %#x: %q", r, visibleString(got))
		}
	}
	// ansiRed is not used by formatRateLimits, so its presence means the
	// injected colour code survived.
	if strings.Contains(got, ansiRed) {
		t.Errorf("formatRateLimits did not strip an injected colour code: %q", got)
	}
	if !strings.Contains(visibleString(got), "limit id x") {
		t.Errorf("formatRateLimits dropped the sanitized limit_id: %q", visibleString(got))
	}
}

func TestFormatRateLimits_RendersCodexPercentWindows(t *testing.T) {
	oldNow := rateLimitNow
	rateLimitNow = func() time.Time { return time.Unix(1_780_000_000, 0) }
	defer func() { rateLimitNow = oldNow }()

	resetAt := rateLimitNow().Add(90 * time.Minute).Unix()
	rl := map[string]interface{}{
		"limitName": "codex",
		"primary": map[string]interface{}{
			"usedPercent":        float64(14),
			"windowDurationMins": float64(300),
			"resetsAt":           float64(resetAt),
		},
		"secondary": map[string]interface{}{
			"used_percent":         float64(92.5),
			"window_duration_mins": float64(10080),
			"resets_in_seconds":    float64(30),
		},
	}
	got := visibleString(formatRateLimits(rl))

	for _, want := range []string{
		"codex",
		"primary 14% used · 5h window · resets in 1h 30m",
		"secondary 92.5% used · 7d window · reset 30s",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("formatRateLimits(percent windows) = %q, want substring %q", got, want)
		}
	}
	if rawEpoch := strconv.FormatInt(resetAt, 10) + "s"; strings.Contains(got, rawEpoch) {
		t.Errorf("formatRateLimits(percent windows) rendered resets_at as relative seconds: %q", got)
	}
}

func TestFormatRateLimits_RendersLegacyBuckets(t *testing.T) {
	rl := map[string]interface{}{
		"limit_id":  "legacy",
		"primary":   map[string]interface{}{"remaining": float64(42), "limit": float64(100), "reset_in_seconds": float64(30)},
		"secondary": map[string]interface{}{"limit": float64(200)},
		"credits":   map[string]interface{}{"has_credits": true, "balance": float64(12.5)},
	}
	got := visibleString(formatRateLimits(rl))

	for _, want := range []string{
		"legacy",
		"primary 42/100 reset 30s",
		"secondary limit 200",
		"credits 12.50",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("formatRateLimits(legacy buckets) = %q, want substring %q", got, want)
		}
	}
}

// ── ansiSeqLen bounds (no OOB on truncated/lone escapes) ────────────────────

func TestAnsiSeqLen_Bounds(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"csi colour", "\x1b[31m", 5},
		{"csi reset", "\x1b[0m", 4},
		{"bare escape", "\x1bX", 2},
		{"lone trailing ESC", "\x1b", 1},
		{"unterminated CSI at end", "\x1b[12", 4},
	}
	for _, c := range cases {
		if got := ansiSeqLen([]rune(c.in), 0); got != c.want {
			t.Errorf("%s: ansiSeqLen(%q, 0) = %d; want %d", c.name, c.in, got, c.want)
		}
	}
}

func findLine(t *testing.T, lines []string, visiblePrefix string) string {
	t.Helper()
	for _, line := range lines {
		if strings.HasPrefix(visibleString(line), visiblePrefix) {
			return line
		}
	}
	t.Fatalf("no line with visible prefix %q in frame:\n%s", visiblePrefix, strings.Join(lines, "\n"))
	return ""
}

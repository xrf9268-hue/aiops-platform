package main

import (
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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
	wantWidth := fixedRunningWidth() + chromeWidth + eventWidth // 66 + 10 + 44 = 120

	r := runningEntry{
		Identifier:        "ENG-1234",
		State:             "running",
		SessionID:         "sess-1", // short → no "..." from the session cell
		TurnCount:         7,
		LastEvent:         "codex/event/token_count",
		StartedAt:         &started,
		Tokens:            tokenInfo{TotalTokens: 1_000_000_000}, // "1,000,000,000" = 13 runes
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

func representativeState(now time.Time) *stateResponse {
	started := now.Add(-3600 * time.Second)
	due := now.Add(30 * time.Second)
	return &stateResponse{
		MaxConcurrentAgents: 10,
		Counts: stateCounts{
			CompletedTotal:                     12,
			AgentHandoffReconcileStoppedTotal:  3,
			AgentHandoffReconcileStoppedRecent: 2,
		},
		CodexTotals: codexTotals{InputTokens: 123456, OutputTokens: 654321, TotalTokens: 1_000_000_000, SecondsRunning: 3600},
		Running: []runningEntry{{
			Identifier:        "ENG-1234",
			State:             "running",
			SessionID:         "01HXAYZ-very-long-session-identifier-0001",
			TurnCount:         7,
			LastEvent:         "codex/event/token_count",
			LastMessage:       "notification", // the word that wrapped (noti / fication) in #466
			StartedAt:         &started,
			LastEventAt:       &now,
			Tokens:            tokenInfo{TotalTokens: 1_000_000_000},
			CodexAppServerPID: 12345,
		}},
		Retrying: []retryEntry{{
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
func assertFrameWithinWidth(t *testing.T, state *stateResponse, now time.Time, width int) {
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

	if !strings.Contains(frame, "Handoffs: completed 12 | agent 3 (recent 2)") {
		t.Fatalf("renderFrame missing agent handoff count: %q", frame)
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

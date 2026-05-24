// cmd/tui renders the orchestrator status dashboard in the terminal.
// It polls /api/v1/state and redraws the screen on each tick, mirroring
// the upstream Elixir SymphonyElixir.StatusDashboard behaviour.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// ANSI escape codes (mirrors Elixir @ansi_* module attributes)
const (
	ansiReset   = "\033[0m"
	ansiBold    = "\033[1m"
	ansiDim     = "\033[2m"
	ansiRed     = "\033[31m"
	ansiGreen   = "\033[32m"
	ansiYellow  = "\033[33m"
	ansiBlue    = "\033[34m"
	ansiMagenta = "\033[35m"
	ansiCyan    = "\033[36m"
	ansiGray    = "\033[90m"

	// Clear screen + cursor home: equivalent to IO.ANSI.home() <> IO.ANSI.clear()
	ansiClear = "\033[H\033[2J"

	// Terminal lifecycle sequences (not part of the upstream parity frame body).
	ansiAltScreenEnter = "\033[?1049h"
	ansiAltScreenLeave = "\033[?1049l"
	ansiHideCursor     = "\033[?25l"
	ansiShowCursor     = "\033[?25h"
)

// Column widths (mirrors Elixir @running_*_width constants)
const (
	colID       = 8
	colStage    = 14
	colPID      = 8
	colAge      = 12
	colTokens   = 10
	colSession  = 14
	colEventMin = 12
	colEventDef = 44
	chromeWidth = 10 // "│  ● " prefix + spacing

	defaultTermCols = 115 // Elixir @default_terminal_columns
)

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
	Running  int `json:"running"`
	Blocked  int `json:"blocked"`
	Retrying int `json:"retrying"`
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

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	urlFlag := flag.String("url", "http://127.0.0.1:4000/", "worker HTTP API base URL")
	intervalFlag := flag.Duration("interval", 5*time.Second, "poll interval")
	rawFlag := flag.Bool("raw", false, "disable alt-screen/cursor management (upstream parity mode)")
	flag.Parse()

	baseURL := strings.TrimSuffix(*urlFlag, "/")
	// Allow env override when using the default flag value.
	if *urlFlag == "http://127.0.0.1:4000/" {
		if env := os.Getenv("AIOPS_DASHBOARD_URL"); env != "" {
			baseURL = strings.TrimSuffix(env, "/")
		}
	}
	stateURL := baseURL + "/api/v1/state"
	client := &http.Client{Timeout: 10 * time.Second}

	interval := *intervalFlag
	if interval <= 0 {
		fmt.Fprintln(os.Stderr, "--interval must be a positive duration (e.g. 5s)")
		os.Exit(1)
	}

	scr := newScreen(os.Stdout, isTerminal(os.Stdout), *rawFlag)

	// NotifyContext cancels ctx on SIGINT/SIGTERM, which both breaks the render
	// loop and aborts any in-flight fetch (the per-fetch context derives from
	// ctx), so Ctrl-C restores the terminal immediately even mid-poll. This
	// mirrors the signal handling in the sibling cmd/ entrypoints.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A second subscription records which signal fired (NotifyContext discards
	// the value) so it can be re-raised for a conventional exit status. The
	// runtime delivers to this channel before NotifyContext's goroutine cancels
	// ctx, so the value is buffered by the time run returns.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	fetch := func(ctx context.Context) (*stateResponse, error) {
		return fetchState(ctx, client, stateURL)
	}

	run(ctx, scr, fetch, interval, baseURL)

	// run has already restored the terminal (its deferred restore). If a signal
	// caused the shutdown, re-raise it so we exit with the conventional
	// 128+signum status (130 for SIGINT) instead of 0.
	select {
	case s := <-sigCh:
		raiseAfterSignal(s)
	default:
	}
}

// raiseAfterSignal restores the default disposition for sig and re-raises it so
// the process terminates with the conventional 128+signum status, falling back
// to an explicit os.Exit if the re-raised signal doesn't terminate us.
func raiseAfterSignal(sig os.Signal) {
	signal.Reset(os.Interrupt, syscall.SIGTERM)
	if p, err := os.FindProcess(os.Getpid()); err == nil {
		_ = p.Signal(sig)
	}
	time.Sleep(100 * time.Millisecond)
	if sysSig, ok := sig.(syscall.Signal); ok {
		os.Exit(128 + int(sysSig))
	}
	os.Exit(1)
}

// run drives the poll/render loop until ctx is cancelled (signal or otherwise),
// restoring terminal state on the way out so an interrupted dashboard never
// leaves the real terminal in alt-screen / cursor-hidden state.
func run(ctx context.Context, scr *screen, fetch func(context.Context) (*stateResponse, error), interval time.Duration, baseURL string) {
	scr.enter()
	defer scr.restore()

	var samples []tokenSample
	var lastTPS float64
	var lastTPSSec int64

	drawOnce := func() {
		// Bound each fetch to the poll interval so a slow/hung worker can't
		// outlive its tick and stall the next refresh (#356). A fetch that
		// cannot finish within the interval is surfaced as an "unavailable"
		// frame rather than blocking the loop.
		fetchCtx, cancel := context.WithTimeout(ctx, interval)
		defer cancel()

		state, fetchErr := fetch(fetchCtx)
		// If we're shutting down, don't paint a spurious final (error) frame —
		// just unwind so the deferred restore runs.
		if ctx.Err() != nil {
			return
		}
		now := time.Now()

		if fetchErr == nil {
			samples = pruneSamples(append([]tokenSample{{at: now, tokens: state.CodexTotals.TotalTokens}}, samples...), now)
			if sec := now.Unix(); sec != lastTPSSec {
				lastTPS = rollingTPS(samples, now, state.CodexTotals.TotalTokens)
				lastTPSSec = sec
			}
		}

		content := safeRenderFrame(state, fetchErr, now, lastTPS, baseURL, interval)
		scr.draw(content)
	}

	drawOnce()
	if ctx.Err() != nil {
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			drawOnce()
		}
	}
}

// ── Terminal screen lifecycle ──────────────────────────────────────────────────

// screen wraps the output writer with optional alt-screen / cursor management.
// The per-frame body bytes are identical across modes; only the surrounding
// terminal control sequences differ, so upstream parity is preserved.
type screen struct {
	w io.Writer
	// isTTY gates the per-frame clear sequence (the ANSI clear is control noise
	// when stdout is piped or redirected).
	isTTY bool
	// lifecycle gates alt-screen + cursor management. Active only on a real TTY
	// when --raw is not set.
	lifecycle bool
}

func newScreen(w io.Writer, isTTY, raw bool) *screen {
	return &screen{w: w, isTTY: isTTY, lifecycle: isTTY && !raw}
}

// enter switches to the alternate screen buffer and hides the cursor.
func (s *screen) enter() {
	if s.lifecycle {
		io.WriteString(s.w, ansiAltScreenEnter+ansiHideCursor)
	}
}

// restore reverses enter: show cursor, reset attributes, leave alt-screen.
func (s *screen) restore() {
	if s.lifecycle {
		io.WriteString(s.w, ansiShowCursor+ansiReset+ansiAltScreenLeave)
	}
}

// draw writes a single rendered frame. On a TTY each frame is preceded by the
// upstream clear sequence; on a non-TTY the clear is dropped so the output is
// plain, appendable text.
func (s *screen) draw(content string) {
	if s.isTTY {
		io.WriteString(s.w, ansiClear+content+"\n")
	} else {
		io.WriteString(s.w, content+"\n")
	}
}

// safeRenderFrame wraps renderFrame with panic recovery, mirroring the
// `rescue` blocks in status_dashboard.ex (maybe_render/1, render_content/3).
func safeRenderFrame(state *stateResponse, fetchErr error, now time.Time, tps float64, baseURL string, interval time.Duration) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = strings.Join([]string{
				colorize("╭─ AIOPS STATUS", ansiBold),
				colorize(fmt.Sprintf("│ Render error: %v", r), ansiRed),
				colorize("╰─", ansiBold),
			}, "\n")
		}
	}()
	return renderFrame(state, fetchErr, now, tps, baseURL, interval)
}

func fetchState(ctx context.Context, client *http.Client, url string) (*stateResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
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

// ── Rendering ────────────────────────────────────────────────────────────────
// Mirrors format_snapshot_content/2 in status_dashboard.ex.

func renderFrame(state *stateResponse, fetchErr error, now time.Time, tps float64, baseURL string, interval time.Duration) string {
	if fetchErr != nil {
		return strings.Join([]string{
			colorize("╭─ AIOPS STATUS", ansiBold),
			colorize("│ Orchestrator snapshot unavailable: ", ansiRed) + colorize(fetchErr.Error(), ansiRed),
			colorize("│ Throughput: ", ansiBold) + colorize(formatTPS(tps)+" tps", ansiCyan),
			colorize("│ Dashboard:  ", ansiBold) + colorize(baseURL+"/", ansiCyan),
			colorize("│ Next refresh: ", ansiBold) + colorize(strconv.Itoa(int(interval.Seconds()))+"s", ansiCyan),
			colorize("╰─", ansiBold),
		}, "\n")
	}

	cols := terminalColumns()
	eventWidth := maxInt(colEventMin, cols-fixedRunningWidth()-chromeWidth)

	agentCount := len(state.Running)
	maxAgents := state.MaxConcurrentAgents

	lines := []string{
		colorize("╭─ AIOPS STATUS", ansiBold),
		colorize("│ Agents: ", ansiBold) +
			colorize(formatCount(int64(agentCount)), ansiGreen) +
			colorize("/", ansiGray) +
			colorize(formatCount(int64(maxAgents)), ansiGray),
		colorize("│ Throughput: ", ansiBold) + colorize(formatTPS(tps)+" tps", ansiCyan),
		colorize("│ Runtime:    ", ansiBold) + colorize(formatRuntimeSecs(state.CodexTotals.SecondsRunning), ansiMagenta),
		colorize("│ Tokens:     ", ansiBold) +
			colorize("in "+formatCount(state.CodexTotals.InputTokens), ansiYellow) +
			colorize(" | ", ansiGray) +
			colorize("out "+formatCount(state.CodexTotals.OutputTokens), ansiYellow) +
			colorize(" | ", ansiGray) +
			colorize("total "+formatCount(state.CodexTotals.TotalTokens), ansiYellow),
		colorize("│ Rate Limits: ", ansiBold) + formatRateLimits(state.RateLimits),
		colorize("│ Dashboard:   ", ansiBold) + colorize(baseURL+"/", ansiCyan),
		colorize("│ Next refresh: ", ansiBold) + colorize(strconv.Itoa(int(interval.Seconds()))+"s", ansiCyan),
		colorize("├─ Running", ansiBold),
		"│",
		runningTableHeader(eventWidth),
		runningTableSep(eventWidth),
	}

	sort.Slice(state.Running, func(i, j int) bool {
		a := state.Running[i].Identifier
		if a == "" {
			a = state.Running[i].IssueID
		}
		b := state.Running[j].Identifier
		if b == "" {
			b = state.Running[j].IssueID
		}
		return a < b
	})

	if len(state.Running) == 0 {
		lines = append(lines, "│  "+colorize("No active agents", ansiGray), "│")
	} else {
		for _, r := range state.Running {
			lines = append(lines, formatRunningRow(r, now, eventWidth))
		}
		lines = append(lines, "│")
	}

	// Backoff queue (mirrors format_retry_rows)
	sort.Slice(state.Retrying, func(i, j int) bool {
		var di, dj time.Duration
		if state.Retrying[i].DueAt != nil {
			di = time.Until(*state.Retrying[i].DueAt)
		}
		if state.Retrying[j].DueAt != nil {
			dj = time.Until(*state.Retrying[j].DueAt)
		}
		return di < dj
	})

	lines = append(lines, colorize("├─ Backoff queue", ansiBold), "│")
	if len(state.Retrying) == 0 {
		lines = append(lines, "│  "+colorize("No queued retries", ansiGray))
	} else {
		for _, r := range state.Retrying {
			lines = append(lines, formatRetryRow(r))
		}
	}
	lines = append(lines, colorize("╰─", ansiBold))
	return strings.Join(lines, "\n")
}

// runningTableHeader mirrors running_table_header_row/1.
func runningTableHeader(eventWidth int) string {
	header := strings.Join([]string{
		padRight("ID", colID),
		padRight("STAGE", colStage),
		padRight("PID", colPID),
		padRight("AGE / TURN", colAge),
		padLeft("TOKENS", colTokens),
		padRight("SESSION", colSession),
		padRight("EVENT", eventWidth),
	}, " ")
	return "│   " + colorize(header, ansiGray)
}

// runningTableSep mirrors running_table_separator_row/1.
func runningTableSep(eventWidth int) string {
	width := fixedRunningWidth() + eventWidth + 6
	return "│   " + colorize(strings.Repeat("─", width), ansiGray)
}

func fixedRunningWidth() int {
	return colID + colStage + colPID + colAge + colTokens + colSession
}

// formatRunningRow mirrors format_running_summary/2.
func formatRunningRow(r runningEntry, now time.Time, eventWidth int) string {
	id := cell(issueLabel(r.Identifier, r.IssueID), colID)
	stage := cell(r.State, colStage)

	pid := "n/a"
	if r.CodexAppServerPID > 0 {
		pid = strconv.Itoa(r.CodexAppServerPID)
	}
	pidCell := cell(pid, colPID)

	var runtimeSecs float64
	if r.StartedAt != nil {
		runtimeSecs = now.Sub(*r.StartedAt).Seconds()
	}
	age := cell(formatRuntimeAndTurns(runtimeSecs, r.TurnCount), colAge)
	tokens := padLeft(formatCount(r.Tokens.TotalTokens), colTokens)
	session := cell(compactSession(r.SessionID), colSession)

	eventText := r.LastMessage
	if eventText == "" {
		eventText = r.LastEvent
	}
	if eventText == "" {
		eventText = "none"
	}
	event := cell(sanitize(eventText), eventWidth)

	dotColor := statusDotColor(r.LastEvent)

	return "│ " +
		colorize("●", dotColor) + " " +
		colorize(id, ansiCyan) + " " +
		colorize(stage, dotColor) + " " +
		colorize(pidCell, ansiYellow) + " " +
		colorize(age, ansiMagenta) + " " +
		colorize(tokens, ansiYellow) + " " +
		colorize(session, ansiCyan) + " " +
		colorize(event, dotColor)
}

// formatRetryRow mirrors format_retry_summary/1.
func formatRetryRow(r retryEntry) string {
	id := issueLabel(r.Identifier, r.IssueID)
	attempt := strconv.Itoa(r.Attempt)

	dueIn := "n/a"
	if r.DueAt != nil {
		d := time.Until(*r.DueAt)
		if d < 0 {
			d = 0
		}
		secs := int(d.Seconds())
		millis := int(d.Milliseconds()) % 1000
		dueIn = fmt.Sprintf("%d.%03ds", secs, millis)
	}

	errorPart := ""
	if r.Error != "" {
		errStr := sanitize(r.Error)
		if len(errStr) > 96 {
			errStr = errStr[:93] + "..."
		}
		errorPart = " " + colorize("error="+errStr, ansiDim)
	}

	return "│  " +
		colorize("↻", ansiYellow) + " " +
		colorize(id, ansiRed) + " " +
		colorize("attempt="+attempt, ansiYellow) +
		colorize(" in ", ansiDim) +
		colorize(dueIn, ansiCyan) +
		errorPart
}

// ── Rate-limit formatting ────────────────────────────────────────────────────
// Mirrors format_rate_limits/1 in status_dashboard.ex.

func formatRateLimits(rl map[string]interface{}) string {
	if rl == nil {
		return colorize("unavailable", ansiGray)
	}
	limitID := strMapGet(rl, "limit_id", "limit_name", "")
	if limitID == "" {
		limitID = "unknown"
	}
	primary := formatRateLimitBucket(rl["primary"])
	secondary := formatRateLimitBucket(rl["secondary"])
	credits := formatCredits(rl["credits"])

	return colorize(limitID, ansiYellow) +
		colorize(" | ", ansiGray) +
		colorize("primary "+primary, ansiCyan) +
		colorize(" | ", ansiGray) +
		colorize("secondary "+secondary, ansiCyan) +
		colorize(" | ", ansiGray) +
		colorize(credits, ansiGreen)
}

func formatRateLimitBucket(v interface{}) string {
	if v == nil {
		return "n/a"
	}
	m, ok := v.(map[string]interface{})
	if !ok {
		return fmt.Sprintf("%v", v)
	}
	remaining := intVal(m, "remaining")
	limit := intVal(m, "limit")

	base := "n/a"
	switch {
	case remaining != nil && limit != nil:
		base = formatCount(*remaining) + "/" + formatCount(*limit)
	case remaining != nil:
		base = "remaining " + formatCount(*remaining)
	case limit != nil:
		base = "limit " + formatCount(*limit)
	case len(m) == 0:
		return "n/a"
	}

	// format_reset_value/1: integers are rendered with an "s" suffix.
	for _, k := range []string{"reset_in_seconds", "resetInSeconds", "reset_at", "resetAt", "resets_at"} {
		if rv, ok := m[k]; ok && rv != nil {
			return base + " reset " + formatResetValue(rv)
		}
	}
	return base
}

func formatCredits(v interface{}) string {
	if v == nil {
		return "credits n/a"
	}
	m, ok := v.(map[string]interface{})
	if !ok {
		return "credits n/a"
	}
	if unlimited, _ := m["unlimited"].(bool); unlimited {
		return "credits unlimited"
	}
	hasCredits, _ := m["has_credits"].(bool)
	if hasCredits {
		if balance, ok := m["balance"]; ok {
			return fmt.Sprintf("credits %.2f", toFloat(balance))
		}
		return "credits available"
	}
	return "credits none"
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func colorize(s, code string) string { return code + s + ansiReset }

func issueLabel(identifier, issueID string) string {
	if identifier != "" {
		return identifier
	}
	if issueID != "" {
		return issueID
	}
	return "unknown"
}

// compactSession mirrors compact_session_id/1.
func compactSession(id string) string {
	if id == "" {
		return "n/a"
	}
	if utf8.RuneCountInString(id) > 10 {
		r := []rune(id)
		return string(r[:4]) + "..." + string(r[len(r)-6:])
	}
	return id
}

// cell left-pads to width, truncating with "…" when needed.
// Mirrors format_cell/3 in status_dashboard.ex.
func cell(s string, width int) string {
	s = sanitize(s)
	runes := []rune(s)
	if len(runes) > width {
		if width <= 3 {
			return string(runes[:width])
		}
		s = string(runes[:width-3]) + "..."
	}
	return fmt.Sprintf("%-*s", width, s)
}

func padLeft(s string, width int) string  { return fmt.Sprintf("%*s", width, s) }
func padRight(s string, width int) string { return fmt.Sprintf("%-*s", width, s) }

// Compiled once at package init — used by sanitize to strip ANSI and control bytes.
// Mirrors sanitize_ansi_and_control_bytes/1 in status_dashboard.ex.
var (
	reCSISeq     = regexp.MustCompile(`\x1B\[[0-9;]*[A-Za-z]`) // CSI sequences e.g. \x1B[31m
	reEscapeSeq  = regexp.MustCompile(`\x1B.`)                 // any other \x1B + one char
	reControlByt = regexp.MustCompile(`[\x00-\x1F\x7F]`)       // control bytes incl. \r \n \t
)

// sanitize strips ANSI escape sequences and control bytes from API-sourced strings
// before they are printed to the terminal, preventing terminal injection via
// crafted last_message / error fields. Control bytes (incl. \n \r \t) are
// replaced with a space so adjacent words don't merge — matching the Elixir
// inline_text/1 behaviour of replacing "\n" with " " before display.
func sanitize(s string) string {
	s = reCSISeq.ReplaceAllString(s, "")
	s = reEscapeSeq.ReplaceAllString(s, "")
	s = reControlByt.ReplaceAllString(s, " ")
	return strings.Join(strings.Fields(s), " ")
}

func formatCount(n int64) string {
	if n < 0 {
		return "-" + groupThousands(strconv.FormatInt(-n, 10))
	}
	return groupThousands(strconv.FormatInt(n, 10))
}

func groupThousands(s string) string {
	if len(s) <= 3 {
		return s
	}
	return groupThousands(s[:len(s)-3]) + "," + s[len(s)-3:]
}

func formatTPS(tps float64) string {
	return groupThousands(strconv.FormatInt(int64(math.Round(tps)), 10))
}

// formatRuntimeSecs mirrors format_runtime_seconds/1.
func formatRuntimeSecs(seconds float64) string {
	total := int(math.Max(0, math.Floor(seconds)))
	mins := total / 60
	secs := total % 60
	return fmt.Sprintf("%dm %ds", mins, secs)
}

// formatRuntimeAndTurns mirrors format_runtime_and_turns/2.
func formatRuntimeAndTurns(seconds float64, turns int) string {
	if turns > 0 {
		return fmt.Sprintf("%s / %d", formatRuntimeSecs(seconds), turns)
	}
	return formatRuntimeSecs(seconds)
}

// statusDotColor mirrors the status_color switch in format_running_summary/2.
// The Elixir match uses the atom :none; over JSON that serializes to "none".
// The Go API emits "" (zero value) when no event has been observed yet, so
// both "" and "none" map to the idle/red state.
func statusDotColor(event string) string {
	switch event {
	case "", "none":
		return ansiRed
	case "codex/event/token_count":
		return ansiYellow
	case "codex/event/task_started":
		return ansiGreen
	case "turn_completed":
		return ansiMagenta
	default:
		return ansiBlue
	}
}

// terminalColumns mirrors terminal_columns/0 + terminal_columns_from_env/0.
// When COLUMNS is absent: Elixir computes fixed_running_width + chrome + event_default.
// When COLUMNS is present but invalid: Elixir falls back to @default_terminal_columns (115).
func terminalColumns() int {
	s := os.Getenv("COLUMNS")
	if s == "" {
		return fixedRunningWidth() + chromeWidth + colEventDef
	}
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
		return n
	}
	return defaultTermCols
}

// formatResetValue mirrors format_reset_value/1: integers get an "s" suffix.
func formatResetValue(v interface{}) string {
	switch x := v.(type) {
	case float64:
		return strconv.FormatInt(int64(x), 10) + "s"
	case int64:
		return strconv.FormatInt(x, 10) + "s"
	case int:
		return strconv.Itoa(x) + "s"
	case string:
		return x
	default:
		return fmt.Sprintf("%v", v)
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func strMapGet(m map[string]interface{}, keys ...string) string {
	// last arg is default
	def := keys[len(keys)-1]
	for _, k := range keys[:len(keys)-1] {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok {
				return s
			}
			return fmt.Sprintf("%v", v)
		}
	}
	return def
}

func intVal(m map[string]interface{}, key string) *int64 {
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	switch x := v.(type) {
	case float64:
		n := int64(x)
		return &n
	case int64:
		return &x
	case int:
		n := int64(x)
		return &n
	}
	return nil
}

func toFloat(v interface{}) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int64:
		return float64(x)
	case int:
		return float64(x)
	}
	return 0
}

// cmd/tui frame rendering: renderFrame and its table/format/terminal-width
// helpers, mirroring format_snapshot_content/2 and friends in the upstream
// Elixir status_dashboard.ex. The per-line width clipping policy lives in
// layout.go; the poll loop and terminal lifecycle live in main.go.
package main

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xrf9268-hue/aiops-platform/internal/stateapi"
)

// Column widths (mirrors Elixir @running_*_width constants)
const (
	colID       = 8
	colStage    = 14
	colPID      = 8
	colAge      = 12
	colTokens   = 10
	colSession  = 14
	colModel    = 20
	colEventMin = 12
	colEventDef = 44
	chromeWidth = 11 // "│ ● " prefix + one space between each of the 8 running columns

	defaultTermCols = 115 // Elixir @default_terminal_columns
)

// safeRenderFrame wraps renderFrame with panic recovery, mirroring the
// `rescue` blocks in status_dashboard.ex (maybe_render/1, render_content/3).
func safeRenderFrame(state *stateapi.StateResponse, fetchErr error, now time.Time, tps float64, baseURL string, interval time.Duration) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = clipFrame([]string{
				colorize("╭─ AIOPS STATUS", ansiBold),
				colorize(fmt.Sprintf("│ Render error: %v", r), ansiRed),
				colorize("╰─", ansiBold),
			})
		}
	}()
	return renderFrame(state, fetchErr, now, tps, baseURL, interval)
}

// ── Rendering ────────────────────────────────────────────────────────────────
// Mirrors format_snapshot_content/2 in status_dashboard.ex.

func renderFrame(state *stateapi.StateResponse, fetchErr error, now time.Time, tps float64, baseURL string, interval time.Duration) string { //nolint:gocognit,funlen // baseline (#521)
	if fetchErr != nil {
		return clipFrame([]string{
			colorize("╭─ AIOPS STATUS", ansiBold),
			colorize("│ Orchestrator snapshot unavailable: ", ansiRed) + colorize(fetchErr.Error(), ansiRed),
			colorize("│ Throughput: ", ansiBold) + colorize(formatTPS(tps)+" tps", ansiCyan),
			colorize("│ Dashboard:  ", ansiBold) + colorize(baseURL+"/", ansiCyan),
			colorize("│ Next refresh: ", ansiBold) + colorize(strconv.Itoa(int(interval.Seconds()))+"s", ansiCyan),
			colorize("╰─", ansiBold),
		})
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
		// Worker default runtime/provider (agent.default). The model is resolved
		// per-run by the agent (shown per-claim in the MODEL column), so the
		// worker-level default model is "unknown" rather than hidden (#977).
		colorize("│ Default:    ", ansiBold) +
			colorize(orUnknown(state.AgentDefault), ansiCyan) +
			colorize(" · model ", ansiGray) +
			colorize("unknown", ansiGray),
		// Which WORKFLOW.md (the profile, e.g. reviewer vs maker) produced the
		// active runs (#983). /api/v1/state carries the profile per claim, but a
		// single TUI views one worker — which resolves one workflow — so a
		// per-row column would be identical cells stealing width from EVENT;
		// summarize it once here, "unknown" until a run reports it.
		colorize("│ Workflow:   ", ansiBold) +
			colorize(resolvedWorkflowLabel(state.Running), ansiCyan),
		colorize("│ Handoffs: ", ansiBold) +
			colorize("completed "+formatCount(state.Counts.CompletedTotal), ansiGreen) +
			colorize(" | ", ansiGray) +
			colorize("agent "+formatCount(state.Counts.AgentHandoffReconcileStoppedTotal), ansiGreen) +
			colorize(" (recent "+formatCount(int64(state.Counts.AgentHandoffReconcileStopped))+")", ansiGray),
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
	return clipFrame(lines)
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
		padRight("MODEL", colModel),
		padRight("EVENT", eventWidth),
	}, " ")
	return "│   " + colorize(header, ansiGray)
}

// runningTableSep mirrors running_table_separator_row/1.
func runningTableSep(eventWidth int) string {
	width := fixedRunningWidth() + eventWidth + 7
	return "│   " + colorize(strings.Repeat("─", width), ansiGray)
}

func fixedRunningWidth() int {
	return colID + colStage + colPID + colAge + colTokens + colSession + colModel
}

// formatRunningRow mirrors format_running_summary/2.
func formatRunningRow(r stateapi.Running, now time.Time, eventWidth int) string {
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
	// Right-aligned *and* truncated (mirrors format_cell(.., :right)); a long
	// token count must not widen the row and push EVENT out of alignment (#466).
	tokens := cellRight(formatCount(r.Tokens.TotalTokens), colTokens)
	session := cell(compactSession(r.SessionID), colSession)
	model := cell(orUnknown(r.AgentModel), colModel)

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
		colorize(model, ansiGray) + " " +
		colorize(event, dotColor)
}

// orUnknown renders an agent model/provider value, falling back to "unknown" so
// a missing value is explicit diagnostic data rather than a blank cell (#977).
func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

// resolvedWorkflowLabel summarizes which WORKFLOW.md produced the active runs
// (#983). The path is the discriminator operators recognize; a default-workflow
// run reports source=default with no path. The worker resolves one workflow, so
// every running row carries the same profile and the first reported one is the
// label; distinct profiles (a hot reload mid-flight) are joined so the summary
// never hides one. Returns "unknown" until a run reports a profile.
func resolvedWorkflowLabel(rows []stateapi.Running) string {
	seen := map[string]struct{}{}
	var labels []string
	for _, r := range rows {
		label := strings.TrimSpace(r.WorkflowPath)
		if label == "" {
			label = strings.TrimSpace(r.WorkflowSource)
		}
		if label == "" {
			continue
		}
		if _, dup := seen[label]; dup {
			continue
		}
		seen[label] = struct{}{}
		labels = append(labels, label)
	}
	if len(labels) == 0 {
		return "unknown"
	}
	return strings.Join(labels, ", ")
}

// formatRetryRow mirrors format_retry_summary/1.
func formatRetryRow(r stateapi.Retry) string {
	id := issueLabel(r.Identifier, r.IssueID)
	attempt := strconv.Itoa(r.Attempt)
	kind := r.Kind
	if kind == "" {
		kind = "failure"
	}

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
	startupPart := ""
	if r.StartupFailure != nil && r.StartupFailure.Phase != "" {
		startupPart = " " + colorize("startup_phase="+sanitize(r.StartupFailure.Phase), ansiDim)
	}
	return "│  " +
		colorize("↻", ansiYellow) + " " +
		colorize(id, ansiRed) + " " +
		colorize("kind="+kind, ansiYellow) + " " +
		colorize("attempt="+attempt, ansiYellow) +
		colorize(" in ", ansiDim) +
		colorize(dueIn, ansiCyan) +
		startupPart +
		errorPart
}

// ── Rate-limit formatting ────────────────────────────────────────────────────
// Mirrors format_rate_limits/1 in status_dashboard.ex.

func formatRateLimits(rl map[string]interface{}) string {
	if rl == nil {
		return colorize("unavailable", ansiGray)
	}
	// limit_id is the one free-form API string on this line; sanitize it so an
	// embedded newline/control byte can't split or corrupt the row (#466). The
	// numeric bucket/credit fields are formatted from typed values below.
	limitID := sanitize(strMapGet(rl, "limit_id", "limit_name", ""))
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
		return sanitize(fmt.Sprintf("%v", v))
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

// cell left-aligns to width, truncating with "..." when needed.
// Mirrors format_cell/3 in status_dashboard.ex; cellRight (layout.go) is the
// right-aligned counterpart and they share truncateCell.
func cell(s string, width int) string {
	return fmt.Sprintf("%-*s", width, truncateCell(sanitize(s), width))
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

// liveTerminalWidth resolves the dashboard output's live terminal width. It is a
// package var so tests can stub the tty query — which can't be forced from a
// non-tty test process — to exercise the terminalColumns precedence.
var liveTerminalWidth = func() (int, bool) { return terminalWidth(os.Stdout) }

// terminalColumns mirrors terminal_columns/0: query the live terminal width
// first (TIOCGWINSZ, the :io.columns() equivalent), then fall back to the
// COLUMNS-env precedence of terminal_columns_from_env/0.
func terminalColumns() int {
	if cols, ok := liveTerminalWidth(); ok {
		return cols
	}
	return terminalColumnsFromEnv()
}

// terminalColumnsFromEnv mirrors terminal_columns_from_env/0.
// When COLUMNS is absent: Elixir computes fixed_running_width + chrome + event_default.
// When COLUMNS is present but invalid: Elixir falls back to @default_terminal_columns (115).
func terminalColumnsFromEnv() int {
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
		return sanitize(x)
	default:
		return sanitize(fmt.Sprintf("%v", v))
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

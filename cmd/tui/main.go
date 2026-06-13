// cmd/tui renders the orchestrator status dashboard in the terminal.
// It polls /api/v1/state and redraws the screen on each tick, mirroring
// the upstream Elixir SymphonyElixir.StatusDashboard behaviour.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/buildinfo"
	"github.com/xrf9268-hue/aiops-platform/internal/stateapi"
)

// version is the build stamp, set at link time via
// -ldflags "-X main.version=<tag>" (release.yml, install.sh). It stays "devel"
// for an un-stamped build; resolveVersion then falls back to the VCS revision
// the Go toolchain records when built from a source tree (#796).
var version = "devel"

func resolveVersion() string { return buildinfo.Resolve(version) }

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

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	urlFlag := flag.String("url", "http://127.0.0.1:4000/", "worker HTTP API base URL")
	intervalFlag := flag.Duration("interval", 5*time.Second, "poll interval")
	rawFlag := flag.Bool("raw", false, "disable alt-screen/cursor management (upstream parity mode)")
	versionFlag := flag.Bool("version", false, "print the build version and exit")
	flag.Parse()
	if *versionFlag {
		fmt.Println(resolveVersion())
		return
	}

	baseURL := strings.TrimSuffix(*urlFlag, "/")
	// Allow env override when using the default flag value.
	if *urlFlag == "http://127.0.0.1:4000/" {
		if env := os.Getenv("AIOPS_DASHBOARD_URL"); env != "" {
			baseURL = strings.TrimSuffix(env, "/")
		}
	}
	stateURL := baseURL + "/api/v1/state"
	authToken := stateAPIAuthTokenFromEnv()
	client := &http.Client{Timeout: 10 * time.Second}

	interval := *intervalFlag
	if interval <= 0 {
		fmt.Fprintln(os.Stderr, "--interval must be a positive duration (e.g. 5s)")
		os.Exit(1)
	}

	scr := newScreen(os.Stdout, isTerminal(os.Stdout), *rawFlag)

	// A single signal subscription drives both shutdown and the exit status.
	// Using one subscription (rather than signal.NotifyContext plus a second
	// signal.Notify) avoids a startup race: two independent subscriptions can't
	// be installed atomically, so a signal landing between them could cancel ctx
	// without the value reaching the exit-status channel, hanging shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	// The first signal cancels ctx — breaking the render loop and aborting any
	// in-flight fetch (the per-fetch context derives from ctx), so Ctrl-C
	// restores the terminal immediately even mid-poll. The signal value is
	// handed to exitSig (buffered, before the cancel) so it can be re-raised for
	// a conventional exit status.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exitSig := make(chan os.Signal, 1)
	go func() {
		// Channel/cancel only — no panic surface, no external I/O.
		select {
		case s := <-sigCh:
			exitSig <- s
			cancel()
		case <-ctx.Done():
		}
	}()

	fetch := func(ctx context.Context) (*stateapi.StateResponse, error) {
		return fetchStateWithAuth(ctx, client, stateURL, authToken)
	}

	run(ctx, scr, fetch, interval, baseURL)

	// run has already restored the terminal (its deferred restore). On signal
	// shutdown exitSig holds the value (sent before the cancel that unblocks
	// run); re-raise it so we exit with the conventional 128+signum status (130
	// for SIGINT). The non-blocking read guarantees we never hang here.
	select {
	case s := <-exitSig:
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
func run(ctx context.Context, scr *screen, fetch func(context.Context) (*stateapi.StateResponse, error), interval time.Duration, baseURL string) { //nolint:gocognit // baseline (#521)
	scr.enter()
	defer scr.restore()

	var samples []tokenSample
	var lastTPS float64
	var lastTPSSec int64

	// Last rendered snapshot, reused by redraw() so a resize reflows the table
	// at the new width without issuing another fetch.
	var (
		haveSnapshot bool
		lastState    *stateapi.StateResponse
		lastErr      error
		lastNow      time.Time
	)

	poll := func() {
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

		lastState, lastErr, lastNow, haveSnapshot = state, fetchErr, now, true
		scr.draw(safeRenderFrame(state, fetchErr, now, lastTPS, baseURL, interval))
	}

	// redraw re-renders the last snapshot at the current terminal width (the
	// width is re-evaluated inside renderFrame via terminalColumns), so a live
	// SIGWINCH resize reflows immediately instead of waiting for the next tick.
	redraw := func() {
		if !haveSnapshot {
			return
		}
		scr.draw(safeRenderFrame(lastState, lastErr, lastNow, lastTPS, baseURL, interval))
	}

	poll()
	if ctx.Err() != nil {
		return
	}

	// Reflow on terminal resize (SIGWINCH on unix; a no-op where unavailable).
	winch := make(chan os.Signal, 1)
	stopResize := notifyResize(winch)
	defer stopResize()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			poll()
		case <-winch:
			redraw()
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
		_, _ = io.WriteString(s.w, ansiAltScreenEnter+ansiHideCursor)
	}
}

// restore reverses enter: show cursor, reset attributes, leave alt-screen.
func (s *screen) restore() {
	if s.lifecycle {
		_, _ = io.WriteString(s.w, ansiShowCursor+ansiReset+ansiAltScreenLeave)
	}
}

// draw writes a single rendered frame. On a TTY each frame is preceded by the
// upstream clear sequence; on a non-TTY the clear is dropped so the output is
// plain, appendable text.
func (s *screen) draw(content string) {
	if s.isTTY {
		_, _ = io.WriteString(s.w, ansiClear+content+"\n")
	} else {
		_, _ = io.WriteString(s.w, content+"\n")
	}
}

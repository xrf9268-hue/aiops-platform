package main

// statehttp_server.go holds the SPEC §13.7 state HTTP server's lifecycle:
// the readiness gate, the hot-reloadable server controller and its run loop,
// server construction, the loopback/auth access guard, and the
// liveness/readiness probes. The JSON response shapes and view->API mappers
// live in stateapi.go; process wiring lives in main.go.

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/orchestrator"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// desiredPortForLoop selects the effective state-server port for one
// tick of runStateHTTPServerLoop. CLI override wins per SPEC §13.7;
// otherwise the workflow snapshot's `server.port` is used; absent a
// snapshot, the loop stays disabled (-1).
func desiredPortForLoop(opts stateHTTPServerLoopOptions, wf *workflow.Workflow) int {
	if opts.PortOverride != nil {
		return *opts.PortOverride
	}
	if wf != nil {
		return wf.Config.Server.Port
	}
	return -1
}

// desiredHostForLoop selects the effective state-server bind host for one tick.
// The AIOPS_SERVER_HOST env override wins, then the workflow snapshot's
// `server.host`, then the loopback default. The empty string is returned as-is
// and normalized to 127.0.0.1 at bind time (normalizeServerHost).
func desiredHostForLoop(opts stateHTTPServerLoopOptions, wf *workflow.Workflow) string {
	if opts.HostOverride != nil {
		return *opts.HostOverride
	}
	if wf != nil {
		return wf.Config.Server.Host
	}
	return ""
}

type stateHTTPReadiness struct {
	ready atomic.Bool
}

func (r *stateHTTPReadiness) MarkReady() {
	r.ready.Store(true)
}
func (r *stateHTTPReadiness) Status() (bool, string) {
	if r.ready.Load() {
		return true, ""
	}
	return false, "startup reconciliation has not completed"
}

type stateHTTPServerController struct {
	snapshot  stateSnapshotFunc
	refresh   stateRefreshFunc
	readiness stateReadinessFunc

	desiredSet  bool
	desiredHost string
	desiredPort int
	cancel      context.CancelFunc
	addr        net.Addr
	serverDone  <-chan struct{}
}

func newStateHTTPServerController(snapshot stateSnapshotFunc, refresh ...stateRefreshFunc) *stateHTTPServerController {
	return &stateHTTPServerController{snapshot: snapshot, refresh: optionalStateRefreshFunc(refresh), readiness: stateHTTPAlwaysReady}
}
func (c *stateHTTPServerController) apply(ctx context.Context, host string, port int) {
	c.refreshStopped()
	if c.desiredSet && c.desiredHost == host && c.desiredPort == port {
		return
	}
	c.stop()
	if port < 0 {
		c.desiredSet = true
		c.desiredHost = host
		c.desiredPort = port
		log.Printf("state HTTP server disabled by server.port=%d", port)
		return
	}
	serverCtx, cancel := context.WithCancel(ctx)
	handle := startStateHTTPServer(serverCtx, host, port, c.snapshot, c.readiness, c.refresh)
	if handle == nil {
		cancel()
		return
	}
	c.desiredSet = true
	c.desiredHost = host
	c.desiredPort = port
	c.cancel = cancel
	c.addr = handle.Addr
	c.serverDone = handle.Done
}
func (c *stateHTTPServerController) refreshStopped() {
	if c.serverDone == nil {
		return
	}
	select {
	case <-c.serverDone:
		if c.cancel != nil {
			c.cancel()
		}
		c.clear()
	default:
	}
}
func (c *stateHTTPServerController) stop() {
	if c.cancel != nil {
		c.cancel()
	}
	if c.serverDone != nil {
		select {
		case <-c.serverDone:
		case <-time.After(stateHTTPServerShutdownTimeout):
			log.Printf("state HTTP server did not stop within %s", stateHTTPServerShutdownTimeout)
		}
	}
	c.clear()
}
func (c *stateHTTPServerController) clear() {
	c.desiredSet = false
	c.desiredHost = ""
	c.desiredPort = 0
	c.cancel = nil
	c.addr = nil
	c.serverDone = nil
}

type stateHTTPServerLoopOptions struct {
	Sleep           func(context.Context, time.Duration) error
	StopAfterChecks int
	Refresh         stateRefreshFunc
	Readiness       stateReadinessFunc
	// PortOverride, when non-nil, replaces the workflow snapshot's
	// `server.port` for every tick of the loop. SPEC §13.7 defines this
	// as the CLI `--port` precedence rule. -1 disables the HTTP server,
	// 0 binds an ephemeral port, 1..65535 binds explicitly.
	PortOverride *int
	// HostOverride, when non-nil, replaces the workflow snapshot's
	// `server.host` for every tick of the loop. It carries the
	// AIOPS_SERVER_HOST env override, mirroring PortOverride's role for the
	// CLI --port flag. An empty string normalizes to the loopback default.
	HostOverride *string
}

func runStateHTTPServerLoop(ctx context.Context, runtime *orchestrator.WorkflowRuntime, snapshot stateSnapshotFunc, opts stateHTTPServerLoopOptions) error {
	if runtime == nil {
		return errors.New("state HTTP server loop requires workflow runtime")
	}
	controller := newStateHTTPServerController(snapshot, opts.Refresh)
	if opts.Readiness != nil {
		controller.readiness = opts.Readiness
	}
	defer controller.stop()
	sleep := opts.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	interval := runtime.ReloadInterval()
	if interval <= 0 {
		interval = time.Second
	}
	checks := 0
	for {
		var wf *workflow.Workflow
		if snap := runtime.Current(); snap.Workflow != nil {
			wf = snap.Workflow
		}
		host := desiredHostForLoop(opts, wf)
		port := desiredPortForLoop(opts, wf)
		controller.apply(ctx, host, port)
		checks++
		if opts.StopAfterChecks > 0 && checks >= opts.StopAfterChecks {
			return nil
		}
		if err := sleep(ctx, interval); err != nil {
			return err
		}
	}
}
func sleepContext(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

const stateHTTPServerShutdownTimeout = 10 * time.Second
const stateHTTPAuthTokenEnv = "AIOPS_STATE_API_TOKEN"

type stateHTTPServerHandle struct {
	Addr net.Addr
	Done <-chan struct{}
}

func startStateHTTPServer(ctx context.Context, host string, port int, snapshot stateSnapshotFunc, readiness stateReadinessFunc, refresh ...stateRefreshFunc) *stateHTTPServerHandle {
	if port < 0 {
		log.Printf("state HTTP server disabled by server.port=%d", port)
		return nil
	}
	server := newStateHTTPServerWithAuthTokenAndReadiness(host, port, os.Getenv(stateHTTPAuthTokenEnv), readiness, snapshot, refresh...)
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		log.Printf("state HTTP server disabled because listen on %s failed: %v", server.Addr, err)
		return nil
	}
	log.Printf("state HTTP server listening on http://%s", listener.Addr().String())
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("state HTTP server shutdown error: %v", err)
			}
		case <-done:
			return
		}
	}()
	go func() {
		defer close(done)
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			if ctx.Err() == nil {
				log.Printf("state HTTP server exited: %v", err)
			}
		}
	}()
	return &stateHTTPServerHandle{Addr: listener.Addr(), Done: done}
}

// normalizeServerHost maps an empty bind host to the loopback default so a
// blank server.host / AIOPS_SERVER_HOST never resolves to net.Listen's
// bind-all wildcard (SPEC §15.3 loopback-only trust boundary).
func normalizeServerHost(host string) string {
	if host == "" {
		return "127.0.0.1"
	}
	return host
}
func newStateHTTPServer(host string, port int, snapshot stateSnapshotFunc, refresh ...stateRefreshFunc) *http.Server {
	return newStateHTTPServerWithAuthToken(host, port, "", snapshot, refresh...)
}
func newStateHTTPServerWithAuthToken(host string, port int, authToken string, snapshot stateSnapshotFunc, refresh ...stateRefreshFunc) *http.Server {
	return newStateHTTPServerWithAuthTokenAndReadiness(host, port, authToken, stateHTTPAlwaysReady, snapshot, refresh...)
}
func newStateHTTPServerWithAuthTokenAndReadiness(host string, port int, authToken string, readiness stateReadinessFunc, snapshot stateSnapshotFunc, refresh ...stateRefreshFunc) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/api/v1/state", stateHTTPHandler(snapshot))
	mux.Handle("/api/v1/refresh", refreshHTTPHandler(optionalStateRefreshFunc(refresh)))
	mux.Handle("/api/v1/", issueHTTPHandler(snapshot))
	mux.Handle("/assets/", dashboardAssetHandler())
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		dashboardHTMLHandler().ServeHTTP(w, r)
	})
	guard := stateHTTPAccessGuard{authToken: strings.TrimSpace(authToken)}
	root := http.NewServeMux()
	root.Handle("/livez", livenessHTTPHandler())
	root.Handle("/readyz", readinessHTTPHandler(readiness))
	root.Handle("/", guard.wrap(mux))
	return &http.Server{
		Addr:              net.JoinHostPort(normalizeServerHost(host), strconv.Itoa(port)),
		Handler:           root,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		// Cap header size well below Go's 1 MiB default. The state and
		// refresh endpoints only exchange ~kilobytes of cookies and
		// headers; an unbounded header read would let a misbehaving
		// or hostile client wedge a connection until ReadHeaderTimeout
		// fires. Go internally adds a 4 KiB bufio slop on top of this
		// value when computing the actual reject threshold.
		MaxHeaderBytes: 64 << 10,
	}
}

type stateReadinessFunc func() (bool, string)

func stateHTTPAlwaysReady() (bool, string) {
	return true, ""
}
func livenessHTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w, "ok\n"); err != nil {
			log.Printf("write /livez response: %v", err)
		}
	})
}
func readinessHTTPHandler(readiness stateReadinessFunc) http.Handler {
	if readiness == nil {
		readiness = stateHTTPAlwaysReady
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ready, reason := readiness()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
			if reason == "" {
				reason = "not ready"
			}
			if _, err := io.WriteString(w, reason+"\n"); err != nil {
				log.Printf("write /readyz unavailable response: %v", err)
			}
			return
		}
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w, "ok\n"); err != nil {
			log.Printf("write /readyz response: %v", err)
		}
	})
}

type stateHTTPAccessGuard struct {
	authToken string
}

func (g stateHTTPAccessGuard) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.authToken != "" {
			if stateHTTPAuthorized(r, g.authToken) {
				next.ServeHTTP(w, r)
				return
			}
			requireStateHTTPAuth(w)
			return
		}
		if isLoopbackHTTPHost(r.Host) && isLoopbackHTTPRemoteAddr(r.RemoteAddr) {
			next.ServeHTTP(w, r)
			return
		}
		if !isLoopbackHTTPHost(r.Host) {
			http.Error(w, "misdirected request", http.StatusMisdirectedRequest)
			return
		}
		http.Error(w, "forbidden", http.StatusForbidden)
	})
}
func requireStateHTTPAuth(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="aiops state API", charset="UTF-8"`)
	http.Error(w, "authentication required", http.StatusUnauthorized)
}
func stateHTTPAuthorized(r *http.Request, token string) bool {
	if user, password, ok := r.BasicAuth(); ok && user == "aiops" && stateHTTPTokenMatches(password, token) {
		return true
	}
	scheme, credentials, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if ok && strings.EqualFold(scheme, "Bearer") && stateHTTPTokenMatches(strings.TrimSpace(credentials), token) {
		return true
	}
	return false
}
func stateHTTPTokenMatches(got, want string) bool {
	if got == "" || want == "" || len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
func isLoopbackHTTPHost(hostport string) bool {
	if hostport == "" {
		return false
	}
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	} else if strings.Contains(hostport, ":") && !strings.HasPrefix(hostport, "[") {
		// "host:port" without IPv6 brackets that failed to split — malformed.
		return false
	}
	// Strip IPv6 brackets only when the host is properly bracketed (e.g. "[::1]").
	// Reject unpaired brackets — "[::1" or "::1]" are malformed Host values.
	if strings.HasPrefix(host, "[") || strings.HasSuffix(host, "]") {
		if !strings.HasPrefix(host, "[") || !strings.HasSuffix(host, "]") {
			return false
		}
		host = host[1 : len(host)-1]
	}
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}
func isLoopbackHTTPRemoteAddr(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type stateSnapshotFunc func(context.Context) (orchestrator.StateView, error)
type stateRefreshFunc func(context.Context) (orchestrator.RefreshRequestResult, error)

func optionalStateRefreshFunc(refresh []stateRefreshFunc) stateRefreshFunc {
	if len(refresh) == 0 {
		return nil
	}
	return refresh[0]
}

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/queue"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/triggerapi"
	"github.com/xrf9268-hue/aiops-platform/internal/worker"
)

type testbed struct {
	pg            *pgEnv
	gitea         *giteaEnv
	triggerSrv    *httptest.Server
	secret        string
	webhookURL    string
	cloneRewriter *cloneRewriter
	cancel        context.CancelFunc
	wg            *sync.WaitGroup
}

func setupTestbed(ctx context.Context) (*testbed, error) {
	pg, err := startPostgres(ctx)
	if err != nil {
		return nil, err
	}

	g, err := startGitea(ctx)
	if err != nil {
		pg.close(context.Background())
		return nil, err
	}

	secret := randomHex(16)

	// Wrap the queue store so Gitea's clone_url (which embeds the container-
	// internal hostname) is rewritten to the host-mapped URL the worker can
	// actually clone from. We also embed the bot token in the URL so the
	// worker can push without a credential helper (the token is the same one
	// used for the Gitea API; Gitea accepts it as an HTTP password).
	rewriter := &cloneRewriter{
		store:    queue.New(pg.pool),
		fromHost: "localhost:3000",
		toHost:   strings.TrimPrefix(g.baseURL, "http://"),
		botUser:  g.botUser,
		botToken: g.botToken,
	}

	// Listen on tcp4 so triggerSrv.URL is always 127.0.0.1.
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		g.close(context.Background())
		pg.close(context.Background())
		return nil, err
	}
	srv := triggerapi.NewServer(rewriter, secret)
	triggerSrv := &httptest.Server{
		Listener: listener,
		Config:   &http.Server{Handler: triggerapi.Routes(srv)},
	}
	triggerSrv.Start()
	if !strings.Contains(triggerSrv.URL, "127.0.0.1") {
		triggerSrv.Close()
		g.close(context.Background())
		pg.close(context.Background())
		return nil, fmt.Errorf("unexpected httptest URL: %s", triggerSrv.URL)
	}

	webhookURL := strings.Replace(triggerSrv.URL, "127.0.0.1", "host.docker.internal", 1) + "/v1/events/gitea"

	cfg := worker.Config{
		WorkspaceRoot:   tmpDir(),
		MirrorRoot:      tmpDir(),
		GiteaBaseURL:    g.baseURL,
		GiteaToken:      g.botToken,
		IdleSleep:       200 * time.Millisecond,
		ClaimErrorSleep: 200 * time.Millisecond,
	}

	wctx, cancel := context.WithCancel(ctx)
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		worker.Run(wctx, queue.New(pg.pool), cfg)
	}()

	return &testbed{
		pg:            pg,
		gitea:         g,
		triggerSrv:    triggerSrv,
		secret:        secret,
		webhookURL:    webhookURL,
		cloneRewriter: rewriter,
		cancel:        cancel,
		wg:            wg,
	}, nil
}

func (b *testbed) close(ctx context.Context) {
	b.cancel()
	stopped := make(chan struct{})
	go func() { b.wg.Wait(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		// best effort; report via stderr (no t available in close path)
	}
	b.triggerSrv.Close()
	b.gitea.close(ctx)
	b.pg.close(ctx)
}

// resetState deletes only rows produced after testStart, leaving rows from
// earlier tests (which their own cleanup should have handled) untouched.
// Uses DELETE rather than TRUNCATE to avoid ACCESS EXCLUSIVE deadlocks
// with the worker's claim transactions.
func (b *testbed) resetState(t *testing.T, testStart time.Time) {
	ctx := context.Background()
	if _, err := b.pg.pool.Exec(ctx,
		`DELETE FROM task_events WHERE task_id IN (SELECT id FROM tasks WHERE created_at >= $1)`,
		testStart); err != nil {
		t.Fatalf("reset task_events: %v", err)
	}
	if _, err := b.pg.pool.Exec(ctx,
		`DELETE FROM tasks WHERE created_at >= $1`, testStart); err != nil {
		t.Fatalf("reset tasks: %v", err)
	}
}

// cloneRewriter wraps a queue.Store. On Enqueue, it rewrites the clone URL
// so the worker can reach Gitea via the host-mapped HTTP port instead of the
// container's internal hostname. It also:
//   - Converts SSH clone URLs (which Gitea includes even when SSH is disabled)
//     to HTTP, since SSH is not reachable in the testbed environment.
//   - Embeds the bot credentials in the HTTP URL so the worker can push
//     without a credential helper.
type cloneRewriter struct {
	store    *queue.Store
	fromHost string
	toHost   string
	botUser  string
	botToken string
}

func (r *cloneRewriter) Enqueue(ctx context.Context, t task.Task) (task.Task, bool, error) {
	if t.CloneURL != "" {
		var repoPath string
		// Convert SSH URL (ssh://git@host:port/owner/repo.git or
		// git@host:owner/repo.git) to HTTP using toHost.
		if strings.HasPrefix(t.CloneURL, "ssh://") {
			trimmed := strings.TrimPrefix(t.CloneURL, "ssh://")
			if slash := strings.Index(trimmed, "/"); slash >= 0 {
				repoPath = trimmed[slash:] // /owner/repo.git
			}
		} else if strings.HasPrefix(t.CloneURL, "git@") {
			if colon := strings.Index(t.CloneURL, ":"); colon >= 0 {
				repoPath = "/" + t.CloneURL[colon+1:] // /owner/repo.git
			}
		}
		if repoPath != "" {
			// Build an authenticated HTTP URL for the rewritten host.
			t.CloneURL = "http://" + r.botUser + ":" + r.botToken + "@" + r.toHost + repoPath
		} else {
			// HTTP URL: rewrite the internal container host to the mapped host,
			// injecting credentials.
			host := r.toHost
			if r.botUser != "" && r.botToken != "" {
				host = r.botUser + ":" + r.botToken + "@" + host
			}
			t.CloneURL = strings.Replace(t.CloneURL, r.fromHost, host, 1)
		}
	}
	return r.store.Enqueue(ctx, t)
}

func (r *cloneRewriter) GetTask(ctx context.Context, id string) (task.Task, error) {
	return r.store.GetTask(ctx, id)
}

func (r *cloneRewriter) ListTasks(ctx context.Context, st task.Status) ([]task.Task, error) {
	return r.store.ListTasks(ctx, st)
}

func (r *cloneRewriter) TaskEvents(ctx context.Context, id string) ([]task.Event, error) {
	return r.store.TaskEvents(ctx, id)
}

func tmpDir() string {
	d, err := os.MkdirTemp("", "aiops-e2e-*")
	if err != nil {
		panic(err)
	}
	return d
}

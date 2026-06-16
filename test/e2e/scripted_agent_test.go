//go:build e2e

package e2e

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/gitea"
	"github.com/xrf9268-hue/aiops-platform/internal/orchestrator"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/worker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// scriptedAgentTemplate is the shell script the test installs as the "coding
// agent" (via `agent.default: claude` + `claude.command`). It performs the
// agent-side half of the SPEC §1 loop from inside the workspace the worker
// prepared: commit a change, push a branch, open a PR via the Gitea API, then
// flip the issue's aiops/* state label. Credentials and coordinates must be
// baked into the file by the test because the worker's agent env passthrough
// deny list keeps GITEA_TOKEN out of the agent environment; the script's
// first step asserts that absence, so a passthrough regression that leaked
// the tracker token to agents fails this test. The fixture deliberately does
// NOT cover the production agent's dynamic tool surface (gitea_issue_labels
// needs a real coding agent): per issue #782 it plays the agent "via the
// Gitea API" so the WORKER-side loop is exercised purely test-side. `set -eu`
// makes any failed step surface as a runner error, which fails the
// taskSucceeded wait.
const scriptedAgentTemplate = `#!/bin/sh
set -eu
if [ -n "${GITEA_TOKEN:-}" ]; then
  echo 'GITEA_TOKEN leaked into the agent environment' >&2
  exit 1
fi
# Pin the label set BEFORE this agent touches it: in the claim->agent-start
# window the worker must not have written tracker state, so the only aiops/*
# label here is the seeded aiops/todo. This covers exactly that window — the
# later replace-all PUT would mask a write landing between this snapshot and
# the PUT, but in this harness the poller never ticks while the run is in
# flight, so no worker code executes in that gap.
labels=$(curl -sfS --max-time 30 -H 'Authorization: token {{TOKEN}}' \
  '{{BASE_URL}}/api/v1/repos/{{OWNER}}/{{REPO}}/issues/{{ISSUE}}/labels')
aiops_labels=$(printf '%s' "$labels" | grep -o '"name":"aiops/[^"]*"' | sed 's/^"name"://' | sort -u)
if [ "$aiops_labels" != '"aiops/todo"' ]; then
  echo "unexpected aiops/* labels before the agent ran: $aiops_labels" >&2
  exit 1
fi
# The agent pushes its OWN agent/<n> branch instead of the worker-prepared
# ai/<n> work branch on purpose: keeping the two actors' remote footprints
# disjoint is what lets the retained negative assertion attribute any remote
# ai/<n> appearance unambiguously to the worker. Which branch a real agent
# pushes is prompt-level convention (SPEC §1 hands the whole handoff to the
# agent); the worker-side contract under test does not depend on it.
git checkout -b '{{BRANCH}}'
printf 'scripted agent change for issue {{ISSUE}}\n' > AGENT_CHANGE.md
git add AGENT_CHANGE.md
git -c user.name=scripted-agent -c user.email=scripted-agent@example.invalid commit --quiet -m 'scripted agent change'
# Push with the credential in an http.extraHeader instead of URL userinfo:
# on failure git echoes the remote URL to stderr, which the runner streams
# into test logs — the URL must therefore stay credential-free (the same
# mask-before-log class the repo enforces for clone URLs).
git -c http.extraHeader='Authorization: Basic {{AUTH_B64}}' push --quiet '{{PUSH_URL}}' 'HEAD:{{BRANCH}}'
curl -sfS --max-time 30 -X POST \
  -H 'Authorization: token {{TOKEN}}' \
  -H 'Content-Type: application/json' \
  -d '{"title":"WIP: scripted agent PR for issue {{ISSUE}}","head":"{{BRANCH}}","base":"main"}' \
  '{{BASE_URL}}/api/v1/repos/{{OWNER}}/{{REPO}}/pulls' > /dev/null
# PUT /issues/{index}/labels replaces all labels; Gitea accepts label names
# here (not only IDs) since 1.20, and the suite pins gitea/gitea:1.26.1 — the
# same name-based contract giteaEnv.replaceIssueLabels relies on.
curl -sfS --max-time 30 -X PUT \
  -H 'Authorization: token {{TOKEN}}' \
  -H 'Content-Type: application/json' \
  -d '{"labels":["aiops/done"]}' \
  '{{BASE_URL}}/api/v1/repos/{{OWNER}}/{{REPO}}/issues/{{ISSUE}}/labels' > /dev/null
`

// TestGiteaScriptedAgentLoop_PositiveHandoff covers the positive half of the
// issue→PR loop that TestGiteaMockLoop_HappyPath only asserts negatively: a
// scripted agent really commits, pushes a branch, opens a PR, and flips the
// aiops/* label from inside the workspace, while the worker observes the
// handoff, reconciles the now-terminal issue, and cleans the workspace —
// without ever writing tracker state or PRs itself (#782).
func TestGiteaScriptedAgentLoop_PositiveHandoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	owner, repo := bed.gitea.botUser, "demo-scripted-agent"
	cloneURL, err := bed.gitea.createRepo(ctx, repo)
	if err != nil {
		t.Fatalf("createRepo: %v", err)
	}
	if err := bed.gitea.putWorkflowFile(ctx, owner, repo, fixtureContent(t, "scripted-agent.md"), "seed workflow"); err != nil {
		t.Fatalf("putWorkflowFile: %v", err)
	}
	issueNum, err := bed.gitea.createIssue(ctx, owner, repo, "scripted handoff", "agent pushes a branch, opens a PR, flips the label")
	if err != nil {
		t.Fatalf("createIssue: %v", err)
	}
	if err := bed.gitea.ensureLabels(ctx, owner, repo, []string{"aiops/todo", "aiops/done"}); err != nil {
		t.Fatalf("ensure labels: %v", err)
	}
	if err := bed.gitea.addIssueLabels(ctx, owner, repo, issueNum, []string{"aiops/todo"}); err != nil {
		t.Fatalf("add todo label: %v", err)
	}

	agentBranch := fmt.Sprintf("agent/%d", issueNum)
	scriptPath := writeScriptedAgent(t, owner, repo, agentBranch, issueNum)

	// Put the tracker credential into the WORKER process environment so the
	// script's GITEA_TOKEN-absence guard pins something real: the runner
	// builds the agent env from an allowlist (internal/runner/env.go), so a
	// regression that inherits the worker env wholesale — or adds the token
	// to the baseline allowlist — delivers the variable and fails the run.
	// (Passthrough *requests* for GITEA_TOKEN are already rejected one layer
	// earlier, at workflow load; see validateAgentEnvPassthrough.)
	t.Setenv("GITEA_TOKEN", bed.gitea.botToken)
	t.Setenv("AIOPS_TRACKER_SECRET", bed.gitea.botToken)
	t.Setenv("EXTRA_BUILD_VAR", "let-me-in")
	serviceWorkflow, err := workflow.Load(writeScriptedAgentServiceWorkflow(t, cloneURL, scriptPath))
	if err != nil {
		t.Fatalf("load service workflow: %v", err)
	}
	t.Setenv("AIOPS_TRACKER_SECRET", "rotated-e2e-tracker-secret")

	// The hand-set fields below deliberately mirror the fixture front matter
	// (test/e2e/fixtures/scripted-agent.md) — the suite-wide assembly pattern
	// shared with runGiteaWorkerTask. Tracker states are taken from the
	// loaded workflow so the poller, reconciler, and dispatcher cannot drift
	// from what the fixture declares.
	cfg := workflow.DefaultConfig()
	cfg.Repo.Owner = owner
	cfg.Repo.Name = repo
	cfg.Repo.CloneURL = cloneURL
	cfg.Repo.DefaultBranch = "main"
	cfg.Tracker.Kind = "gitea"
	cfg.Tracker.APIKey = bed.gitea.botToken
	cfg.Tracker.ActiveStates = serviceWorkflow.Config.Tracker.ActiveStates
	cfg.Tracker.TerminalStates = serviceWorkflow.Config.Tracker.TerminalStates
	serviceWorkflow.Config.Tracker.APIKey = bed.gitea.botToken
	client := gitea.NewTrackerClient(cfg.Tracker, bed.gitea.baseURL, owner, repo)
	client.HTTP = httpClientForE2E()

	workspaceRoot := tmpDir()
	events := &e2eEventRecorder{}
	dispatcher := &scriptedAgentDispatcher{workspaceRoot: workspaceRoot}
	dispatcher.WorkerTaskDispatcher = orchestrator.WorkerTaskDispatcher{
		BuildTask: func(issue tracker.Issue) (task.Task, error) {
			tk, err := orchestrator.TaskFromIssue(issue, cfg)
			if err != nil {
				return task.Task{}, err
			}
			events.recordTask(tk)
			return tk, nil
		},
		Config: worker.Config{
			WorkspaceRoot: workspaceRoot,
			MirrorRoot:    tmpDir(),
			Workflow:      serviceWorkflow,
		},
		Emitter:           events,
		WorkspacePrepared: dispatcher.workspacePrepared,
	}
	orch := orchestrator.New(orchestrator.NewOrchestratorState(15000, 1), orchestrator.Deps{
		Dispatcher: dispatcher,
		Scheduler:  orchestrator.RetryScheduler{MaxBackoff: time.Minute},
		// Production wires the RuntimeDispatcher as the WorkspaceCleaner
		// (cmd/worker/main.go); this cleaner delegates to the same shared
		// worker.RemoveIssueWorkspace routine so the SPEC §18.1 terminal
		// cleanup path under test matches the deployed seam.
		WorkspaceCleaner: &scriptedAgentWorkspaceCleaner{
			emitter:        events,
			workspaceRoot:  workspaceRoot,
			workflowConfig: serviceWorkflow.Config,
			hooks:          serviceWorkflow.Config.WorkspaceHooks(),
		},
	})
	orchCtx, orchCancel := context.WithCancel(ctx)
	t.Cleanup(orchCancel)
	go orch.Run(orchCtx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("start orchestrator: %v", err)
	}

	poller := orchestrator.NewPollerWithReconciliation(client, orch, orchestrator.ReconciliationConfig{
		ActiveStates:      cfg.Tracker.ActiveStates,
		TerminalStates:    cfg.Tracker.TerminalStates,
		WorkerExitTimeout: 10 * time.Second,
	})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("initial poll: %v", err)
	}

	var taskID string
	pollUntil(ctx, t, 90*time.Second, 250*time.Millisecond, func(ctx context.Context) (bool, error) {
		tk, ok := events.taskBySource("gitea_issue", fmt.Sprintf("#%d", issueNum))
		if !ok {
			return false, nil
		}
		succeeded, err := events.taskSucceeded(tk.ID)
		if err != nil || !succeeded {
			return false, err
		}
		taskID = tk.ID
		return true, nil
	})
	if taskID == "" {
		t.Fatalf("scripted agent task did not complete")
	}

	workspacePath := dispatcher.preparedWorkspacePath()
	if workspacePath == "" {
		t.Fatalf("workspace path was not recorded by WorkspacePrepared")
	}
	// No race with the §16.6 continuation timer here: a fired continuation is
	// a wake signal only (fireWakeSignal, internal/orchestrator/actor_retry.go)
	// — never a timer-driven cleanup — and workspace prepare reuses the
	// deterministic directory rather than delete/recreate, so the directory
	// cannot vanish before the reconcile PollOnce below runs.
	if info, err := os.Stat(workspacePath); err != nil || !info.IsDir() {
		t.Fatalf("Stat(%q) = %v, %v; want existing workspace dir after the run", workspacePath, info, err)
	}

	assertScriptedAgentHandoff(t, ctx, owner, repo, issueNum, agentBranch, events.task(taskID).WorkBranch)

	// The worker observes the handoff on the next reconciliation poll: the
	// clean exit queued a SPEC §16.6 continuation retry (1s wake) carrying the
	// run's workspace, and the reconcile pass — seeing the issue in a terminal
	// state — releases the claim and routes the directory through the SPEC
	// §18.1 WorkspaceCleaner seam. Both the continuation scheduling and the
	// cleanup itself run on async followups, so poll until they settle instead
	// of asserting after a single tick.
	pollUntil(ctx, t, 60*time.Second, 500*time.Millisecond, func(ctx context.Context) (bool, error) {
		// Treat transient poll/snapshot errors as not-yet-settled: the next
		// tick retries and only the 60s timeout reports failure, so one API
		// hiccup under CI load cannot fail the whole e2e run. The error is
		// logged so a timeout still shows what kept the loop waiting.
		if err := poller.PollOnce(ctx); err != nil {
			t.Logf("reconcile PollOnce: %v (retrying)", err)
			return false, nil
		}
		view, err := orch.Snapshot(ctx)
		if err != nil {
			t.Logf("orchestrator Snapshot: %v (retrying)", err)
			return false, nil
		}
		if len(view.Running) != 0 || len(view.Retrying) != 0 {
			return false, nil
		}
		if _, err := os.Stat(workspacePath); !os.IsNotExist(err) {
			return false, nil
		}
		return true, nil
	})
	beforeRemoveMarker := filepath.Join(filepath.Dir(workspacePath), "before-remove-env")
	body, err := os.ReadFile(beforeRemoveMarker)
	if err != nil {
		t.Fatalf("read before_remove env marker: %v", err)
	}
	if got := string(body); got != "<let-me-in><>" {
		t.Fatalf("before_remove env marker = %q, want tracker secret slot empty", got)
	}

	// Re-check the tracker after reconciliation: the worker must not have
	// rewritten the agent's label flip or opened a PR of its own.
	assertScriptedAgentHandoff(t, ctx, owner, repo, issueNum, agentBranch, events.task(taskID).WorkBranch)
}

// assertScriptedAgentHandoff checks the agent-owned remote state (branch, PR,
// label) and the worker-side negative boundary (no pushed work branch). It
// runs both right after the task succeeds and again after reconciliation, so
// a worker-side tracker/PR write at either stage fails the test.
func assertScriptedAgentHandoff(t *testing.T, ctx context.Context, owner, repo string, issueNum int, agentBranch, workBranch string) {
	t.Helper()

	branchExists, err := bed.gitea.getBranch(ctx, owner, repo, agentBranch)
	if err != nil {
		t.Fatalf("getBranch(%q): %v", agentBranch, err)
	}
	if !branchExists {
		t.Fatalf("getBranch(%q) = false; want agent branch pushed by the scripted agent", agentBranch)
	}

	prs, err := bed.gitea.listOpenPRs(ctx, owner, repo)
	if err != nil {
		t.Fatalf("listOpenPRs: %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("listOpenPRs = %+v; want exactly 1 open PR (the scripted agent's)", prs)
	}
	if prs[0].Head.Ref != agentBranch {
		t.Fatalf("open PR head = %q; want %q", prs[0].Head.Ref, agentBranch)
	}
	// What this pins is the fixture's INTERNAL consistency: the front matter
	// declares policy.mode: draft_pr and the scripted agent honors it with
	// the WIP title prefix Gitea maps to the draft flag. policy.mode is
	// prompt-level convention (the worker-side gate was removed in #561), so
	// no worker enforcement is claimed — editing the script to open a
	// non-draft PR under the declared policy is what goes red here.
	if !prs[0].Draft {
		t.Fatalf("open PR draft = %v (title %q); want a draft PR per the fixture's policy.mode: draft_pr", prs[0].Draft, prs[0].Title)
	}

	labels, err := bed.gitea.getIssueLabels(ctx, owner, repo, issueNum)
	if err != nil {
		t.Fatalf("getIssueLabels: %v", err)
	}
	sort.Strings(labels)
	if len(labels) != 1 || labels[0] != "aiops/done" {
		t.Fatalf("getIssueLabels(#%d) = %v; want exactly [aiops/done] flipped by the agent", issueNum, labels)
	}

	if !regexp.MustCompile(`^ai/[0-9]+$`).MatchString(workBranch) {
		t.Fatalf("unexpected worker task branch %q", workBranch)
	}
	workBranchExists, err := bed.gitea.getBranch(ctx, owner, repo, workBranch)
	if err != nil {
		t.Fatalf("getBranch(%q): %v", workBranch, err)
	}
	if workBranchExists {
		t.Fatalf("worker must not push work branch %q", workBranch)
	}
}

// writeScriptedAgent renders scriptedAgentTemplate with the live test-bed
// coordinates baked in and installs it as an executable script.
func writeScriptedAgent(t *testing.T, owner, repo, agentBranch string, issueNum int) string {
	t.Helper()
	pushURL := bed.gitea.baseURL + "/" + owner + "/" + repo + ".git"
	authB64 := base64.StdEncoding.EncodeToString([]byte(bed.gitea.botUser + ":" + bed.gitea.botToken))
	script := strings.NewReplacer(
		"{{BRANCH}}", agentBranch,
		"{{ISSUE}}", fmt.Sprintf("%d", issueNum),
		"{{PUSH_URL}}", pushURL,
		"{{AUTH_B64}}", authB64,
		"{{TOKEN}}", bed.gitea.botToken,
		"{{BASE_URL}}", bed.gitea.baseURL,
		"{{OWNER}}", owner,
		"{{REPO}}", repo,
	).Replace(scriptedAgentTemplate)
	path := filepath.Join(t.TempDir(), "scripted-agent.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write scripted agent: %v", err)
	}
	return path
}

// writeScriptedAgentServiceWorkflow rewrites the fixture's placeholders (the
// validator-only clone_url and the runtime-generated agent script path) and
// writes the result where workflow.Load can read it, mirroring
// writeE2EServiceWorkflow.
func writeScriptedAgentServiceWorkflow(t *testing.T, cloneURL, agentCommand string) string {
	t.Helper()
	body := string(fixtureContent(t, "scripted-agent.md"))
	// Fail loud when the fixture and these literals drift: a silent no-op
	// replacement would surface much later as an opaque clone/exec timeout.
	for _, placeholder := range []string{
		"http://localhost:3000/aiops-bot/demo-scripted-agent.git",
		"__SCRIPTED_AGENT_COMMAND__",
	} {
		if !strings.Contains(body, placeholder) {
			t.Fatalf("fixture scripted-agent.md does not contain placeholder %q; update writeScriptedAgentServiceWorkflow alongside the fixture", placeholder)
		}
	}
	body = strings.ReplaceAll(body, "http://localhost:3000/aiops-bot/demo-scripted-agent.git", cloneURL)
	body = strings.ReplaceAll(body, "__SCRIPTED_AGENT_COMMAND__", agentCommand)
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write service workflow: %v", err)
	}
	return path
}

// scriptedAgentDispatcher wraps WorkerTaskDispatcher to mirror the production
// RuntimeDispatcher wiring this test depends on: AttachOrchestrator (called by
// orchestrator.New) plus a WorkspacePrepared hook that records the workspace
// path + root on the running entry so terminal reconciliation can clean it.
type scriptedAgentDispatcher struct {
	orchestrator.WorkerTaskDispatcher

	workspaceRoot string

	mu            sync.Mutex
	orch          *orchestrator.Orchestrator
	workspacePath string
}

func (d *scriptedAgentDispatcher) AttachOrchestrator(o *orchestrator.Orchestrator) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.orch = o
}

func (d *scriptedAgentDispatcher) workspacePrepared(ctx context.Context, issue tracker.Issue, _ task.Task, path string) {
	d.mu.Lock()
	d.workspacePath = path
	orch := d.orch
	d.mu.Unlock()
	if orch == nil {
		return
	}
	// Mirror RuntimeDispatcher.Spawn: capture the root the path was created
	// under so SPEC §18.1 cleanup removes it against the same root.
	_ = orch.RecordWorkspace(ctx, issue.ID, orchestrator.Workspace{Path: path, Root: d.workspaceRoot})
}

func (d *scriptedAgentDispatcher) preparedWorkspacePath() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.workspacePath
}

// scriptedAgentWorkspaceCleaner implements orchestrator.WorkspaceCleaner the
// way production does (RuntimeDispatcher.CleanupReconciledWorkspace): it
// delegates to worker.RemoveIssueWorkspace, the same routine the startup
// sweep uses, so before_remove and the reconcile_workspace event contract
// match the deployed worker.
type scriptedAgentWorkspaceCleaner struct {
	emitter        worker.EventEmitter
	workspaceRoot  string
	workflowConfig workflow.Config
	hooks          workflow.WorkspaceHooks
}

func (c *scriptedAgentWorkspaceCleaner) CleanupReconciledWorkspace(ctx context.Context, w orchestrator.ReconciledWorkspace) {
	root := strings.TrimSpace(w.Root)
	if root == "" {
		root = c.workspaceRoot
	}
	if _, err := worker.RemoveIssueWorkspace(ctx, c.emitter, worker.RemoveWorkspaceRequest{
		WorkspaceRoot:      root,
		TaskID:             "reconcile-active",
		Path:               w.Path,
		IssueID:            string(w.IssueID),
		Identifier:         w.Identifier,
		State:              w.State,
		Reason:             w.Reason,
		WorkflowConfig:     c.workflowConfig,
		BeforeRemoveHook:   c.hooks.BeforeRemove,
		HookTimeoutMillis:  c.hooks.TimeoutMs,
		HookEnvPassthrough: c.hooks.EnvPassthrough,
	}); err != nil {
		log.Printf("e2e scripted-agent workspace cleanup failed: workspace=%q error=%q", w.Path, err)
	}
}

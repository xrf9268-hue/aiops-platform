package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func appServerInput(workdir string) RunInput {
	return RunInput{
		Task: task.Task{ID: "tsk-001", SourceEventID: "AIOPS-64", Title: "Wire Codex app-server", Model: "codex-app-server"},
		Workflow: workflow.Workflow{Config: workflow.Config{
			Agent:     workflow.AgentConfig{MaxTurns: 8},
			Workspace: workflow.WorkspaceConfig{Root: filepath.Dir(workdir)},
			Codex: workflow.CommandConfig{
				Command:        "codex app-server",
				ApprovalPolicy: "never",
				ThreadSandbox:  "workspace-write",
				TurnSandboxPolicy: map[string]any{
					"mode": "workspace-write",
				},
				EnvPassthrough: []string{
					"CODEX_ARGV_LOG",
					"CODEX_STDIN_LOG",
					"CODEX_STARTED_LOG",
				},
				TurnTimeoutMs:  3000,
				ReadTimeoutMs:  3000,
				StallTimeoutMs: 3000,
			},
		}},
		Workdir: workdir,
		Prompt:  "ignored — runner reads .aiops/PROMPT.md",
	}
}

func codexAppServerStubScript(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "codex")
	header := `#!/usr/bin/env python3
import os, sys
with open(os.environ['CODEX_ARGV_LOG'], 'w') as f:
    f.write('\n'.join(sys.argv[1:]))
    f.write('\n')
`
	if err := os.WriteFile(scriptPath, []byte(header+body), 0o755); err != nil {
		t.Fatal(err)
	}
	path := dir + string(os.PathListSeparator) + os.Getenv("PATH")
	t.Setenv("PATH", path)
	useAgentLoginPATH(t, path)
	t.Setenv("CODEX_ARGV_LOG", filepath.Join(dir, "argv.txt"))
	t.Setenv("CODEX_STDIN_LOG", filepath.Join(dir, "stdin.txt"))
	return dir
}

func codexAppServerEnvStubScript(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "codex")
	header := `#!/usr/bin/env python3
import os, sys
with open("env.txt", "w") as f:
    for name in ("LINEAR_API_KEY", "GITEA_TOKEN", "GITHUB_TOKEN", "PATH"):
        if name in os.environ:
            f.write(f"{name}={os.environ[name]}\n")
`
	if err := os.WriteFile(scriptPath, []byte(header+body), 0o755); err != nil {
		t.Fatal(err)
	}
	path := dir + string(os.PathListSeparator) + os.Getenv("PATH")
	t.Setenv("PATH", path)
	useAgentLoginPATH(t, path)
	return dir
}

func TestNewSupportsCodexAppServerRunner(t *testing.T) {
	r, err := New("codex-app-server")
	if err != nil {
		t.Fatalf("New(codex-app-server): %v", err)
	}
	if _, ok := r.(CodexAppServerRunner); !ok {
		t.Fatalf("New(codex-app-server) = %T, want CodexAppServerRunner", r)
	}
}

func TestCodexAppServerRunnerRejectsOutsideWorkdirWhenSandboxDisabled(t *testing.T) {
	binDir := codexAppServerStubScript(t, `
with open(os.environ['CODEX_STARTED_LOG'], 'w') as f:
    f.write('started')
sys.exit(0)
`)
	t.Setenv("CODEX_STARTED_LOG", filepath.Join(binDir, "started.txt"))

	root := t.TempDir()
	wd := codexWorkdir(t, "x")
	in := appServerInput(wd)
	in.Workflow.Config.Workspace.Root = root

	_, err := (CodexAppServerRunner{}).Run(context.Background(), in)
	if err == nil {
		t.Fatal("expected invalid workspace cwd error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid_workspace_cwd") {
		t.Fatalf("error = %q, want invalid_workspace_cwd", err)
	}
	if _, statErr := os.Stat(filepath.Join(binDir, "started.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("codex app-server was launched before workspace validation; stat err=%v", statErr)
	}
}

func TestCodexAppServerRunnerSendsInitializeThreadAndTurn(t *testing.T) {
	binDir := codexAppServerStubScript(t, `
import json
log=open(os.environ['CODEX_STDIN_LOG'], 'w')
for line in sys.stdin:
    log.write(line); log.flush()
    msg=json.loads(line)
    method=msg.get('method')
    if method == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {'protocolVersion': '0.1.0'}}), flush=True)
    elif method == 'initialized':
        pass
    elif method == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif method == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        print(json.dumps({'method': 'turn/completed', 'params': {'turnId': 'turn-1', 'lastAssistantMessage': 'done via app server'}}), flush=True)
        break
`)
	wd := codexWorkdir(t, "app-server prompt canary")
	outPath := filepath.Join(wd, ".aiops/CODEX_APP_SERVER_OUTPUT.txt")
	if err := os.WriteFile(outPath, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := (CodexAppServerRunner{}).Run(context.Background(), appServerInput(wd))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Summary != "done via app server" {
		t.Fatalf("Summary = %q, want app-server completion summary", res.Summary)
	}
	assertSensitiveArtifactPerm(t, outPath)

	argv, err := os.ReadFile(filepath.Join(binDir, "argv.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(argv)); got != "app-server" {
		t.Fatalf("codex argv = %q, want app-server", got)
	}

	stdin, err := os.ReadFile(filepath.Join(binDir, "stdin.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var messages []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(stdin)), "\n") {
		if line == "" {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("decode stdin JSON line %q: %v", line, err)
		}
		messages = append(messages, msg)
	}
	if len(messages) != 4 {
		t.Fatalf("sent %d messages, want initialize/initialized/thread/start/turn/start; stdin=%s", len(messages), stdin)
	}
	if messages[0]["method"] != "initialize" || messages[1]["method"] != "initialized" || messages[2]["method"] != "thread/start" || messages[3]["method"] != "turn/start" {
		t.Fatalf("unexpected method sequence: %#v", messages)
	}
	threadParams := messages[2]["params"].(map[string]any)
	if threadParams["cwd"] != wd {
		t.Fatalf("thread cwd = %#v, want %q", threadParams["cwd"], wd)
	}
	if threadParams["approvalPolicy"] != "never" {
		t.Fatalf("approvalPolicy = %#v, want never", threadParams["approvalPolicy"])
	}
	if threadParams["sandbox"] != "workspace-write" {
		t.Fatalf("sandbox = %#v, want workspace-write", threadParams["sandbox"])
	}
	turnParams := messages[3]["params"].(map[string]any)
	input := turnParams["input"].([]any)[0].(map[string]any)
	if input["text"] != "app-server prompt canary" {
		t.Fatalf("turn prompt text = %#v, want prompt file contents", input["text"])
	}
	if turnParams["title"] != "AIOPS-64: Wire Codex app-server" {
		t.Fatalf("turn title = %#v", turnParams["title"])
	}
}

func TestCodexAppServerRunnerDoesNotInheritWorkerSecretsByDefault(t *testing.T) {
	codexAppServerEnvStubScript(t, `
import json
for line in sys.stdin:
    msg=json.loads(line)
    method=msg.get('method')
    if method == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {'protocolVersion': '0.1.0'}}), flush=True)
    elif method == 'initialized':
        pass
    elif method == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif method == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        print(json.dumps({'method': 'turn/completed', 'params': {'turnId': 'turn-1', 'lastAssistantMessage': 'done'}}), flush=True)
        break
`)
	wd := codexWorkdir(t, "app-server env")
	t.Setenv("LINEAR_API_KEY", "linear-secret")
	t.Setenv("GITEA_TOKEN", "gitea-secret")
	t.Setenv("GITHUB_TOKEN", "github-secret")

	if _, err := (CodexAppServerRunner{}).Run(context.Background(), appServerInput(wd)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(wd, "env.txt"))
	if err != nil {
		t.Fatalf("read env.txt: %v", err)
	}
	for _, secretName := range []string{"LINEAR_API_KEY", "GITEA_TOKEN", "GITHUB_TOKEN"} {
		if strings.Contains(string(body), secretName+"=") {
			t.Fatalf("codex app-server env leaked %s:\n%s", secretName, body)
		}
	}
	if !strings.Contains(string(body), "PATH=") {
		t.Fatalf("codex app-server env lost baseline PATH:\n%s", body)
	}
}

func TestCodexAppServerRunnerCapturesSpecRuntimeEvents(t *testing.T) {
	codexAppServerStubScript(t, `
import json
log=open(os.environ['CODEX_STDIN_LOG'], 'w')
for line in sys.stdin:
    log.write(line); log.flush()
    msg=json.loads(line)
    method=msg.get('method')
    if method == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif method == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif method == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        print(json.dumps({'method': 'item/updated', 'params': {'message': 'thinking'}}), flush=True)
        print(json.dumps({'method': 'turn/completed', 'params': {'turnId': 'turn-1', 'lastAssistantMessage': 'done', 'usage': {'totalTokens': 5}}}), flush=True)
        break
`)
	wd := codexWorkdir(t, "capture runtime events")

	res, err := (CodexAppServerRunner{}).Run(context.Background(), appServerInput(wd))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.RuntimeEvents) != 3 {
		t.Fatalf("RuntimeEvents len = %d, want session_started/notification/turn_completed: %#v", len(res.RuntimeEvents), res.RuntimeEvents)
	}
	if res.RuntimeEvents[0].Event != task.EventSessionStarted {
		t.Fatalf("first event = %q, want %q", res.RuntimeEvents[0].Event, task.EventSessionStarted)
	}
	if got := runtimeEventField(t, res.RuntimeEvents[0], "session_id"); got != "thread-1-turn-1" {
		t.Fatalf("session_started session_id = %#v, want thread-1-turn-1", got)
	}
	if got := runtimeEventField(t, res.RuntimeEvents[0], "thread_id"); got != "thread-1" {
		t.Fatalf("session_started thread_id = %#v, want thread-1", got)
	}
	if got := runtimeEventField(t, res.RuntimeEvents[0], "turn_id"); got != "turn-1" {
		t.Fatalf("session_started turn_id = %#v, want turn-1", got)
	}
	pid := runtimeEventField(t, res.RuntimeEvents[0], "codex_app_server_pid")
	if pid == nil {
		t.Fatalf("session_started missing codex_app_server_pid; payload=%#v", res.RuntimeEvents[0].Payload)
	}
	if n, _ := pid.(int); n <= 0 {
		t.Fatalf("session_started codex_app_server_pid = %#v, want positive int (real codex subprocess PID)", pid)
	}
	if res.RuntimeEvents[1].Event != task.EventNotification {
		t.Fatalf("second event = %q, want %q", res.RuntimeEvents[1].Event, task.EventNotification)
	}
	if res.RuntimeEvents[2].Event != task.EventTurnCompleted {
		t.Fatalf("third event = %q, want %q", res.RuntimeEvents[2].Event, task.EventTurnCompleted)
	}
	if got := runtimeEventField(t, res.RuntimeEvents[2], "turn_id"); got != "turn-1" {
		t.Fatalf("turn_completed turn_id = %#v, want turn-1", got)
	}
}

func TestCodexAppServerRunnerEmitsOtherMessageForMethodlessRuntimePayload(t *testing.T) {
	codexAppServerStubScript(t, `
import json
for line in sys.stdin:
    msg=json.loads(line)
    method=msg.get('method')
    if method == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif method == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif method == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        print(json.dumps({'wrapper': 'unexpected', 'payload': {'message': 'known JSON without method'}}), flush=True)
        print(json.dumps({'method': 'turn/completed', 'params': {'lastAssistantMessage': 'done'}}), flush=True)
        break
`)
	wd := codexWorkdir(t, "other message")

	res, err := (CodexAppServerRunner{}).Run(context.Background(), appServerInput(wd))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	events := runtimeEventsNamed(res.RuntimeEvents, task.EventOtherMessage)
	if len(events) != 1 {
		t.Fatalf("other_message events = %d, want 1; events=%#v", len(events), res.RuntimeEvents)
	}
	if got := runtimeEventField(t, events[0], "wrapper"); got != "unexpected" {
		t.Fatalf("other_message wrapper = %#v, want unexpected; payload=%#v", got, events[0].Payload)
	}
}

func TestCodexAppServerRunnerEmitsMalformedAndContinuesForProtocolLikeInvalidJSON(t *testing.T) {
	codexAppServerStubScript(t, `
import json
for line in sys.stdin:
    msg=json.loads(line)
    method=msg.get('method')
    if method == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif method == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif method == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        print('{"method": "item/updated", "params": ', flush=True)
        print(json.dumps({'method': 'turn/completed', 'params': {'lastAssistantMessage': 'done after malformed'}}), flush=True)
        break
`)
	wd := codexWorkdir(t, "malformed message")

	res, err := (CodexAppServerRunner{}).Run(context.Background(), appServerInput(wd))
	if err != nil {
		t.Fatalf("Run: %v, want malformed protocol line to be reported then ignored", err)
	}
	events := runtimeEventsNamed(res.RuntimeEvents, task.EventMalformed)
	if len(events) != 1 {
		t.Fatalf("malformed events = %d, want 1; events=%#v", len(events), res.RuntimeEvents)
	}
	if raw, _ := runtimeEventField(t, events[0], "raw").(string); !strings.Contains(raw, `"item/updated"`) {
		t.Fatalf("malformed raw = %#v, want original invalid JSON line", raw)
	}
}

func TestCodexAppServerRunnerRejectsPlainMalformedStdout(t *testing.T) {
	codexAppServerStubScript(t, `
import json
for line in sys.stdin:
    msg=json.loads(line)
    method=msg.get('method')
    if method == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif method == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif method == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        print('plain diagnostic on stdout', flush=True)
        print(json.dumps({'method': 'turn/completed', 'params': {'lastAssistantMessage': 'should not complete'}}), flush=True)
        break
`)
	wd := codexWorkdir(t, "plain malformed stdout")

	res, err := (CodexAppServerRunner{}).Run(context.Background(), appServerInput(wd))
	if err == nil || !strings.Contains(err.Error(), "decode codex app-server message") {
		t.Fatalf("Run error = %v, want plain stdout garbage to remain a protocol decode failure", err)
	}
	if events := runtimeEventsNamed(res.RuntimeEvents, task.EventMalformed); len(events) != 0 {
		t.Fatalf("malformed events = %d, want non-protocol stdout to fail without malformed event; events=%#v", len(events), res.RuntimeEvents)
	}
}

func TestCodexAppServerRunnerTreatsUserInputRequestAsInputBlocked(t *testing.T) {
	codexAppServerStubScript(t, `
import json
log=open(os.environ['CODEX_STDIN_LOG'], 'w')
for line in sys.stdin:
    log.write(line); log.flush()
    msg=json.loads(line)
    if msg.get('method') == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif msg.get('method') == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif msg.get('method') == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        print(json.dumps({'jsonrpc': '2.0', 'id': 'input-1', 'method': 'item/tool/requestUserInput', 'params': {'questions': [{'id': 'q1', 'label': 'Need operator'}]}}), flush=True)
    elif msg.get('id') == 'input-1':
        answers = msg.get('result', {}).get('answers', {})
        if 'q1' not in answers:
            print(json.dumps({'method': 'turn/failed', 'params': {'reason': 'missing non-interactive answer', 'got': msg}}), flush=True)
        break
    elif msg.get('method') == 'initialized':
        pass
`)
	wd := codexWorkdir(t, "input blocked")

	res, err := (CodexAppServerRunner{}).Run(context.Background(), appServerInput(wd))
	if err == nil || !strings.Contains(err.Error(), "input required") {
		t.Fatalf("Run error = %v, want input required", err)
	}
	assertInputRequiredEvent(t, res.RuntimeEvents, "item/tool/requestUserInput")
}

func TestCodexAppServerRunnerTreatsMCPElicitationAsInputBlocked(t *testing.T) {
	codexAppServerStubScript(t, `
import json
log=open(os.environ['CODEX_STDIN_LOG'], 'w')
for line in sys.stdin:
    log.write(line); log.flush()
    msg=json.loads(line)
    if msg.get('method') == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif msg.get('method') == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif msg.get('method') == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        print(json.dumps({'jsonrpc': '2.0', 'id': 'elicitation-1', 'method': 'mcpServer/elicitation/request', 'params': {'message': 'Need operator input'}}), flush=True)
    elif msg.get('id') == 'elicitation-1':
        result = msg.get('result', {})
        if result.get('action') != 'decline' or result.get('content') is not None:
            print(json.dumps({'method': 'turn/failed', 'params': {'reason': 'unexpected elicitation response', 'got': msg}}), flush=True)
        break
    elif msg.get('method') == 'initialized':
        pass
`)
	wd := codexWorkdir(t, "mcp blocked")

	res, err := (CodexAppServerRunner{}).Run(context.Background(), appServerInput(wd))
	if err == nil || !strings.Contains(err.Error(), "input required") {
		t.Fatalf("Run error = %v, want input required", err)
	}
	assertInputRequiredEvent(t, res.RuntimeEvents, "mcpServer/elicitation/request")
}

// TestCodexAppServerRunnerEmitsStartupFailedOnInitializeError pins SPEC §10.4:
// a JSON-RPC error response to `initialize` is a startup failure and must
// surface as task.EventStartupFailed so /api/v1/state can distinguish "failed
// to start" from "ran a turn and failed".
func TestCodexAppServerRunnerEmitsStartupFailedOnInitializeError(t *testing.T) {
	codexAppServerStubScript(t, `
import json
for line in sys.stdin:
    msg=json.loads(line)
    if msg.get('method') == 'initialize':
        print(json.dumps({'id': msg['id'], 'error': {'code': -32603, 'message': 'codex bad startup'}}), flush=True)
        break
`)
	wd := codexWorkdir(t, "startup fails")

	res, err := (CodexAppServerRunner{}).Run(context.Background(), appServerInput(wd))
	if err == nil {
		t.Fatalf("Run error = nil, want startup failure")
	}
	events := runtimeEventsNamed(res.RuntimeEvents, task.EventStartupFailed)
	if len(events) != 1 {
		t.Fatalf("startup_failed events = %d, want 1; events=%#v", len(events), res.RuntimeEvents)
	}
	if got := runtimeEventField(t, events[0], "phase"); got != "initialize" {
		t.Fatalf("startup_failed phase = %#v, want initialize", got)
	}
	if got := runtimeEventField(t, events[0], "error"); got == nil || got == "" {
		t.Fatalf("startup_failed error = %#v, want a non-empty error string", got)
	}
}

// TestCodexAppServerRunnerEmitsStartupFailedOnThreadStartError pins the
// equivalent for the `thread/start` failure site.
func TestCodexAppServerRunnerEmitsStartupFailedOnThreadStartError(t *testing.T) {
	codexAppServerStubScript(t, `
import json
for line in sys.stdin:
    msg=json.loads(line)
    if msg.get('method') == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif msg.get('method') == 'thread/start':
        print(json.dumps({'id': msg['id'], 'error': {'code': -32603, 'message': 'thread denied'}}), flush=True)
        break
    elif msg.get('method') == 'initialized':
        pass
`)
	wd := codexWorkdir(t, "thread fails")

	res, err := (CodexAppServerRunner{}).Run(context.Background(), appServerInput(wd))
	if err == nil {
		t.Fatalf("Run error = nil, want thread/start failure")
	}
	events := runtimeEventsNamed(res.RuntimeEvents, task.EventStartupFailed)
	if len(events) != 1 {
		t.Fatalf("startup_failed events = %d, want 1; events=%#v", len(events), res.RuntimeEvents)
	}
	if got := runtimeEventField(t, events[0], "phase"); got != "thread/start" {
		t.Fatalf("startup_failed phase = %#v, want thread/start", got)
	}
}

// TestCodexAppServerRunnerEmitsUnsupportedToolCallForUnknownTool pins SPEC
// §10.4: when the agent invokes a tool the runtime did not advertise, the
// orchestrator must learn about it via task.EventUnsupportedToolCall (the
// wire still carries the structured failure result, but observability needs
// a typed event).
func TestCodexAppServerRunnerEmitsUnsupportedToolCallForUnknownTool(t *testing.T) {
	codexAppServerStubScript(t, `
import json
for line in sys.stdin:
    msg=json.loads(line)
    if msg.get('method') == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif msg.get('method') == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif msg.get('method') == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        print(json.dumps({'jsonrpc': '2.0', 'id': 'call-1', 'method': 'item/tool/call', 'params': {'name': 'missing_tool', 'arguments': {}}}), flush=True)
    elif msg.get('id') == 'call-1':
        print(json.dumps({'method': 'turn/completed', 'params': {'lastAssistantMessage': 'done after unsupported tool'}}), flush=True)
        break
    elif msg.get('method') == 'initialized':
        pass
`)
	wd := codexWorkdir(t, "unsupported tool")

	res, err := (CodexAppServerRunner{}).Run(context.Background(), appServerInput(wd))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	events := runtimeEventsNamed(res.RuntimeEvents, task.EventUnsupportedToolCall)
	if len(events) != 1 {
		t.Fatalf("unsupported_tool_call events = %d, want 1; events=%#v", len(events), res.RuntimeEvents)
	}
	if got := runtimeEventField(t, events[0], "tool"); got != "missing_tool" {
		t.Fatalf("unsupported_tool_call tool = %#v, want missing_tool", got)
	}
}

// TestCodexAppServerRunnerEmitsTurnInputRequiredOnDeclinedApproval pins SPEC
// §10.4 turn_input_required: an approval request that is not auto-approved
// (here `item/commandExecution/requestApproval` with the default `never`
// approval policy) is operator-required input — the runner must emit the
// event and bail with InputRequiredError so the orchestrator can mark the
// issue blocked, just as it does for `item/tool/requestUserInput`.
func TestCodexAppServerRunnerEmitsTurnInputRequiredOnDeclinedApproval(t *testing.T) {
	codexAppServerStubScript(t, `
import json
for line in sys.stdin:
    msg=json.loads(line)
    if msg.get('method') == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif msg.get('method') == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif msg.get('method') == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        print(json.dumps({'jsonrpc': '2.0', 'id': 'approve-1', 'method': 'item/commandExecution/requestApproval', 'params': {'command': 'rm -rf /'}}), flush=True)
    elif msg.get('id') == 'approve-1':
        result = msg.get('result', {})
        if result.get('decision') != 'decline':
            print(json.dumps({'method': 'turn/failed', 'params': {'reason': 'expected decline', 'got': msg}}), flush=True)
        break
    elif msg.get('method') == 'initialized':
        pass
`)
	wd := codexWorkdir(t, "declined approval")
	in := appServerInput(wd)
	// "untrusted" is the non-auto-approving policy: autoApproveRequest only
	// short-circuits on "never" / "on-failure" string policies, so any other
	// string drives the decline branch in protocolServerRequestResult.
	in.Workflow.Config.Codex.ApprovalPolicy = "untrusted"

	res, err := (CodexAppServerRunner{}).Run(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "input required") {
		t.Fatalf("Run error = %v, want input required for declined approval", err)
	}
	assertInputRequiredEvent(t, res.RuntimeEvents, "item/commandExecution/requestApproval")
}

func TestCodexAppServerRunnerEmitsSpecPhaseTransitions(t *testing.T) {
	codexAppServerStubScript(t, `
import json
log=open(os.environ['CODEX_STDIN_LOG'], 'w')
for line in sys.stdin:
    log.write(line); log.flush()
    msg=json.loads(line)
    method=msg.get('method')
    if method == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif method == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif method == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        print(json.dumps({'method': 'turn/completed', 'params': {'turnId': 'turn-1', 'lastAssistantMessage': 'done'}}), flush=True)
        break
`)
	wd := codexWorkdir(t, "capture phase transitions")
	in := appServerInput(wd)
	var transitions []task.PhaseTransition
	in.PhaseTransitionSink = func(from, to task.RunAttemptPhase) {
		transitions = append(transitions, task.PhaseTransitionEvent(from, to))
	}

	_, err := (CodexAppServerRunner{}).Run(context.Background(), in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []task.PhaseTransition{
		task.PhaseTransitionEvent(task.PhaseLaunchingAgentProcess, task.PhaseInitializingSession),
		task.PhaseTransitionEvent(task.PhaseInitializingSession, task.PhaseStreamingTurn),
	}
	if len(transitions) != len(want) {
		t.Fatalf("phase transitions = %#v, want %#v", transitions, want)
	}
	for i := range want {
		if transitions[i] != want[i] {
			t.Fatalf("phase transition[%d] = %#v, want %#v", i, transitions[i], want[i])
		}
	}
}

func TestCodexAppServerRunnerNormalizesNestedRuntimePayloadKeys(t *testing.T) {
	codexAppServerStubScript(t, `
import json
log=open(os.environ['CODEX_STDIN_LOG'], 'w')
for line in sys.stdin:
    log.write(line); log.flush()
    msg=json.loads(line)
    method=msg.get('method')
    if method == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif method == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif method == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        print(json.dumps({'method': 'turn/completed', 'params': {'turnID': 'turn-1', 'URLPath': '/review', 'usage': {'totalTokens': 5}, 'items': [{'itemID': 'i1'}], 'lastAssistantMessage': 'done'}}), flush=True)
        break
`)
	wd := codexWorkdir(t, "normalize nested keys")

	res, err := (CodexAppServerRunner{}).Run(context.Background(), appServerInput(wd))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	completed := res.RuntimeEvents[len(res.RuntimeEvents)-1]
	if got := runtimeEventField(t, completed, "turn_id"); got != "turn-1" {
		t.Fatalf("turn_id = %#v, want turn-1; payload=%#v", got, completed.Payload)
	}
	if got := runtimeEventField(t, completed, "url_path"); got != "/review" {
		t.Fatalf("url_path = %#v, want /review; payload=%#v", got, completed.Payload)
	}
	usage, ok := runtimeEventField(t, completed, "usage").(map[string]any)
	if !ok {
		t.Fatalf("usage payload is %T, want map[string]any; payload=%#v", runtimeEventField(t, completed, "usage"), completed.Payload)
	}
	if got := usage["total_tokens"]; got != float64(5) {
		t.Fatalf("usage.total_tokens = %#v, want 5; usage=%#v", got, usage)
	}
	items, ok := runtimeEventField(t, completed, "items").([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items payload = %#v, want one-item array", runtimeEventField(t, completed, "items"))
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("items[0] is %T, want map[string]any", items[0])
	}
	if got := item["item_id"]; got != "i1" {
		t.Fatalf("items[0].item_id = %#v, want i1; item=%#v", got, item)
	}
}

func TestCodexAppServerRunnerEmitsSessionStartedOnceForMultiTurnRun(t *testing.T) {
	codexAppServerStubScript(t, `
import json
turns=0
log=open(os.environ['CODEX_STDIN_LOG'], 'w')
for line in sys.stdin:
    log.write(line); log.flush()
    msg=json.loads(line)
    method=msg.get('method')
    if method == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif method == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif method == 'turn/start':
        turns += 1
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-%d' % turns}}}), flush=True)
        if turns == 1:
            print(json.dumps({'method': 'turn/completed', 'params': {'lastAssistantMessage': 'continue', 'continue': True}}), flush=True)
        else:
            print(json.dumps({'method': 'turn/completed', 'params': {'lastAssistantMessage': 'done'}}), flush=True)
            break
`)
	wd := codexWorkdir(t, "session once")
	in := appServerInput(wd)
	in.Workflow.Config.Agent.MaxTurns = 2

	res, err := (CodexAppServerRunner{}).Run(context.Background(), in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sessions []task.RuntimeEvent
	for _, event := range res.RuntimeEvents {
		if event.Event == task.EventSessionStarted {
			sessions = append(sessions, event)
		}
	}
	if len(sessions) != 1 {
		t.Fatalf("session_started count = %d, want 1; events=%#v", len(sessions), res.RuntimeEvents)
	}
	if got := runtimeEventField(t, sessions[0], "turn_id"); got != "turn-1" {
		t.Fatalf("session_started turn_id = %#v, want first turn id", got)
	}
}

func runtimeEventField(t *testing.T, event task.RuntimeEvent, key string) any {
	t.Helper()
	payload, ok := event.Payload.(map[string]any)
	if !ok {
		t.Fatalf("runtime event payload is %T, want map[string]any: %#v", event.Payload, event.Payload)
	}
	return payload[key]
}

func assertInputRequiredEvent(t *testing.T, events []task.RuntimeEvent, method string) {
	t.Helper()
	for _, event := range events {
		if event.Event != task.EventTurnInputRequired {
			continue
		}
		if got := runtimeEventField(t, event, "method"); got != method {
			t.Fatalf("turn_input_required method = %#v, want %q", got, method)
		}
		if got := runtimeEventField(t, event, "turn_id"); got != "turn-1" {
			t.Fatalf("turn_input_required turn_id = %#v, want turn-1", got)
		}
		return
	}
	t.Fatalf("runtime events missing turn_input_required for %s: %#v", method, events)
}

func runtimeEventsNamed(events []task.RuntimeEvent, name string) []task.RuntimeEvent {
	var matches []task.RuntimeEvent
	for _, event := range events {
		if event.Event == name {
			matches = append(matches, event)
		}
	}
	return matches
}

func TestCodexAppServerRunnerDoesNotPutLinearTokenOnWire(t *testing.T) {
	secret := "lin_api_secret_should_not_leak"
	binDir := codexAppServerStubScript(t, `
import json
log=open(os.environ['CODEX_STDIN_LOG'], 'w')
for line in sys.stdin:
    log.write(line); log.flush()
    msg=json.loads(line)
    if msg.get('method') == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif msg.get('method') == 'thread/start':
        tools = msg.get('params', {}).get('dynamicTools', [])
        linear = next((tool for tool in tools if tool.get('name') == 'linear_graphql'), None)
        workpad = next((tool for tool in tools if tool.get('name') == 'linear_ai_workpad'), None)
        if linear is None or 'inputSchema' not in linear or 'query' not in linear.get('inputSchema', {}).get('properties', {}):
            print(json.dumps({'id': msg['id'], 'error': {'message': 'linear_graphql missing inputSchema', 'tool': linear}}), flush=True)
            break
        if workpad is None or 'inputSchema' not in workpad or 'variables' not in workpad.get('inputSchema', {}).get('properties', {}):
            print(json.dumps({'id': msg['id'], 'error': {'message': 'linear_ai_workpad missing inputSchema', 'tool': workpad}}), flush=True)
            break
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif msg.get('method') == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        print(json.dumps({'method': 'turn/completed', 'params': {}}), flush=True)
        break
`)
	wd := codexWorkdir(t, "x")
	in := appServerInput(wd)
	in.Workflow.Config.Tracker = workflow.TrackerConfig{Kind: "linear", APIKey: secret}

	_, err := (CodexAppServerRunner{}).Run(context.Background(), in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	stdin, err := os.ReadFile(filepath.Join(binDir, "stdin.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stdin), secret) {
		t.Fatalf("app-server stdin leaked tracker api key: %s", stdin)
	}
}

func TestCodexAppServerRunnerInvokesLinearGraphQLThroughOrchestratorProxy(t *testing.T) {
	linear := &fakeLinearGraphQLServer{}
	linearServer := httptest.NewServer(linear.handler())
	defer linearServer.Close()
	oldEndpoint := defaultLinearGraphQLEndpoint
	defaultLinearGraphQLEndpoint = linearServer.URL
	t.Cleanup(func() { defaultLinearGraphQLEndpoint = oldEndpoint })

	secret := "lin_api_secret_should_not_leak_to_agent"
	t.Setenv("LINEAR_API_KEY", "")
	binDir := codexAppServerStubScript(t, `
import json
log=open(os.environ['CODEX_STDIN_LOG'], 'w')
for line in sys.stdin:
    log.write(line); log.flush()
    msg=json.loads(line)
    if msg.get('method') == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif msg.get('method') == 'thread/start':
        tools = msg.get('params', {}).get('dynamicTools', [])
        if not any(tool.get('name') == 'linear_graphql' for tool in tools):
            print(json.dumps({'id': msg['id'], 'error': {'message': 'linear_graphql was not advertised', 'tools': tools}}), flush=True)
            break
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif msg.get('method') == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        print(json.dumps({'method': 'item/tool/call', 'params': {'call_id': 'call-1', 'name': 'linear_graphql', 'arguments': {'query': 'mutation IssueUpdate($id: String!) { issueUpdate(id: $id, input: {}) { success } }', 'variables': {'id': 'issue-1'}}}}), flush=True)
    elif msg.get('method') == 'item/tool/call/output':
        output = msg.get('params', {}).get('output', {})
        if output.get('success') is not True:
            print(json.dumps({'method': 'turn/failed', 'params': {'reason': 'linear_graphql did not return success', 'output': output}}), flush=True)
            break
        print(json.dumps({'method': 'turn/completed', 'params': {'lastAssistantMessage': 'linear proxy worked'}}), flush=True)
        break
    elif msg.get('method') == 'initialized':
        pass
`)
	wd := codexWorkdir(t, "x")
	in := appServerInput(wd)
	in.Workflow.Config.Tracker = workflow.TrackerConfig{Kind: "linear", APIKey: secret}
	in.Workflow.Config.Codex.LinearGraphQL = workflow.LinearGraphQLConfig{AllowMutations: true}

	res, err := (CodexAppServerRunner{}).Run(context.Background(), in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Summary != "linear proxy worked" {
		t.Fatalf("Summary = %q, want linear dynamic tool completion", res.Summary)
	}
	stdin, err := os.ReadFile(filepath.Join(binDir, "stdin.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stdin), secret) {
		t.Fatalf("app-server stdin leaked tracker api key: %s", stdin)
	}
	auth, body, requests := linear.recorded()
	if requests != 1 {
		t.Fatalf("Linear GraphQL requests = %d, want 1", requests)
	}
	if auth != "Bearer "+secret {
		t.Fatalf("Authorization = %q, want orchestrator-held Linear token", auth)
	}
	if strings.Contains(body, secret) {
		t.Fatalf("GraphQL request body leaked token to agent-controlled payload: %s", body)
	}
	if !strings.Contains(body, `"query"`) || !strings.Contains(body, `"variables"`) {
		t.Fatalf("GraphQL request body missing query/variables: %s", body)
	}
	mutationEvents := runtimeEventsNamed(res.RuntimeEvents, task.EventToolCallMutation)
	if len(mutationEvents) != 1 {
		t.Fatalf("tool_call_mutation events = %d, want 1; events=%#v", len(mutationEvents), res.RuntimeEvents)
	}
	if got := runtimeEventField(t, mutationEvents[0], "operation_field"); got != "issueUpdate" {
		t.Fatalf("tool_call_mutation operation_field = %#v, want issueUpdate", got)
	}
	if got := runtimeEventField(t, mutationEvents[0], "tool"); got != "linear_graphql" {
		t.Fatalf("tool_call_mutation tool = %#v, want linear_graphql", got)
	}
	payloadJSON, err := json.Marshal(mutationEvents[0].Payload)
	if err != nil {
		t.Fatalf("marshal mutation payload: %v", err)
	}
	if strings.Contains(string(payloadJSON), "issueUpdate(id:") || strings.Contains(string(payloadJSON), `"issue-1"`) {
		t.Fatalf("tool_call_mutation leaked GraphQL query body / variables onto status surface: %s", payloadJSON)
	}
}

func TestCodexAppServerRunnerHandlesDynamicToolCallsAndContinuesSession(t *testing.T) {
	binDir := codexAppServerStubScript(t, `
import json
log=open(os.environ['CODEX_STDIN_LOG'], 'w')
for line in sys.stdin:
    log.write(line); log.flush()
    msg=json.loads(line)
    if msg.get('method') == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif msg.get('method') == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif msg.get('method') == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        print(json.dumps({'jsonrpc': '2.0', 'id': 'call-1', 'method': 'item/tool/call', 'params': {'name': 'missing_tool', 'arguments': {'query': 'query { viewer { id } }'}}}), flush=True)
    elif msg.get('id') == 'call-1':
        result=msg.get('result', {})
        if result.get('success') is not False or 'unsupported dynamic tool' not in result.get('output', ''):
            print(json.dumps({'method': 'turn/failed', 'params': {'reason': 'unsupported tool did not return structured failure', 'got': result}}), flush=True)
            break
        print(json.dumps({'method': 'turn/completed', 'params': {'lastAssistantMessage': 'continued after unsupported tool'}}), flush=True)
        break
    elif msg.get('method') == 'initialized':
        pass
`)
	wd := codexWorkdir(t, "x")

	res, err := (CodexAppServerRunner{}).Run(context.Background(), appServerInput(wd))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Summary != "continued after unsupported tool" {
		t.Fatalf("Summary = %q, want continued turn completion", res.Summary)
	}
	stdin, err := os.ReadFile(filepath.Join(binDir, "stdin.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stdin), `"id":"call-1"`) || !strings.Contains(string(stdin), `"success":false`) {
		t.Fatalf("stdin did not include structured unsupported-tool response: %s", stdin)
	}
}

func TestCodexAppServerRunnerHonorsMaxTurnsForContinuationRequests(t *testing.T) {
	codexAppServerStubScript(t, `
import json
turns=0
log=open(os.environ['CODEX_STDIN_LOG'], 'w')
for line in sys.stdin:
    log.write(line); log.flush()
    msg=json.loads(line)
    if msg.get('method') == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif msg.get('method') == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif msg.get('method') == 'turn/start':
        turns += 1
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-%d' % turns}}}), flush=True)
        if turns < 3:
            print(json.dumps({'method': 'turn/completed', 'params': {'lastAssistantMessage': 'needs another turn', 'continue': True}}), flush=True)
        else:
            print(json.dumps({'method': 'turn/completed', 'params': {'lastAssistantMessage': 'done after turn 3'}}), flush=True)
            break
    elif msg.get('method') == 'initialized':
        pass
`)
	wd := codexWorkdir(t, "x")
	in := appServerInput(wd)
	in.Workflow.Config.Agent.MaxTurns = 3

	res, err := (CodexAppServerRunner{}).Run(context.Background(), in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Summary != "done after turn 3" {
		t.Fatalf("Summary = %q, want final third-turn summary", res.Summary)
	}
}

// TestCodexAppServerRunnerExitsWhenIssueLeavesActiveStateBetweenTurns pins
// SPEC §16.5: after each turn the runner consults the tracker and exits
// cleanly when the linked issue is no longer active, even when the agent
// is requesting another turn via params.continue=true. Without this gate
// the worker would burn the full agent.max_turns budget before the
// orchestrator's next per-tick reconcile noticed the operator cancel.
func TestCodexAppServerRunnerExitsWhenIssueLeavesActiveStateBetweenTurns(t *testing.T) {
	codexAppServerStubScript(t, `
import json
turns=0
log=open(os.environ['CODEX_STDIN_LOG'], 'w')
for line in sys.stdin:
    log.write(line); log.flush()
    msg=json.loads(line)
    if msg.get('method') == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif msg.get('method') == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif msg.get('method') == 'turn/start':
        turns += 1
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-%d' % turns}}}), flush=True)
        print(json.dumps({'method': 'turn/completed', 'params': {'lastAssistantMessage': 'turn %d done' % turns, 'continue': True}}), flush=True)
    elif msg.get('method') == 'initialized':
        pass
`)
	wd := codexWorkdir(t, "x")
	in := appServerInput(wd)
	in.Workflow.Config.Agent.MaxTurns = 8

	var refreshCalls int
	in.RefreshIssueState = func(ctx context.Context) (bool, error) {
		refreshCalls++
		// First turn: issue is still active, runner should request
		// another turn. Second turn: operator cancelled (issue moved to
		// "Done"); refresher reports inactive and the runner must exit
		// before sending the third turn/start even though the agent's
		// continue=true would otherwise keep the loop running.
		return refreshCalls < 2, nil
	}

	res, err := (CodexAppServerRunner{}).Run(context.Background(), in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if refreshCalls != 2 {
		t.Fatalf("RefreshIssueState calls = %d, want 2 (one after each completed turn before the third start)", refreshCalls)
	}
	completed := runtimeEventsNamed(res.RuntimeEvents, task.EventTurnCompleted)
	if len(completed) != 2 {
		t.Fatalf("turn_completed events = %d, want exactly 2 (refresher cut off the agent's third turn); events=%#v", len(completed), res.RuntimeEvents)
	}
	if res.Summary != "turn 2 done" {
		t.Fatalf("Summary = %q, want last completed turn's message", res.Summary)
	}
	// The stdin log captures every JSON-RPC message the runner sent to
	// codex. Counting turn/start requests proves the runner *didn't even
	// send* a third turn — without this, "only 2 completions" could just
	// mean the third response never returned in time.
	stdinLog, err := os.ReadFile(os.Getenv("CODEX_STDIN_LOG"))
	if err != nil {
		t.Fatalf("read CODEX_STDIN_LOG: %v", err)
	}
	if got := strings.Count(string(stdinLog), `"method":"turn/start"`); got != 2 {
		t.Fatalf("turn/start requests sent = %d, want 2 (runner should not send a third turn after refresher returns inactive); stdin=\n%s", got, stdinLog)
	}
}

// TestCodexAppServerRunnerSurfacesRefreshIssueStateError pins the SPEC
// §16.5 "if refreshed_issue failed: fail" branch: a tracker outage during
// the per-turn refresh aborts the loop with the refresher's error
// wrapped, instead of letting the worker continue running with stale
// continuation state.
func TestCodexAppServerRunnerSurfacesRefreshIssueStateError(t *testing.T) {
	codexAppServerStubScript(t, `
import json
for line in sys.stdin:
    msg=json.loads(line)
    if msg.get('method') == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif msg.get('method') == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif msg.get('method') == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        print(json.dumps({'method': 'turn/completed', 'params': {'lastAssistantMessage': 'mid-run', 'continue': True}}), flush=True)
    elif msg.get('method') == 'initialized':
        pass
`)
	wd := codexWorkdir(t, "x")
	in := appServerInput(wd)
	in.Workflow.Config.Agent.MaxTurns = 4
	refresherErr := errors.New("tracker unreachable")
	in.RefreshIssueState = func(ctx context.Context) (bool, error) {
		return false, refresherErr
	}

	_, err := (CodexAppServerRunner{}).Run(context.Background(), in)
	if err == nil || !errors.Is(err, refresherErr) {
		t.Fatalf("Run error = %v, want wrapped refresher error %q", err, refresherErr)
	}
}

func TestCodexAppServerRunnerTreatsFailedTurnCompletedAsError(t *testing.T) {
	codexAppServerStubScript(t, `
import json
for line in sys.stdin:
    msg=json.loads(line)
    if msg.get('method') == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif msg.get('method') == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif msg.get('method') == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        print(json.dumps({'method': 'turn/completed', 'params': {'status': 'failed', 'reason': 'tool crashed'}}), flush=True)
        break
`)
	wd := codexWorkdir(t, "x")

	res, err := (CodexAppServerRunner{}).Run(context.Background(), appServerInput(wd))
	if err == nil || !strings.Contains(err.Error(), "turn/completed failed") || !strings.Contains(err.Error(), "tool crashed") {
		t.Fatalf("Run error = %v, want failed turn/completed error with reason", err)
	}
	events := runtimeEventsNamed(res.RuntimeEvents, task.EventTurnEndedWithError)
	if len(events) != 1 {
		t.Fatalf("turn_ended_with_error events = %d, want 1; events=%#v", len(events), res.RuntimeEvents)
	}
	if got := runtimeEventField(t, events[0], "reason"); got != "tool crashed" {
		t.Fatalf("turn_ended_with_error reason = %#v, want tool crashed; payload=%#v", got, events[0].Payload)
	}
}

func TestCodexAppServerRunnerTreatsInterruptedTurnCompletedAsSuccess(t *testing.T) {
	codexAppServerStubScript(t, `
import json
for line in sys.stdin:
    msg=json.loads(line)
    if msg.get('method') == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif msg.get('method') == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif msg.get('method') == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        print(json.dumps({'method': 'turn/completed', 'params': {'status': 'interrupted', 'lastAssistantMessage': 'cancelled cleanly'}}), flush=True)
        break
`)
	wd := codexWorkdir(t, "x")

	res, err := (CodexAppServerRunner{}).Run(context.Background(), appServerInput(wd))
	if err != nil {
		t.Fatalf("Run: %v, want interrupted turn/completed to be a normal terminal state", err)
	}
	if res.Summary != "cancelled cleanly" {
		t.Fatalf("Summary = %q, want interrupted turn summary", res.Summary)
	}
}

func TestCodexAppServerRunnerUsesReadTimeout(t *testing.T) {
	codexAppServerStubScript(t, `
import json, time
for line in sys.stdin:
    msg=json.loads(line)
    if msg.get('method') == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif msg.get('method') == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif msg.get('method') == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        time.sleep(2)
        break
`)
	wd := codexWorkdir(t, "x")
	in := appServerInput(wd)
	in.Workflow.Config.Codex.ReadTimeoutMs = 25
	in.Workflow.Config.Codex.StallTimeoutMs = 0
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := (CodexAppServerRunner{}).Run(ctx, in)
	if err == nil || !strings.Contains(err.Error(), "read timeout") {
		t.Fatalf("Run error = %v, want app-server read timeout", err)
	}
}

func TestCodexAppServerRunnerReadTimeoutDoesNotPreemptStallBudget(t *testing.T) {
	codexAppServerStubScript(t, `
import json, time
for line in sys.stdin:
    msg=json.loads(line)
    if msg.get('method') == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif msg.get('method') == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif msg.get('method') == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        print(json.dumps({'method': 'item/updated', 'params': {'message': 'started'}}), flush=True)
        time.sleep(2)
        break
`)
	wd := codexWorkdir(t, "x")
	in := appServerInput(wd)
	in.Workflow.Config.Codex.ReadTimeoutMs = 5000
	in.Workflow.Config.Codex.StallTimeoutMs = 250
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	_, err := (CodexAppServerRunner{}).Run(ctx, in)
	elapsed := time.Since(start)
	if !IsStall(err) {
		t.Fatalf("Run error = %T %[1]v, want stall timeout despite shorter read timeout", err)
	}
	if elapsed < 250*time.Millisecond {
		t.Fatalf("Run elapsed = %s, want read_timeout_ms not to fire before stall_timeout_ms", elapsed)
	}
}

func TestCodexAppServerRunnerTreatsNestedFailedTurnCompletedAsError(t *testing.T) {
	codexAppServerStubScript(t, `
import json
for line in sys.stdin:
    msg=json.loads(line)
    if msg.get('method') == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif msg.get('method') == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif msg.get('method') == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        print(json.dumps({'method': 'turn/completed', 'params': {'turn': {'status': 'failed', 'error': 'nested tool failure'}}}), flush=True)
        break
`)
	wd := codexWorkdir(t, "x")

	_, err := (CodexAppServerRunner{}).Run(context.Background(), appServerInput(wd))
	if err == nil || !strings.Contains(err.Error(), "turn/completed failed") || !strings.Contains(err.Error(), "nested tool failure") {
		t.Fatalf("Run error = %v, want nested failed turn/completed error with reason", err)
	}
}

func TestCodexAppServerRunnerRedactsTurnFailureParams(t *testing.T) {
	const secret = "sk-secret-must-not-leak-9f7c1e"
	cases := []struct {
		name      string
		stubBody  string
		event     string
		errSubstr string
		reason    string
	}{
		{
			name: "turn_failed",
			stubBody: `
import json
for line in sys.stdin:
    msg=json.loads(line)
    if msg.get('method') == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif msg.get('method') == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif msg.get('method') == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        print(json.dumps({'method': 'turn/failed', 'params': {'reason': 'planner exploded', 'leak': '` + secret + `', 'got': {'inner': '` + secret + `'}}}), flush=True)
        break
`,
			event:     task.EventTurnFailed,
			errSubstr: "planner exploded",
			reason:    "planner exploded",
		},
		{
			name: "turn_cancelled",
			stubBody: `
import json
for line in sys.stdin:
    msg=json.loads(line)
    if msg.get('method') == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif msg.get('method') == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif msg.get('method') == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        print(json.dumps({'method': 'turn/cancelled', 'params': {'message': 'user aborted', 'leak': '` + secret + `'}}), flush=True)
        break
`,
			event:     task.EventTurnCancelled,
			errSubstr: "user aborted",
			reason:    "user aborted",
		},
		{
			name: "turn_completed_structured_turn_error",
			stubBody: `
import json
for line in sys.stdin:
    msg=json.loads(line)
    if msg.get('method') == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif msg.get('method') == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif msg.get('method') == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        print(json.dumps({'method': 'turn/completed', 'params': {'turn': {'status': 'failed', 'error': {'message': 'sandbox denied write', 'code': 'sandbox_violation', 'leak_inside_error': '` + secret + `'}}, 'leak': '` + secret + `'}}), flush=True)
        break
`,
			event:     task.EventTurnEndedWithError,
			errSubstr: "sandbox denied write",
			reason:    "sandbox denied write",
		},
		{
			name: "turn_failed_structured_error",
			stubBody: `
import json
for line in sys.stdin:
    msg=json.loads(line)
    if msg.get('method') == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif msg.get('method') == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif msg.get('method') == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        print(json.dumps({'method': 'turn/failed', 'params': {'error': {'message': 'planner crashed', 'leak': '` + secret + `'}}}), flush=True)
        break
`,
			event:     task.EventTurnFailed,
			errSubstr: "planner crashed",
			reason:    "planner crashed",
		},
		{
			name: "turn_completed_unknown_status_fallback",
			stubBody: `
import json
for line in sys.stdin:
    msg=json.loads(line)
    if msg.get('method') == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif msg.get('method') == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif msg.get('method') == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        print(json.dumps({'method': 'turn/completed', 'params': {'status': 'broken', 'leak': '` + secret + `'}}), flush=True)
        break
`,
			event:     task.EventTurnEndedWithError,
			errSubstr: "reason unavailable",
			reason:    "reason unavailable",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			codexAppServerStubScript(t, tc.stubBody)
			wd := codexWorkdir(t, "x")

			res, err := (CodexAppServerRunner{}).Run(context.Background(), appServerInput(wd))
			if err == nil {
				t.Fatalf("Run err = nil, want failure")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("err leaked secret: %v", err)
			}
			if !strings.Contains(err.Error(), tc.errSubstr) {
				t.Fatalf("err = %v, want substring %q", err, tc.errSubstr)
			}
			events := runtimeEventsNamed(res.RuntimeEvents, tc.event)
			if len(events) != 1 {
				t.Fatalf("%s events = %d, want 1; events=%#v", tc.event, len(events), res.RuntimeEvents)
			}
			payloadJSON, marshalErr := json.Marshal(events[0].Payload)
			if marshalErr != nil {
				t.Fatalf("marshal payload: %v", marshalErr)
			}
			if bytes.Contains(payloadJSON, []byte(secret)) {
				t.Fatalf("payload leaked secret: %s", payloadJSON)
			}
			if got := runtimeEventField(t, events[0], "reason"); got != tc.reason {
				t.Fatalf("reason = %#v, want %q; payload=%s", got, tc.reason, payloadJSON)
			}
			if _, ok := events[0].Payload.(map[string]any)["leak"]; ok {
				t.Fatalf("payload should not retain non-allowlisted field 'leak': %s", payloadJSON)
			}
		})
	}
}

func TestCodexAppServerRunnerSendsContinuationInputAfterContinueRequest(t *testing.T) {
	binDir := codexAppServerStubScript(t, `
import json
turns=0
log=open(os.environ['CODEX_STDIN_LOG'], 'w')
for line in sys.stdin:
    log.write(line); log.flush()
    msg=json.loads(line)
    if msg.get('method') == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif msg.get('method') == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif msg.get('method') == 'turn/start':
        turns += 1
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-%d' % turns}}}), flush=True)
        if turns == 1:
            print(json.dumps({'method': 'turn/completed', 'params': {'lastAssistantMessage': 'needs follow-up', 'continue': True}}), flush=True)
        else:
            print(json.dumps({'method': 'turn/completed', 'params': {'lastAssistantMessage': 'done'}}), flush=True)
            break
    elif msg.get('method') == 'initialized':
        pass
`)
	wd := codexWorkdir(t, "x")
	in := appServerInput(wd)
	in.Workflow.Config.Agent.MaxTurns = 2

	_, err := (CodexAppServerRunner{}).Run(context.Background(), in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	stdin, err := os.ReadFile(filepath.Join(binDir, "stdin.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var turnInputs []any
	for _, line := range strings.Split(strings.TrimSpace(string(stdin)), "\n") {
		if line == "" {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("decode stdin JSON line %q: %v", line, err)
		}
		if msg["method"] == "turn/start" {
			params := msg["params"].(map[string]any)
			turnInputs = append(turnInputs, params["input"])
		}
	}
	if len(turnInputs) != 2 {
		t.Fatalf("turn/start count = %d, want 2", len(turnInputs))
	}
	items, ok := turnInputs[1].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("second turn input = %#v, want non-empty continuation guidance items", turnInputs[1])
	}
	text, _ := items[0].(map[string]any)["text"].(string)
	if !strings.Contains(strings.ToLower(text), "continue") || !strings.Contains(text, "AIOPS-64") {
		t.Fatalf("continuation text = %q, want task-specific continuation guidance", text)
	}
}

func TestBuildCodexAppServerCmdUsesAppServerWhenDefaultCodexExecCommandIsUnchanged(t *testing.T) {
	codexAppServerStubScript(t, `
`)
	wd := codexWorkdir(t, "x")
	in := appServerInput(wd)
	in.Workflow.Config.Codex.Command = "codex exec"

	cmd, directExec, err := buildCodexAppServerCmd(context.Background(), in, []string{"PATH=" + os.Getenv("PATH")})
	if err != nil {
		t.Fatalf("buildCodexAppServerCmd: %v", err)
	}
	if !directExec {
		t.Fatalf("directCodexExec = false for default codex command, want true")
	}
	if len(cmd.Args) < 2 || filepath.Base(cmd.Args[0]) != "codex" || cmd.Args[1] != "app-server" {
		t.Fatalf("cmd.Args = %#v, want codex app-server", cmd.Args)
	}
}

func TestBuildCodexAppServerCmdResolvesCodexFromAgentEnvPATH(t *testing.T) {
	rawPathDir := t.TempDir()
	t.Setenv("PATH", rawPathDir)
	binDir := t.TempDir()
	codex := filepath.Join(binDir, "codex")
	if err := os.WriteFile(codex, []byte("#!/usr/bin/env sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	wd := codexWorkdir(t, "x")
	in := appServerInput(wd)

	cmd, directExec, err := buildCodexAppServerCmd(context.Background(), in, []string{"PATH=" + binDir})
	if err != nil {
		t.Fatalf("buildCodexAppServerCmd: %v", err)
	}
	if !directExec {
		t.Fatalf("directCodexExec = false, want true")
	}
	if cmd.Path != codex {
		t.Fatalf("cmd.Path = %q, want codex from agent env PATH %q", cmd.Path, codex)
	}
}

// TestBuildCodexAppServerCmdReportsCustomCommandAsIndirect pins the
// directCodexExec flag's negative case: when `codex.command` is anything other
// than the built-in `codex app-server` invocation, the cmd runs through `sh -c`.
// The runner
// uses this signal to suppress codex_app_server_pid emission, since
// cmd.Process.Pid would be the shell wrapper rather than the actual codex
// process.
func TestBuildCodexAppServerCmdReportsCustomCommandAsIndirect(t *testing.T) {
	wd := codexWorkdir(t, "x")
	in := appServerInput(wd)
	in.Workflow.Config.Codex.Command = "/usr/local/bin/my-codex-wrapper --foo"

	cmd, directExec, err := buildCodexAppServerCmd(context.Background(), in, []string{"PATH=" + os.Getenv("PATH")})
	if err != nil {
		t.Fatalf("buildCodexAppServerCmd: %v", err)
	}
	if directExec {
		t.Fatalf("directCodexExec = true for custom command %q, want false (sh -c wrapper)", in.Workflow.Config.Codex.Command)
	}
	if len(cmd.Args) < 3 || cmd.Args[0] != "sh" || cmd.Args[1] != "-c" || cmd.Args[2] != in.Workflow.Config.Codex.Command {
		t.Fatalf("cmd.Args = %#v, want sh -c wrapper", cmd.Args)
	}
}

func TestBuildCodexAppServerCmdPreservesQuotedCustomCodexCommand(t *testing.T) {
	wd := codexWorkdir(t, "x")
	in := appServerInput(wd)
	in.Workflow.Config.Codex.Command = `codex app-server --config "model = gpt-5"`

	cmd, directExec, err := buildCodexAppServerCmd(context.Background(), in, []string{"PATH=" + os.Getenv("PATH")})
	if err != nil {
		t.Fatalf("buildCodexAppServerCmd: %v", err)
	}
	if directExec {
		t.Fatalf("directCodexExec = true for custom command %q, want false (shell wrapper)", in.Workflow.Config.Codex.Command)
	}
	if len(cmd.Args) < 3 || cmd.Args[0] != "sh" || cmd.Args[1] != "-c" || cmd.Args[2] != in.Workflow.Config.Codex.Command {
		t.Fatalf("cmd.Args = %#v, want shell wrapper preserving quoted command", cmd.Args)
	}
}

func TestCodexAppServerRunnerUsesTurnTimeout(t *testing.T) {
	codexAppServerStubScript(t, `
import json, time
for line in sys.stdin:
    msg=json.loads(line)
    if msg.get('method') == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif msg.get('method') == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif msg.get('method') == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        time.sleep(2)
        print(json.dumps({'method': 'turn/completed', 'params': {'lastAssistantMessage': 'too late'}}), flush=True)
        break
`)
	wd := codexWorkdir(t, "x")
	in := appServerInput(wd)
	in.Workflow.Config.Codex.TurnTimeoutMs = 25
	in.Workflow.Config.Codex.ReadTimeoutMs = 5000
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := (CodexAppServerRunner{}).Run(ctx, in)
	if err == nil || !strings.Contains(err.Error(), "turn timeout") {
		t.Fatalf("Run error = %v, want app-server turn timeout", err)
	}
}

func TestCodexAppServerRunnerUsesStallTimeoutWhenEventStreamGoesSilent(t *testing.T) {
	codexAppServerStubScript(t, `
import json, time
for line in sys.stdin:
    msg=json.loads(line)
    if msg.get('method') == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif msg.get('method') == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif msg.get('method') == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        print(json.dumps({'method': 'item/updated', 'params': {'message': 'initial progress'}}), flush=True)
        time.sleep(2)
        break
`)
	wd := codexWorkdir(t, "x")
	in := appServerInput(wd)
	in.Workflow.Config.Codex.ReadTimeoutMs = 5000
	in.Workflow.Config.Codex.StallTimeoutMs = 25
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := (CodexAppServerRunner{}).Run(ctx, in)
	if err == nil || !strings.Contains(err.Error(), "stall timeout") {
		t.Fatalf("Run error = %v, want app-server stall timeout", err)
	}
	if !IsStall(err) {
		t.Fatalf("Run error = %T %[1]v, want runner.IsStall=true", err)
	}
}

func TestCodexAppServerRunnerActivityWithinStallWindowKeepsTurnAlive(t *testing.T) {
	codexAppServerStubScript(t, `
import json, time
for line in sys.stdin:
    msg=json.loads(line)
    if msg.get('method') == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif msg.get('method') == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif msg.get('method') == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        for i in range(5):
            print(json.dumps({'method': 'item/updated', 'params': {'message': 'progress %d' % i}}), flush=True)
            time.sleep(0.02)
        print(json.dumps({'method': 'turn/completed', 'params': {'lastAssistantMessage': 'completed after progress'}}), flush=True)
        break
`)
	wd := codexWorkdir(t, "x")
	in := appServerInput(wd)
	in.Workflow.Config.Codex.ReadTimeoutMs = 5000
	in.Workflow.Config.Codex.StallTimeoutMs = 1000
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	res, err := (CodexAppServerRunner{}).Run(ctx, in)
	if err != nil {
		t.Fatalf("Run: %v, want periodic app-server events to avoid stall timeout", err)
	}
	if res.Summary != "completed after progress" {
		t.Fatalf("Summary = %q, want completion after progress", res.Summary)
	}
}

func TestCodexAppServerRunnerDynamicToolCallsRefreshStallClock(t *testing.T) {
	codexAppServerStubScript(t, `
import json, time
for line in sys.stdin:
    msg=json.loads(line)
    if msg.get('method') == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif msg.get('method') == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif msg.get('method') == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        print(json.dumps({'method': 'item/tool/call', 'params': {'call_id': 'call-1', 'name': 'gitea_issue_labels', 'arguments': {'owner': 'o', 'repo': 'r', 'number': 1}}}), flush=True)
    elif msg.get('method') == 'item/tool/call/output':
        time.sleep(0.08)
        print(json.dumps({'method': 'turn/completed', 'params': {'lastAssistantMessage': 'completed after tool'}}), flush=True)
        break
`)
	wd := codexWorkdir(t, "x")
	in := appServerInput(wd)
	in.Workflow.Config.Codex.ReadTimeoutMs = 5000
	in.Workflow.Config.Codex.StallTimeoutMs = 1000
	in.Workflow.Config.Tracker.Kind = "gitea"
	in.Workflow.Config.Tracker.APIKey = "token"
	in.Workflow.Config.Tracker.ProjectSlug = "http://127.0.0.1:1"
	in.Workflow.Config.Repo.Owner = "o"
	in.Workflow.Config.Repo.Name = "r"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	res, err := (CodexAppServerRunner{}).Run(ctx, in)
	if err != nil {
		t.Fatalf("Run: %v, want dynamic tool call to count as activity after output", err)
	}
	if res.Summary != "completed after tool" {
		t.Fatalf("Summary = %q, want completion after tool", res.Summary)
	}
}

func TestCodexAppServerRunnerServerRequestsRefreshStallClock(t *testing.T) {
	codexAppServerStubScript(t, `
import json, time
for line in sys.stdin:
    msg=json.loads(line)
    if msg.get('method') == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif msg.get('method') == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif msg.get('method') == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        time.sleep(0.08)
        print(json.dumps({'jsonrpc': '2.0', 'id': 99, 'method': 'server/needs-help', 'params': {}}), flush=True)
    elif msg.get('id') == 99:
        time.sleep(0.08)
        print(json.dumps({'method': 'turn/completed', 'params': {'lastAssistantMessage': 'completed after server request'}}), flush=True)
        break
`)
	wd := codexWorkdir(t, "x")
	in := appServerInput(wd)
	in.Workflow.Config.Codex.ReadTimeoutMs = 5000
	in.Workflow.Config.Codex.StallTimeoutMs = 500
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	res, err := (CodexAppServerRunner{}).Run(ctx, in)
	if err != nil {
		t.Fatalf("Run: %v, want server request activity to avoid stall timeout", err)
	}
	if res.Summary != "completed after server request" {
		t.Fatalf("Summary = %q, want completion after server request", res.Summary)
	}
}

func TestCodexAppServerRunnerReturnsTurnTimeoutWithoutWaitingForOuterContext(t *testing.T) {
	codexAppServerStubScript(t, `
import json, time
for line in sys.stdin:
    msg=json.loads(line)
    if msg.get('method') == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif msg.get('method') == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif msg.get('method') == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        time.sleep(2)
        break
`)
	wd := codexWorkdir(t, "x")
	in := appServerInput(wd)
	in.Workflow.Config.Codex.TurnTimeoutMs = 25
	in.Workflow.Config.Codex.ReadTimeoutMs = 5000
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := (CodexAppServerRunner{}).Run(ctx, in)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "turn timeout") {
		t.Fatalf("Run error = %v, want app-server turn timeout", err)
	}
	if elapsed >= time.Second {
		t.Fatalf("Run elapsed %s, want return before outer context deadline", elapsed)
	}
}

func TestCodexAppServerRunnerRepliesToApprovalRequestsWithProtocolValidResults(t *testing.T) {
	binDir := codexAppServerStubScript(t, `
import json
log=open(os.environ['CODEX_STDIN_LOG'], 'w')
approval_methods = [
    ('approval-command', 'item/commandExecution/requestApproval', {'command': 'go test ./...'}, {'decision': 'acceptForSession'}),
    ('approval-file', 'item/fileChange/requestApproval', {'path': 'main.go'}, {'decision': 'acceptForSession'}),
    (
        'approval-permissions',
        'item/permissions/requestApproval',
        {'permissions': {'filesystem': {'write': True}, 'network': {'enabled': False}}},
        {'permissions': {'filesystem': {'write': True}, 'network': {'enabled': False}}},
    ),
]
pending = list(approval_methods)
for line in sys.stdin:
    log.write(line); log.flush()
    msg=json.loads(line)
    if msg.get('method') == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif msg.get('method') == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif msg.get('method') == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        req_id, method, params, expected = pending.pop(0)
        print(json.dumps({'jsonrpc': '2.0', 'id': req_id, 'method': method, 'params': params}), flush=True)
    elif msg.get('id', '').startswith('approval-'):
        req_id = msg['id']
        expected = next(item[3] for item in approval_methods if item[0] == req_id)
        if msg.get('result') != expected:
            print(json.dumps({'method': 'turn/failed', 'params': {'reason': 'unexpected approval response', 'got': msg.get('result'), 'want': expected}}), flush=True)
            break
        if pending:
            req_id, method, params, _expected = pending.pop(0)
            print(json.dumps({'jsonrpc': '2.0', 'id': req_id, 'method': method, 'params': params}), flush=True)
        else:
            print(json.dumps({'method': 'turn/completed', 'params': {'lastAssistantMessage': 'approvals handled'}}), flush=True)
            break
    elif msg.get('method') == 'initialized':
        pass
`)
	wd := codexWorkdir(t, "x")
	in := appServerInput(wd)
	in.Workflow.Config.Codex.ApprovalPolicy = "on-failure"

	res, err := (CodexAppServerRunner{}).Run(context.Background(), in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Summary != "approvals handled" {
		t.Fatalf("Summary = %q, want approvals handled", res.Summary)
	}
	events := runtimeEventsNamed(res.RuntimeEvents, task.EventApprovalAutoApproved)
	if len(events) != 3 {
		t.Fatalf("approval_auto_approved events = %d, want 3; events=%#v", len(events), res.RuntimeEvents)
	}
	stdin, err := os.ReadFile(filepath.Join(binDir, "stdin.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"decision":"acceptForSession"`, `"permissions":{"filesystem":{"write":true},"network":{"enabled":false}}`} {
		if !strings.Contains(string(stdin), want) {
			t.Fatalf("stdin missing %s in approval responses: %s", want, stdin)
		}
	}
}

// TestCodexAppServerRunnerTranslatesLegacyRejectPolicyForApprovalDecisions
// pins the boundary contract from #335: the approval policy is translated to
// codex's `granular` wire shape exactly once (codexWireApprovalPolicy) and the
// SAME translated value drives both the wire payload and the harness-side
// auto-approve decision (autoApproveRequest).
//
// The config below is the obsolete reject:{...} shape with every flag false —
// legacy operator intent "don't reject, allow these approval flows". Codex
// receives the translated granular:{...true} (so it emits approval prompts),
// and autoApproveRequest — which no longer special-cases reject: since #334 —
// only produces the right ALLOW decision if it is fed the translated value.
// If c.approvalPolicy were instead the raw reject:{...} map, autoApproveRequest
// would fall through to its safe-fallback decline, the runner would emit
// turn_input_required + InputRequiredError, and Run would error here.
func TestCodexAppServerRunnerTranslatesLegacyRejectPolicyForApprovalDecisions(t *testing.T) {
	binDir := codexAppServerStubScript(t, `
import json
log=open(os.environ['CODEX_STDIN_LOG'], 'w')
for line in sys.stdin:
    log.write(line); log.flush()
    msg=json.loads(line)
    if msg.get('method') == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif msg.get('method') == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif msg.get('method') == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        print(json.dumps({'jsonrpc': '2.0', 'id': 'approval-command', 'method': 'item/commandExecution/requestApproval', 'params': {'command': 'go test ./...'}}), flush=True)
    elif msg.get('id') == 'approval-command':
        if msg.get('result') == {'decision': 'acceptForSession'}:
            print(json.dumps({'method': 'turn/completed', 'params': {'lastAssistantMessage': 'approvals handled'}}), flush=True)
        else:
            print(json.dumps({'method': 'turn/failed', 'params': {'reason': 'unexpected approval response', 'got': msg.get('result')}}), flush=True)
        break
`)
	wd := codexWorkdir(t, "x")
	in := appServerInput(wd)
	// Obsolete reject:{flag:false} == "don't reject; allow these prompts".
	in.Workflow.Config.Codex.ApprovalPolicy = map[string]any{"reject": map[string]any{"sandbox_approval": false, "rules": false, "mcp_elicitations": false}}

	res, err := (CodexAppServerRunner{}).Run(context.Background(), in)
	if err != nil {
		t.Fatalf("Run: %v (legacy reject:{false} must auto-approve, not decline)", err)
	}
	if res.Summary != "approvals handled" {
		t.Fatalf("Summary = %q, want approvals handled", res.Summary)
	}
	if events := runtimeEventsNamed(res.RuntimeEvents, task.EventApprovalAutoApproved); len(events) != 1 {
		t.Fatalf("approval_auto_approved events = %d, want 1; events=%#v", len(events), res.RuntimeEvents)
	}
	stdin, err := os.ReadFile(filepath.Join(binDir, "stdin.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stdin), `"decision":"acceptForSession"`) {
		t.Fatalf("stdin missing acceptForSession in approval response: %s", stdin)
	}
	// The translated granular payload — not the raw reject: shape — must be
	// what reached codex on the wire.
	if strings.Contains(string(stdin), `"reject"`) {
		t.Fatalf("raw reject: shape leaked onto the wire: %s", stdin)
	}
	if !strings.Contains(string(stdin), `"granular"`) {
		t.Fatalf("stdin missing translated granular policy on the wire: %s", stdin)
	}
}

// Replaced by TestCodexAppServerRunnerEmitsTurnInputRequiredOnDeclinedApproval:
// the previous TestCodexAppServerRunnerDeclinesApprovalsWhenPolicyRequiresReview
// pinned the SPEC §10.4-non-compliant silent-decline behavior (decline sent
// over the wire, no turn_input_required event, agent continues). #214 makes
// the runner emit turn_input_required and bail with InputRequiredError so the
// orchestrator can mark the issue blocked when an approval cannot be
// auto-approved.

func TestProtocolServerRequestResultLegacyApprovalMethodsUseProtocolDecisions(t *testing.T) {
	for _, tc := range []struct {
		name       string
		method     string
		policy     any
		wantResult map[string]any
	}{
		{name: "exec allow", method: "execCommandApproval", policy: "on-failure", wantResult: map[string]any{"decision": "allow"}},
		{name: "exec never auto-approves", method: "execCommandApproval", policy: "never", wantResult: map[string]any{"decision": "allow"}},
		{name: "apply allow", method: "applyPatchApproval", policy: "on-failure", wantResult: map[string]any{"decision": "allow"}},
		{name: "apply never auto-approves", method: "applyPatchApproval", policy: "never", wantResult: map[string]any{"decision": "allow"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := protocolServerRequestResult(tc.method, map[string]any{}, tc.policy)
			if !ok || !reflect.DeepEqual(got, tc.wantResult) {
				t.Fatalf("protocolServerRequestResult(%q) = %#v, %v; want %#v, true", tc.method, got, ok, tc.wantResult)
			}
		})
	}
}

func TestProtocolServerRequestResultApprovalPolicyMatrix(t *testing.T) {
	request := map[string]any{
		"params": map[string]any{
			"permissions": map[string]any{
				"filesystem": map[string]any{"write": true},
				"network":    map[string]any{"enabled": true},
			},
		},
	}
	cases := []struct {
		name               string
		policy             any
		wantModernDecision string
		wantPermissions    bool
	}{
		{name: "never", policy: "never", wantModernDecision: "acceptForSession", wantPermissions: true},
		{name: "on-request", policy: "on-request", wantModernDecision: "decline"},
		{name: "untrusted", policy: "untrusted", wantModernDecision: "decline"},
		{name: "on-failure", policy: "on-failure", wantModernDecision: "acceptForSession", wantPermissions: true},
		// `reject:` is the obsolete codex variant renamed to `granular:` in
		// PR #14516 (b7dba72db). Harness no longer special-cases it — the
		// codexWireApprovalPolicy boundary translates it to granular before
		// going to codex, so by the time protocolServerRequestResult sees a
		// payload it should never observe a raw reject:. The case below
		// pins the safe fallback: unknown shapes don't auto-approve.
		{name: "reject legacy map (unknown to harness)", policy: map[string]any{"reject": map[string]any{"sandbox_approval": true, "rules": true}}, wantModernDecision: "decline"},
		{name: "granular denies all disabled allowances", policy: map[string]any{"granular": map[string]any{"sandbox_approval": false, "rules": false, "request_permissions": false}}, wantModernDecision: "decline"},
		{name: "granular allows sandbox only", policy: map[string]any{"granular": map[string]any{"sandbox_approval": true, "rules": false, "request_permissions": false}}, wantModernDecision: "acceptForSession"},
		{name: "granular allows permissions only", policy: map[string]any{"granular": map[string]any{"sandbox_approval": false, "rules": false, "request_permissions": true}}, wantModernDecision: "decline", wantPermissions: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, method := range []string{"item/commandExecution/requestApproval", "item/fileChange/requestApproval"} {
				result, ok := protocolServerRequestResult(method, request, tc.policy)
				if !ok || result["decision"] != tc.wantModernDecision {
					t.Fatalf("%s result = %#v, %v; want decision %q", method, result, ok, tc.wantModernDecision)
				}
			}
			result, ok := protocolServerRequestResult("item/permissions/requestApproval", request, tc.policy)
			if !ok {
				t.Fatal("permissions approval was not handled")
			}
			permissions, _ := result["permissions"].(map[string]any)
			if got := len(permissions) > 0; got != tc.wantPermissions {
				t.Fatalf("permissions granted = %v with result %#v, want %v", got, result, tc.wantPermissions)
			}
		})
	}
	inputResult, ok := protocolServerRequestResult("item/tool/requestUserInput", map[string]any{
		"params": map[string]any{"questions": []any{map[string]any{"id": "q1"}}},
	}, "on-request")
	if !ok || inputResult["answers"] == nil {
		t.Fatalf("user input result = %#v, %v; want answers payload", inputResult, ok)
	}
	elicitationResult, ok := protocolServerRequestResult("mcpServer/elicitation/request", map[string]any{}, "never")
	if !ok || elicitationResult["action"] != "decline" {
		t.Fatalf("elicitation result = %#v, %v; want decline action", elicitationResult, ok)
	}
	if _, ok := elicitationResult["content"]; !ok || elicitationResult["content"] != nil {
		t.Fatalf("elicitation content = %#v, present %v; want explicit null content", elicitationResult["content"], ok)
	}
}

// TestCodexWireApprovalPolicy pins the translation contract between
// aiops-platform's ApprovalPolicy value and codex's `AskForApproval` wire
// format (codex-rs/protocol/src/protocol.rs `AskForApproval`).
//
// The codex enum is the source of truth: codex commit b7dba72db (PR #14516,
// 2026-03-12) renamed the `Reject` variant to `Granular` and inverted the
// field polarity (true = ALLOW, was true = REJECT). Sending the obsolete
// `{"reject": {...}}` payload to a current codex returns
// `-32600 unknown variant ` and breaks startup (#329).
//
// Future maintainers: do NOT "fix" this by reverting to `reject:` — the
// rename is a deliberate codex protocol change. Check
// codex-rs/protocol/src/protocol.rs `pub enum AskForApproval` first.
func TestCodexWireApprovalPolicy(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want any
	}{
		{
			name: "never string passes through",
			in:   "never",
			want: "never",
		},
		{
			name: "untrusted string passes through",
			in:   "untrusted",
			want: "untrusted",
		},
		{
			name: "on-failure string passes through",
			in:   "on-failure",
			want: "on-failure",
		},
		{
			name: "on-request string passes through",
			in:   "on-request",
			want: "on-request",
		},
		{
			name: "granular map passes through unchanged",
			in:   map[string]any{"granular": map[string]any{"sandbox_approval": true, "rules": false, "mcp_elicitations": true}},
			want: map[string]any{"granular": map[string]any{"sandbox_approval": true, "rules": false, "mcp_elicitations": true}},
		},
		{
			name: "legacy reject:{true} translates to granular:{false}",
			// Operator intent: reject sandbox+rules+mcp prompts.
			in: map[string]any{"reject": map[string]any{"sandbox_approval": true, "rules": true, "mcp_elicitations": true}},
			// Codex equivalent: granular flags ALL false (none allowed).
			want: map[string]any{"granular": map[string]any{"sandbox_approval": false, "rules": false, "mcp_elicitations": false}},
		},
		{
			name: "legacy reject:{false} translates to granular:{true}",
			// Operator intent: don't reject; allow sandbox approvals.
			in:   map[string]any{"reject": map[string]any{"sandbox_approval": false, "rules": true, "mcp_elicitations": true}},
			want: map[string]any{"granular": map[string]any{"sandbox_approval": true, "rules": false, "mcp_elicitations": false}},
		},
		{
			name: "unrecognized shape passes through so codex surfaces its own validation error",
			in:   map[string]any{"future_variant": map[string]any{"x": 1}},
			want: map[string]any{"future_variant": map[string]any{"x": 1}},
		},
		{
			name: "nil passes through",
			in:   nil,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := codexWireApprovalPolicy(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("codexWireApprovalPolicy(%#v) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

func TestCodexAppServerRunnerRepliesToUnknownServerRequestsWithMethodNotFound(t *testing.T) {
	binDir := codexAppServerStubScript(t, `
import json
log=open(os.environ['CODEX_STDIN_LOG'], 'w')
for line in sys.stdin:
    log.write(line); log.flush()
    msg=json.loads(line)
    if msg.get('method') == 'initialize':
        print(json.dumps({'id': msg['id'], 'result': {}}), flush=True)
    elif msg.get('method') == 'thread/start':
        print(json.dumps({'id': msg['id'], 'result': {'thread': {'id': 'thread-1'}}}), flush=True)
    elif msg.get('method') == 'turn/start':
        print(json.dumps({'id': msg['id'], 'result': {'turn': {'id': 'turn-1'}}}), flush=True)
        print(json.dumps({'jsonrpc': '2.0', 'id': 'unknown-1', 'method': 'server/needs-help', 'params': {'reason': 'need approval'}}), flush=True)
    elif msg.get('id') == 'unknown-1':
        err = msg.get('error', {})
        if err.get('code') != -32601 or 'server/needs-help' not in err.get('message', ''):
            print(json.dumps({'method': 'turn/failed', 'params': {'reason': 'unexpected unknown request response', 'got': msg}}), flush=True)
            break
        print(json.dumps({'method': 'turn/completed', 'params': {'lastAssistantMessage': 'unknown request rejected'}}), flush=True)
        break
    elif msg.get('method') == 'initialized':
        pass
`)
	wd := codexWorkdir(t, "x")

	res, err := (CodexAppServerRunner{}).Run(context.Background(), appServerInput(wd))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Summary != "unknown request rejected" {
		t.Fatalf("Summary = %q, want unknown request rejected", res.Summary)
	}
	stdin, err := os.ReadFile(filepath.Join(binDir, "stdin.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stdin), `"code":-32601`) || strings.Contains(string(stdin), `"success":false`) {
		t.Fatalf("stdin did not include JSON-RPC method-not-found error: %s", stdin)
	}
}

// TestAppServerClient_ReadLineRejectsOversizedLine drives readLine directly
// with a synthesized stdout stream where a single line exceeds the 10 MiB
// SPEC §10.1 ceiling. The Scanner-bounded reader must surface the cap as a
// wrapped bufio.ErrTooLong instead of growing the buffer until OOM.
func TestAppServerClient_ReadLineRejectsOversizedLine(t *testing.T) {
	payload := bytes.Repeat([]byte{'x'}, maxAppServerLineBytes+1)
	payload = append(payload, '\n')

	sc := bufio.NewScanner(bytes.NewReader(payload))
	sc.Buffer(make([]byte, 0, appServerScannerInitialBuf), maxAppServerLineBytes+1)

	c := &appServerClient{scanner: sc}
	_, err := c.readLine(context.Background())
	if err == nil {
		t.Fatal("readLine err = nil, want over-cap error")
	}
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("readLine err = %v, want errors.Is(err, bufio.ErrTooLong)", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(maxAppServerLineBytes)) {
		t.Fatalf("readLine err = %q, want cap value %d to appear in message", err.Error(), maxAppServerLineBytes)
	}
}

func TestAppServerClient_ReadLineAcceptsLineAtCap(t *testing.T) {
	// A line of exactly maxAppServerLineBytes — the documented limit — must
	// pass; only strictly-larger lines should be rejected.
	payload := bytes.Repeat([]byte{'x'}, maxAppServerLineBytes)
	payload = append(payload, '\n')

	sc := bufio.NewScanner(bytes.NewReader(payload))
	sc.Buffer(make([]byte, 0, appServerScannerInitialBuf), maxAppServerLineBytes+1)

	c := &appServerClient{scanner: sc}
	line, err := c.readLine(context.Background())
	if err != nil {
		t.Fatalf("readLine at cap err = %v, want nil (10 MiB is the documented inclusive limit)", err)
	}
	if got, want := len(line), maxAppServerLineBytes; got != want {
		t.Fatalf("readLine line len = %d, want %d", got, want)
	}
}

// TestAppServerTurnTitle_PrefersIssueIdentifier pins SPEC §10.2's
// "<issue.identifier>: <issue.title>" pattern. Task.SourceEventID
// carries the tracker identifier ("AIOPS-64", "MT-649"); Task.ID is
// an internal queue nonce. The title must surface the identifier so
// operators can cross-reference a Codex session log to a Linear /
// Gitea ticket without translating through queue storage.
//
// Paired-edges across (identifier set, title set) × (identifier set,
// title set):
//   - both set → "<id>: <title>"
//   - identifier only → "<id>"
//   - title only → "<title>"
//   - neither (only Task.ID) → Task.ID as last-resort
func TestAppServerTurnTitle_PrefersIssueIdentifier(t *testing.T) {
	cases := []struct {
		name string
		in   RunInput
		want string
	}{
		{
			name: "identifier_and_title_set",
			in:   RunInput{Task: task.Task{ID: "tsk-001", SourceEventID: "AIOPS-64", Title: "Wire Codex app-server"}},
			want: "AIOPS-64: Wire Codex app-server",
		},
		{
			name: "identifier_only_set",
			in:   RunInput{Task: task.Task{ID: "tsk-002", SourceEventID: "AIOPS-65"}},
			want: "AIOPS-65",
		},
		{
			name: "title_only_set",
			in:   RunInput{Task: task.Task{ID: "tsk-003", Title: "Untracked title"}},
			want: "Untracked title",
		},
		{
			name: "neither_set_falls_back_to_task_id",
			in:   RunInput{Task: task.Task{ID: "tsk-004"}},
			want: "tsk-004",
		},
		{
			name: "whitespace_only_identifier_treated_as_unset",
			in:   RunInput{Task: task.Task{ID: "tsk-005", SourceEventID: "  ", Title: "Fallback title"}},
			want: "Fallback title",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := appServerTurnTitle(tc.in); got != tc.want {
				t.Fatalf("appServerTurnTitle = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAppServerContinuationPrompt_InheritsIdentifierSubject confirms
// the continuation-turn prompt subject also resolves through the
// identifier-first rule (since appServerContinuationPrompt delegates
// to appServerTurnTitle). The prompt template stays operator-readable
// when the identifier is the visible breadcrumb.
func TestAppServerContinuationPrompt_InheritsIdentifierSubject(t *testing.T) {
	in := RunInput{Task: task.Task{ID: "tsk-099", SourceEventID: "ENG-42", Title: "Mid-run continuation"}}
	got := appServerContinuationPrompt(in, 2)
	if !strings.Contains(got, "ENG-42: Mid-run continuation") {
		t.Fatalf("continuation prompt should carry identifier subject, got: %q", got)
	}
	if strings.Contains(got, "tsk-099") {
		t.Fatalf("continuation prompt should NOT include the internal task nonce; got: %q", got)
	}
}

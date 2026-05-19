package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func appServerInput(workdir string) RunInput {
	return RunInput{
		Task: task.Task{ID: "AIOPS-64", Title: "Wire Codex app-server", Model: "codex-app-server"},
		Workflow: workflow.Workflow{Config: workflow.Config{
			Agent: workflow.AgentConfig{MaxTurns: 8},
			Codex: workflow.CommandConfig{
				Command:        "codex app-server",
				ApprovalPolicy: "never",
				ThreadSandbox:  "workspace-write",
				TurnSandboxPolicy: map[string]any{
					"mode": "workspace-write",
				},
				TurnTimeoutMs:  1000,
				ReadTimeoutMs:  1000,
				StallTimeoutMs: 1000,
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
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CODEX_ARGV_LOG", filepath.Join(dir, "argv.txt"))
	t.Setenv("CODEX_STDIN_LOG", filepath.Join(dir, "stdin.txt"))
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

	res, err := (CodexAppServerRunner{}).Run(context.Background(), appServerInput(wd))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Summary != "done via app server" {
		t.Fatalf("Summary = %q, want app-server completion summary", res.Summary)
	}

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

	_, err := (CodexAppServerRunner{}).Run(context.Background(), appServerInput(wd))
	if err == nil || !strings.Contains(err.Error(), "turn/completed failed") || !strings.Contains(err.Error(), "tool crashed") {
		t.Fatalf("Run error = %v, want failed turn/completed error with reason", err)
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
	in.Workflow.Config.Codex.ReadTimeoutMs = 100
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

	cmd, err := buildCodexAppServerCmd(context.Background(), in)
	if err != nil {
		t.Fatalf("buildCodexAppServerCmd: %v", err)
	}
	if len(cmd.Args) < 2 || cmd.Args[0] != "codex" || cmd.Args[1] != "app-server" {
		t.Fatalf("cmd.Args = %#v, want codex app-server", cmd.Args)
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
	in.Workflow.Config.Codex.StallTimeoutMs = 50
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
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
        time.sleep(0.03)
        print(json.dumps({'method': 'turn/completed', 'params': {'lastAssistantMessage': 'completed after tool'}}), flush=True)
        break
`)
	wd := codexWorkdir(t, "x")
	in := appServerInput(wd)
	in.Workflow.Config.Codex.ReadTimeoutMs = 5000
	in.Workflow.Config.Codex.StallTimeoutMs = 50
	in.Workflow.Config.Tracker.Kind = "gitea"
	in.Workflow.Config.Tracker.APIKey = "token"
	in.Workflow.Config.Tracker.ProjectSlug = "http://127.0.0.1:1"
	in.Workflow.Config.Repo.Owner = "o"
	in.Workflow.Config.Repo.Name = "r"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
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
        time.sleep(0.03)
        print(json.dumps({'jsonrpc': '2.0', 'id': 99, 'method': 'server/needs-help', 'params': {}}), flush=True)
    elif msg.get('id') == 99:
        time.sleep(0.03)
        print(json.dumps({'method': 'turn/completed', 'params': {'lastAssistantMessage': 'completed after server request'}}), flush=True)
        break
`)
	wd := codexWorkdir(t, "x")
	in := appServerInput(wd)
	in.Workflow.Config.Codex.ReadTimeoutMs = 5000
	in.Workflow.Config.Codex.StallTimeoutMs = 50
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	res, err := (CodexAppServerRunner{}).Run(ctx, in)
	if err != nil {
		t.Fatalf("Run: %v, want server request to count as activity", err)
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

	res, err := (CodexAppServerRunner{}).Run(context.Background(), appServerInput(wd))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Summary != "approvals handled" {
		t.Fatalf("Summary = %q, want approvals handled", res.Summary)
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

func TestCodexAppServerRunnerRepliesToUnsupportedServerRequests(t *testing.T) {
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
        print(json.dumps({'jsonrpc': '2.0', 'id': 'approval-1', 'method': 'approval/request', 'params': {'reason': 'need approval'}}), flush=True)
    elif msg.get('id') == 'approval-1':
        result = msg.get('result', {})
        if result.get('success') is not False or 'unsupported server request' not in result.get('output', ''):
            print(json.dumps({'method': 'turn/failed', 'params': {'reason': 'unexpected unsupported request response', 'got': result}}), flush=True)
            break
        print(json.dumps({'method': 'turn/completed', 'params': {'lastAssistantMessage': 'request rejected'}}), flush=True)
        break
    elif msg.get('method') == 'initialized':
        pass
`)
	wd := codexWorkdir(t, "x")

	res, err := (CodexAppServerRunner{}).Run(context.Background(), appServerInput(wd))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Summary != "request rejected" {
		t.Fatalf("Summary = %q, want request rejected", res.Summary)
	}
	stdin, err := os.ReadFile(filepath.Join(binDir, "stdin.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stdin), `"id":"approval-1"`) || !strings.Contains(string(stdin), "unsupported server request") {
		t.Fatalf("stdin did not include unsupported server request response: %s", stdin)
	}
}

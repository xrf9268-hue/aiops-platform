package runner

import (
	"context"
	"encoding/json"
	"net/http/httptest"
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
	in.Workflow.Config.Codex.StallTimeoutMs = 1000
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
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
	in := appServerInput(wd)
	in.Workflow.Config.Codex.ApprovalPolicy = "on-failure"

	res, err := (CodexAppServerRunner{}).Run(context.Background(), in)
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

func TestCodexAppServerRunnerDeclinesApprovalsWhenPolicyRequiresReview(t *testing.T) {
	binDir := codexAppServerStubScript(t, `
import json
log=open(os.environ['CODEX_STDIN_LOG'], 'w')
approval_methods = [
    ('approval-command', 'item/commandExecution/requestApproval', {'command': 'go test ./...'}, {'decision': 'decline'}),
    ('approval-file', 'item/fileChange/requestApproval', {'path': 'main.go'}, {'decision': 'decline'}),
    (
        'approval-permissions',
        'item/permissions/requestApproval',
        {'permissions': {'filesystem': {'write': True}, 'network': {'enabled': True}}},
        {'permissions': {}},
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
        req_id, method, params, _expected = pending.pop(0)
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
            print(json.dumps({'method': 'turn/completed', 'params': {'lastAssistantMessage': 'review policy enforced'}}), flush=True)
            break
    elif msg.get('method') == 'initialized':
        pass
`)
	wd := codexWorkdir(t, "x")
	in := appServerInput(wd)
	in.Workflow.Config.Codex.ApprovalPolicy = map[string]any{
		"reject": map[string]any{
			"sandbox_approval": true,
			"rules":            true,
		},
	}

	res, err := (CodexAppServerRunner{}).Run(context.Background(), in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Summary != "review policy enforced" {
		t.Fatalf("Summary = %q, want review policy enforced", res.Summary)
	}
	stdin, err := os.ReadFile(filepath.Join(binDir, "stdin.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"decision":"decline"`, `"permissions":{}`} {
		if !strings.Contains(string(stdin), want) {
			t.Fatalf("stdin missing %s in policy-limited approval responses: %s", want, stdin)
		}
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
		{name: "never", policy: "never", wantModernDecision: "decline"},
		{name: "on-request", policy: "on-request", wantModernDecision: "decline"},
		{name: "untrusted", policy: "untrusted", wantModernDecision: "decline"},
		{name: "on-failure", policy: "on-failure", wantModernDecision: "acceptForSession", wantPermissions: true},
		{name: "reject legacy map", policy: map[string]any{"reject": map[string]any{"sandbox_approval": true, "rules": true}}, wantModernDecision: "decline"},
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

package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// CodexAppServerRunner talks to `codex app-server` over JSON-RPC 2.0 stdio.
// It is intentionally separate from CodexRunner so the existing one-shot
// `codex exec` path remains backwards-compatible while the app-server transport
// can grow toward the Symphony protocol.
type CodexAppServerRunner struct{}

const (
	codexAppServerOutputPath = ".aiops/CODEX_APP_SERVER_OUTPUT.txt"
)

func (CodexAppServerRunner) Run(ctx context.Context, in RunInput) (Result, error) {
	promptAbs := filepath.Join(in.Workdir, PromptPath)
	prompt, err := os.ReadFile(promptAbs)
	if err != nil {
		return Result{}, fmt.Errorf("read %s: %w", PromptPath, err)
	}

	cmd, err := buildCodexAppServerCmd(ctx, in)
	if err != nil {
		return Result{}, err
	}
	cmd.Dir = in.Workdir
	configurePlatformKill(cmd)
	cmd.WaitDelay = killGrace

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Result{}, fmt.Errorf("open codex app-server stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("open codex app-server stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, fmt.Errorf("open codex app-server stderr: %w", err)
	}

	buf := &cappedWriter{Cap: CodexOutputCap}
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		_, _ = io.Copy(buf, stderr)
	}()

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{}, err
	}

	client := &appServerClient{
		stdin:  stdin,
		reader: bufio.NewReader(stdout),
		out:    buf,
	}
	runErr := client.run(ctx, in, string(prompt))
	_ = stdin.Close()
	waitErr := cmd.Wait()
	<-stderrDone
	elapsed := time.Since(start)

	writeAppServerArtifact(in.Workdir, buf)
	res := Result{
		Summary:       client.summary(),
		OutputBytes:   int64(len(buf.Bytes())),
		OutputDropped: buf.Dropped(),
	}
	head, tail := headTail(buf.Bytes(), CodexEventOutputCap)
	if len(head) > 0 {
		res.OutputHead = string(head)
	}
	res.OutputTail = tail

	if runErr != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return res, &TimeoutError{Timeout: deadlineBudget(ctx, start), Elapsed: elapsed, Cause: runErr}
		}
		return res, runErr
	}
	if waitErr != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return res, &TimeoutError{Timeout: deadlineBudget(ctx, start), Elapsed: elapsed, Cause: waitErr}
		}
		return res, waitErr
	}
	return res, nil
}

func buildCodexAppServerCmd(ctx context.Context, in RunInput) (*exec.Cmd, error) {
	command := strings.TrimSpace(in.Workflow.Config.Codex.Command)
	if command == "" {
		command = "codex app-server"
	}
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return nil, fmt.Errorf("codex app-server command is empty")
	}
	if fields[0] == "codex" {
		if _, err := exec.LookPath("codex"); err != nil {
			return nil, fmt.Errorf("codex binary not found in PATH; install codex CLI or set agent.default to claude/mock")
		}
		return exec.CommandContext(ctx, fields[0], fields[1:]...), nil
	}
	return exec.CommandContext(ctx, "sh", "-lc", command), nil
}

type appServerClient struct {
	stdin          io.Writer
	reader         *bufio.Reader
	out            io.Writer
	nextID         int
	threadID       string
	lastMessage    string
	continueRun    bool
	tools          DynamicToolSet
	readTimeoutMs  int
	stallTimeoutMs int
}

func (c *appServerClient) run(ctx context.Context, in RunInput, prompt string) error {
	c.nextID = 1
	c.continueRun = false
	c.tools = DynamicToolsForWorkflow(workflow.Workflow{Config: in.Workflow.Config})
	c.readTimeoutMs = in.Workflow.Config.Codex.ReadTimeoutMs
	c.stallTimeoutMs = in.Workflow.Config.Codex.StallTimeoutMs
	if _, err := c.request(ctx, "initialize", map[string]any{
		"capabilities": map[string]any{"experimentalApi": true},
		"clientInfo": map[string]any{
			"name":    "aiops-platform",
			"title":   "AIOps Platform",
			"version": "0.1.0",
		},
	}); err != nil {
		return fmt.Errorf("codex app-server initialize: %w", err)
	}
	if err := c.notify("initialized", map[string]any{}); err != nil {
		return err
	}

	threadResult, err := c.request(ctx, "thread/start", map[string]any{
		"approvalPolicy": in.Workflow.Config.Codex.ApprovalPolicy,
		"sandbox":        in.Workflow.Config.Codex.ThreadSandbox,
		"cwd":            in.Workdir,
		"dynamicTools":   appServerDynamicToolSpecs(in.Workflow.Config),
	})
	if err != nil {
		return fmt.Errorf("codex app-server thread/start: %w", err)
	}
	threadID, err := extractString(threadResult, "thread", "id")
	if err != nil {
		return fmt.Errorf("codex app-server thread/start: %w", err)
	}
	c.threadID = threadID

	maxTurns := in.Workflow.Config.Agent.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 20
	}
	for turn := 1; turn <= maxTurns; turn++ {
		input := []map[string]any(nil)
		if turn == 1 {
			input = []map[string]any{{
				"type": "text",
				"text": prompt,
			}}
		}
		turnResult, err := c.request(ctx, "turn/start", map[string]any{
			"threadId":       threadID,
			"input":          input,
			"cwd":            in.Workdir,
			"title":          appServerTurnTitle(in),
			"approvalPolicy": in.Workflow.Config.Codex.ApprovalPolicy,
			"sandboxPolicy":  in.Workflow.Config.Codex.TurnSandboxPolicy,
		})
		if err != nil {
			return fmt.Errorf("codex app-server turn/start: %w", err)
		}
		if _, err := extractString(turnResult, "turn", "id"); err != nil {
			return fmt.Errorf("codex app-server turn/start: %w", err)
		}
		c.continueRun = false
		if err := c.awaitTurnCompletion(ctx); err != nil {
			return err
		}
		if !c.continueRun {
			return nil
		}
	}
	return fmt.Errorf("codex app-server exceeded agent.max_turns=%d", maxTurns)
}

func (c *appServerClient) request(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	id := c.nextID
	c.nextID++
	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for {
		msg, err := c.readMessage(ctx)
		if err != nil {
			return nil, err
		}
		if gotID, ok := numberID(msg["id"]); ok && gotID == id {
			if e, ok := msg["error"]; ok {
				return nil, fmt.Errorf("rpc error: %v", e)
			}
			result, _ := msg["result"].(map[string]any)
			if result == nil {
				result = map[string]any{}
			}
			return result, nil
		}
		c.handleNotification(msg)
	}
}

func (c *appServerClient) notify(method string, params map[string]any) error {
	return c.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *appServerClient) send(msg map[string]any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = c.stdin.Write(b)
	return err
}

func (c *appServerClient) readMessage(ctx context.Context) (map[string]any, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	line, err := c.readLine(ctx)
	if err != nil {
		return nil, err
	}
	_, _ = c.out.Write(line)
	var msg map[string]any
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil, fmt.Errorf("decode codex app-server message: %w", err)
	}
	return msg, nil
}

func (c *appServerClient) readLine(ctx context.Context) ([]byte, error) {
	type readResult struct {
		line []byte
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		line, err := c.reader.ReadBytes('\n')
		ch <- readResult{line: line, err: err}
	}()

	var timeout <-chan time.Time
	if c.readTimeoutMs > 0 {
		timer := time.NewTimer(time.Duration(c.readTimeoutMs) * time.Millisecond)
		defer timer.Stop()
		timeout = timer.C
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timeout:
		return nil, fmt.Errorf("codex app-server read timeout after %dms", c.readTimeoutMs)
	case res := <-ch:
		if res.err != nil {
			return nil, res.err
		}
		return res.line, nil
	}
}

func (c *appServerClient) awaitTurnCompletion(ctx context.Context) error {
	for {
		msg, err := c.readMessage(ctx)
		if err != nil {
			return err
		}
		if method, _ := msg["method"].(string); method != "" {
			switch method {
			case "turn/completed":
				if err := completedTurnError(msg); err != nil {
					return err
				}
				c.handleNotification(msg)
				return nil
			case "turn/failed", "turn/cancelled":
				return fmt.Errorf("%s: %v", method, msg["params"])
			case "item/tool/call":
				if err := c.handleDynamicToolCall(ctx, msg); err != nil {
					return err
				}
			default:
				c.handleNotification(msg)
			}
		}
	}
}

func (c *appServerClient) handleNotification(msg map[string]any) {
	params, _ := msg["params"].(map[string]any)
	if v, ok := params["continue"].(bool); ok {
		c.continueRun = v
	}
	for _, key := range []string{"lastAssistantMessage", "last_message", "message", "summary"} {
		if v, _ := params[key].(string); strings.TrimSpace(v) != "" {
			c.lastMessage = strings.TrimSpace(v)
			return
		}
	}
}

func completedTurnError(msg map[string]any) error {
	params, _ := msg["params"].(map[string]any)
	status, _ := params["status"].(string)
	status = strings.ToLower(strings.TrimSpace(status))
	if status == "" || status == "completed" || status == "succeeded" || status == "success" {
		return nil
	}
	reason := ""
	for _, key := range []string{"reason", "error", "message"} {
		if v, _ := params[key].(string); strings.TrimSpace(v) != "" {
			reason = strings.TrimSpace(v)
			break
		}
	}
	if reason == "" {
		reason = fmt.Sprint(params)
	}
	return fmt.Errorf("turn/completed failed with status %q: %s", status, reason)
}

func (c *appServerClient) handleDynamicToolCall(ctx context.Context, msg map[string]any) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	params, _ := msg["params"].(map[string]any)
	name := appServerToolCallName(params)
	arguments := appServerToolCallArguments(params)
	result, err := dynamicToolResult(false, "unsupported dynamic tool: "+name)
	if err != nil {
		return err
	}
	if tool, ok := c.tools.Lookup(name); ok {
		call := ToolCall{}
		if err := json.Unmarshal(arguments, &call); err != nil {
			result, err = dynamicToolFailure(err.Error())
		} else {
			result, err = tool.Call(ctx, call)
			if err != nil {
				result, err = dynamicToolFailure(err.Error())
			}
		}
		if err != nil {
			return err
		}
	}
	var payload any
	if err := json.Unmarshal([]byte(result), &payload); err != nil {
		payload = map[string]any{"success": false, "output": result}
	}
	return c.send(map[string]any{"jsonrpc": "2.0", "id": msg["id"], "result": payload})
}

func appServerToolCallName(params map[string]any) string {
	for _, key := range []string{"tool", "name"} {
		if v, _ := params[key].(string); strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func appServerToolCallArguments(params map[string]any) json.RawMessage {
	if raw, ok := params["arguments"]; ok && raw != nil {
		b, err := json.Marshal(raw)
		if err == nil {
			return b
		}
	}
	return json.RawMessage(`{}`)
}

func (c *appServerClient) summary() string {
	if strings.TrimSpace(c.lastMessage) != "" {
		return strings.TrimSpace(c.lastMessage)
	}
	return "codex app-server completed"
}

func appServerDynamicToolSpecs(cfg workflow.Config) []map[string]any {
	toolSet := DynamicToolsForWorkflow(workflow.Workflow{Config: cfg})
	names := toolSet.Names()
	specs := make([]map[string]any, 0, len(names))
	for _, name := range names {
		tool, ok := toolSet.Lookup(name)
		if !ok {
			continue
		}
		specs = append(specs, map[string]any{"name": tool.Name, "description": tool.Description})
	}
	return specs
}

func appServerTurnTitle(in RunInput) string {
	if in.Task.ID != "" && in.Task.Title != "" {
		return in.Task.ID + ": " + in.Task.Title
	}
	if in.Task.Title != "" {
		return in.Task.Title
	}
	return in.Task.ID
}

func extractString(m map[string]any, outer, inner string) (string, error) {
	o, _ := m[outer].(map[string]any)
	if o == nil {
		return "", fmt.Errorf("missing %s", outer)
	}
	v, _ := o[inner].(string)
	if v == "" {
		return "", fmt.Errorf("missing %s.%s", outer, inner)
	}
	return v, nil
}

func numberID(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case int:
		return x, true
	default:
		return 0, false
	}
}

func writeAppServerArtifact(workdir string, buf *cappedWriter) {
	dir := filepath.Join(workdir, ".aiops")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	body := append([]byte{}, buf.Bytes()...)
	if buf.Dropped() > 0 {
		footer := fmt.Sprintf("\n...output truncated at %d bytes\n", CodexOutputCap)
		body = append(body, []byte(footer)...)
	}
	_ = os.WriteFile(filepath.Join(workdir, codexAppServerOutputPath), body, 0o644)
}

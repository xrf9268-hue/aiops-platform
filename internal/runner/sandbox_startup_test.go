package runner

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestClassifySandboxStartupFailure(t *testing.T) {
	bwrapErr := NewError(CategoryTurnFailed, "turn/failed: bwrap: setting up uid map: Permission denied", nil)
	cases := []struct {
		name    string
		err     error
		res     Result
		wantTag bool
	}{
		{
			name:    "uid map denial in turn reason",
			err:     bwrapErr,
			wantTag: true,
		},
		{
			name:    "gid-map denial only in captured output tail",
			err:     NewError(CategoryPortExit, "codex app-server process exited", errors.New("exit status 1")),
			res:     Result{OutputTail: "bwrap: setting up gid map: Permission denied\n"},
			wantTag: true,
		},
		{
			name:    "namespace denial in captured output head",
			err:     NewError(CategoryPortExit, "codex app-server process exited", errors.New("exit status 1")),
			res:     Result{OutputHead: "bwrap: No permissions to creating new namespace, likely because the kernel does not allow non-privileged user namespaces\n"},
			wantTag: true,
		},
		{
			name:    "bwrap netns denial (non uid-map variant)",
			err:     NewError(CategoryTurnFailed, "turn/failed: bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted", nil),
			wantTag: true,
		},
		{
			name:    "generic turn failure is left untagged",
			err:     NewError(CategoryTurnFailed, "turn/failed: tests failed", nil),
			res:     Result{OutputTail: "FAIL ./...\n"},
			wantTag: false,
		},
		{
			name:    "unrelated permission denied without bwrap prefix is not a sandbox failure",
			err:     NewError(CategoryTurnFailed, "turn/failed: open /etc/shadow: permission denied", nil),
			wantTag: false,
		},
		{
			name:    "bwrap mentioned without a denial token is not a sandbox failure",
			err:     NewError(CategoryTurnFailed, "turn/failed: edited internal/runner/sandbox.go to call bwrap: ok", nil),
			res:     Result{OutputTail: "grep -n 'bwrap:' sandbox.go\n"},
			wantTag: false,
		},
		{
			name:    "nil error stays nil",
			err:     nil,
			wantTag: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifySandboxStartupFailure(tc.err, tc.res)
			if IsSandboxStartup(got) != tc.wantTag {
				t.Fatalf("classifySandboxStartupFailure(%v) IsSandboxStartup = %v; want %v (got err %v)", tc.err, IsSandboxStartup(got), tc.wantTag, got)
			}
			if !tc.wantTag && !errors.Is(got, tc.err) {
				t.Fatalf("classifySandboxStartupFailure(%v) = %v; want the original error unchanged", tc.err, got)
			}
		})
	}
}

// More-specific outcomes must not be masked even when their message or output
// carries a sandbox signature: each keeps its own worker routing.
func TestClassifySandboxStartupFailure_DoesNotMaskSpecificOutcomes(t *testing.T) {
	signatureOutput := Result{OutputTail: "bwrap: setting up uid map: Permission denied\n"}
	cases := []struct {
		name string
		err  error
	}{
		{"timeout", &TimeoutError{Timeout: time.Second, Elapsed: time.Second, Cause: errors.New("bwrap: setting up uid map: Permission denied")}},
		{"stall", &StallError{Timeout: time.Second, Elapsed: time.Second}},
		{"read timeout", &ReadTimeoutError{Timeout: time.Second}},
		{"turn timeout", &TurnTimeoutError{Timeout: time.Second, Elapsed: time.Second}},
		{"input required", &InputRequiredError{Method: "session/requestInput"}},
		{"quota backoff", &QuotaBackoffError{Message: "usage limit"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifySandboxStartupFailure(tc.err, signatureOutput)
			if IsSandboxStartup(got) {
				t.Fatalf("classifySandboxStartupFailure(%T) was re-tagged as sandbox startup; want the original %T preserved", tc.err, tc.err)
			}
			if !errors.Is(got, tc.err) {
				t.Fatalf("classifySandboxStartupFailure(%T) = %v; want original error unchanged", tc.err, got)
			}
		})
	}
}

// TestCodexAppServerRunnerClassifiesBwrapTurnFailureAsSandboxStartup drives a
// real turn/failed carrying bwrap's uid-map denial through CodexAppServerRunner.Run
// and asserts it emerges as a *SandboxStartupError. This pins the load-bearing
// wiring (Run → classifyAppServerOutcome → classifySandboxStartupFailure) and the
// assumption that the bwrap denial reaches the classifier via the turn reason —
// the unit tests above bypass Run with a synthetic error.
func TestCodexAppServerRunnerClassifiesBwrapTurnFailureAsSandboxStartup(t *testing.T) {
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
        print(json.dumps({'method': 'turn/failed', 'params': {'reason': 'bwrap: setting up uid map: Permission denied'}}), flush=True)
        break
`)
	wd := codexWorkdir(t, "bwrap sandbox startup")

	_, err := (CodexAppServerRunner{}).Run(context.Background(), appServerInput(wd))
	if !IsSandboxStartup(err) {
		t.Fatalf("Run() err = %T %[1]v; want a *SandboxStartupError", err)
	}
}

func TestSandboxStartupError_Message(t *testing.T) {
	withDetail := &SandboxStartupError{Detail: "host denied the codex bwrap user namespace"}
	if got, want := withDetail.Error(), "agent sandbox failed to start: host denied the codex bwrap user namespace"; got != want {
		t.Fatalf("SandboxStartupError.Error() = %q; want %q", got, want)
	}
	if got, want := (&SandboxStartupError{}).Error(), "agent sandbox failed to start"; got != want {
		t.Fatalf("empty SandboxStartupError.Error() = %q; want %q", got, want)
	}
	if !IsSandboxStartup(withDetail) {
		t.Fatalf("IsSandboxStartup(%v) = false; want true", withDetail)
	}
	if IsSandboxStartup(context.Canceled) {
		t.Fatalf("IsSandboxStartup(context.Canceled) = true; want false")
	}
}

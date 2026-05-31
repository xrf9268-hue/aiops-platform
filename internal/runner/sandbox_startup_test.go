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
			name:    "denial only in captured output tail",
			err:     NewError(CategoryPortExit, "codex app-server process exited", errors.New("exit status 1")),
			res:     Result{OutputTail: "bwrap: setting up gid map: Permission denied\n"},
			wantTag: true,
		},
		{
			name:    "denial in captured output head",
			err:     NewError(CategoryPortExit, "codex app-server process exited", errors.New("exit status 1")),
			res:     Result{OutputHead: "creating new user namespace failed\n"},
			wantTag: true,
		},
		{
			name:    "generic turn failure is left untagged",
			err:     NewError(CategoryTurnFailed, "turn/failed: tests failed", nil),
			res:     Result{OutputTail: "FAIL ./...\n"},
			wantTag: false,
		},
		{
			name:    "unrelated permission denied is not a sandbox failure",
			err:     NewError(CategoryTurnFailed, "turn/failed: open /etc/shadow: permission denied", nil),
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

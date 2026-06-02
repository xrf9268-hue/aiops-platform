package workspace

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func TestRedactCredentials(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "git clone failure line with user:token",
			in:   "fatal: unable to access 'https://git:s3cr3t@example.com/owner/repo.git/': boom",
			want: "fatal: unable to access 'https://example.com/owner/repo.git/': boom",
		},
		{
			name: "bare token userinfo",
			in:   "remote: https://x-access-token:ghp_abc123@github.com/o/r.git",
			want: "remote: https://github.com/o/r.git",
		},
		{
			name: "ssh scp-style is left untouched",
			in:   "git@example.com:owner/repo.git",
			want: "git@example.com:owner/repo.git",
		},
		{
			name: "at-sign in path is not treated as userinfo",
			in:   "https://example.com/a@b/c",
			want: "https://example.com/a@b/c",
		},
		{
			name: "no url",
			in:   "fatal: repository not found",
			want: "fatal: repository not found",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactCredentials(tc.in); got != tc.want {
				t.Errorf("redactCredentials(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCredentialRedactingWriter_ScrubsAcrossWriteBoundary(t *testing.T) {
	const secret = "s3cr3t-token"
	var sink bytes.Buffer
	w := &credentialRedactingWriter{w: &sink}

	// Split the credentialed URL across two writes with no intervening newline
	// to exercise the partial-line buffering path.
	if _, err := io.WriteString(w, "fatal: unable to access 'https://git:"); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	if _, err := io.WriteString(w, secret+"@example.com/o/r.git/'\n"); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	got := sink.String()
	if strings.Contains(got, secret) {
		t.Errorf("redacted output %q leaked secret %q", got, secret)
	}
	if strings.Contains(got, "git:"+secret+"@") {
		t.Errorf("redacted output %q leaked userinfo", got)
	}
	if !strings.Contains(got, "https://example.com/o/r.git") {
		t.Errorf("redacted output %q dropped the masked host/path", got)
	}
}

func TestCredentialRedactingWriter_FlushesUnterminatedLine(t *testing.T) {
	const secret = "tok-9999"
	var sink bytes.Buffer
	w := &credentialRedactingWriter{w: &sink}

	// No trailing newline: the credential only reaches the sink via Flush.
	if _, err := io.WriteString(w, "cloning https://u:"+secret+"@host/x.git"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := sink.String(); got != "" {
		t.Fatalf("partial line leaked before flush: %q", got)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	got := sink.String()
	if strings.Contains(got, secret) {
		t.Errorf("flushed output %q leaked secret %q", got, secret)
	}
	if !strings.Contains(got, "https://host/x.git") {
		t.Errorf("flushed output %q dropped masked URL", got)
	}
}

// TestEnsureMirror_FailedCloneMasksCredentials is the #595 regression: a clone
// against a credentialed clone URL that fails must leak the `user:token@`
// userinfo neither into the returned error string (vector 1, MaskCloneURL) nor
// into the forwarded git stderr (vector 2, runGitRedacted). 127.0.0.1:1 refuses
// the connection immediately, so the clone fails fast without network access.
func TestEnsureMirror_FailedCloneMasksCredentials(t *testing.T) {
	const secret = "s3cr3t-token-595"
	cloneURL := "https://git:" + secret + "@127.0.0.1:1/owner/repo.git"

	// Redirect os.Stderr so runGitRedacted's forwarded output is captured. The
	// credentialRedactingWriter reads os.Stderr at call time, so reassigning it
	// before EnsureMirror is enough.
	origStderr := os.Stderr
	r, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = pw
	captured := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		captured <- buf.String()
	}()

	mgr := &Manager{Root: t.TempDir(), MirrorRoot: t.TempDir()}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, cloneErr := mgr.EnsureMirror(ctx, cloneURL)

	_ = pw.Close()
	os.Stderr = origStderr
	stderr := <-captured

	if cloneErr == nil {
		t.Fatalf("EnsureMirror(%q) = nil error; want clone failure", workflow.MaskCloneURL(cloneURL))
	}
	errStr := cloneErr.Error()

	leaks := []string{secret, "git:" + secret + "@", "git:" + secret}
	for _, leak := range leaks {
		if strings.Contains(errStr, leak) {
			t.Errorf("error string %q leaked secret fragment %q", errStr, leak)
		}
		if strings.Contains(stderr, leak) {
			t.Errorf("forwarded stderr %q leaked secret fragment %q", stderr, leak)
		}
	}
	if !strings.Contains(errStr, "127.0.0.1:1/owner/repo.git") {
		t.Errorf("error string %q missing masked clone URL; want host/path present", errStr)
	}
}

package workspace

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// TestRedactTruncated pins the byte-cap edge from the #1032 review (P1): a
// cappedBuffer can cut through `user:token@` before the `@`, leaving a
// trailing authority fragment credentialURLRe cannot match. redactTruncated
// must fail closed on that tail — but only when the output was actually
// truncated, and only for an end-of-string authority fragment.
func TestRedactTruncated(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		truncated bool
		want      string
	}{
		{
			name:      "cap cut before the at-sign drops the partial authority",
			in:        "origin https://bot:hunter2tok",
			truncated: true,
			want:      "origin https://",
		},
		{
			name:      "cap cut inside the host after scrub drops the partial host",
			in:        "origin https://bot:tok@git.exa",
			truncated: true,
			want:      "origin https://",
		},
		{
			name:      "truncated tail inside a path is preserved",
			in:        "fetch https://example.com/some/pa",
			truncated: true,
			want:      "fetch https://example.com/some/pa",
		},
		{
			name:      "complete output keeps terminal host names",
			in:        "origin https://bot:tok@example.com/r.git and https://example.com",
			truncated: false,
			want:      "origin https://example.com/r.git and https://example.com",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redactTruncated(tt.in, tt.truncated); got != tt.want {
				t.Errorf("redactTruncated(%q, %v) = %q; want %q", tt.in, tt.truncated, got, tt.want)
			}
		})
	}
}

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
			// Password containing '@' must be stripped up to the LAST '@' in the
			// authority, matching workflow.MaskCloneURL (url.Parse, RFC 3986).
			// Stopping at the first '@' would leak the password tail.
			name: "password containing at-sign is fully stripped",
			in:   "fatal: unable to access 'https://git:to@ken@example.com/o/r.git/'",
			want: "fatal: unable to access 'https://example.com/o/r.git/'",
		},
		{
			name: "at-sign in query is not treated as userinfo",
			in:   "https://u:p@host/path?x=a@b",
			want: "https://host/path?x=a@b",
		},
		{
			name: "two credentialed urls on one line",
			in:   "from https://a:b@h1/x to https://c:d@h2/y",
			want: "from https://h1/x to https://h2/y",
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

// TestEnsureMirror_FailedCloneMasksErrorString covers #595 vector 1: a failing
// clone against a credentialed clone URL must mask the `user:token@` userinfo in
// the returned error string. 127.0.0.1:1 refuses the connection immediately, so
// the clone fails fast without network access. (Vector 2 — git stderr
// forwarding — is covered deterministically by TestRunRedacted_ScrubsStderr,
// because git itself sanitises the URL in its own messages on some versions, so
// a clone-driven stderr assertion would be a placebo.)
func TestEnsureMirror_FailedCloneMasksErrorString(t *testing.T) {
	const secret = "s3cr3t-token-595"
	cloneURL := "https://git:" + secret + "@127.0.0.1:1/owner/repo.git"

	mgr := &Manager{Root: t.TempDir(), MirrorRoot: t.TempDir()}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, cloneErr := mgr.EnsureMirror(ctx, cloneURL)

	if cloneErr == nil {
		t.Fatalf("EnsureMirror(%q) = nil error; want clone failure", workflow.MaskCloneURL(cloneURL))
	}
	errStr := cloneErr.Error()

	for _, leak := range []string{secret, "git:" + secret + "@", "git:" + secret} {
		if strings.Contains(errStr, leak) {
			t.Errorf("error string %q leaked secret fragment %q", errStr, leak)
		}
	}
	if !strings.Contains(errStr, "127.0.0.1:1/owner/repo.git") {
		t.Errorf("error string %q missing masked clone URL; want host/path present", errStr)
	}
}

// TestRunRedacted_ScrubsStderr covers #595 vector 2 end-to-end: runRedacted must
// strip basic-auth userinfo from a subprocess's stderr before it reaches
// os.Stderr. It feeds a command that prints a real `user:token@` URL to stderr
// (git's own messages sanitise the URL on some versions, which would make a
// git-driven assertion a placebo), then asserts the captured os.Stderr contains
// the masked URL and none of the secret. Reverting runRedacted to forward raw
// os.Stderr makes this fail.
//
// It reassigns the process-global os.Stderr, so it must never run in parallel
// with another test in this package — do not add t.Parallel here.
func TestRunRedacted_ScrubsStderr(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	const secret = "tok-vector2-595"
	url := "https://git:" + secret + "@example.com/owner/repo.git"

	origStderr := os.Stderr
	r, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = pw
	defer func() { os.Stderr = origStderr }()
	captured := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		captured <- buf.String()
	}()

	// $0 carries the line so the credentialed URL needs no shell quoting.
	line := "fatal: unable to access '" + url + "'\n"
	cmd := exec.Command("sh", "-c", `printf '%s' "$0" 1>&2; exit 3`, line)
	runErr := runRedacted(cmd)

	_ = pw.Close()
	os.Stderr = origStderr
	stderr := <-captured

	if runErr == nil {
		t.Fatalf("runRedacted(printf...; exit 3) = nil error; want non-nil exit")
	}
	for _, leak := range []string{secret, "git:" + secret + "@", "git:" + secret} {
		if strings.Contains(stderr, leak) {
			t.Errorf("forwarded stderr %q leaked secret fragment %q", stderr, leak)
		}
	}
	if !strings.Contains(stderr, "https://example.com/owner/repo.git") {
		t.Errorf("forwarded stderr %q missing masked URL; want host/path present", stderr)
	}
}

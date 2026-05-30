package runner

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// recordingCloser counts Close calls so closeOnError's two branches can be
// asserted directly.
type recordingCloser struct{ closed int }

func (r *recordingCloser) Close() error {
	r.closed++
	return nil
}

func TestCloseOnError(t *testing.T) {
	t.Run("closes when err is set", func(t *testing.T) {
		rc := &recordingCloser{}
		err := error(errors.New("boom"))
		closeOnError(&err, rc)
		if rc.closed != 1 {
			t.Fatalf("closeOnError(*errp=%v) closed = %d; want 1", err, rc.closed)
		}
	})
	t.Run("no-op when err is nil", func(t *testing.T) {
		rc := &recordingCloser{}
		var err error
		closeOnError(&err, rc)
		if rc.closed != 0 {
			t.Fatalf("closeOnError(*errp=nil) closed = %d; want 0", rc.closed)
		}
	})
}

// TestOpenAppServerPipesClosesStdinOnLaterPipeFailure proves openAppServerPipes
// closes the already-opened stdin pipe when a later pipe fails (#507 item 2).
// Presetting cmd.Stdout forces StdoutPipe to fail after StdinPipe succeeded.
// StdinPipe stored the pipe's read end on cmd.Stdin; if the returned write end
// was closed on the failure, reading that read end returns io.EOF instead of
// blocking forever on the leaked fd.
func TestOpenAppServerPipesClosesStdinOnLaterPipeFailure(t *testing.T) {
	cmd := exec.Command("codex-app-server-never-started")
	cmd.Stdout = &bytes.Buffer{} // makes StdoutPipe fail: "exec: Stdout already set"

	stdin, stdout, stderr, err := openAppServerPipes(cmd)
	if err == nil || !strings.Contains(err.Error(), "stdout") {
		t.Fatalf("openAppServerPipes(stdout-preset) err = %v; want a wrapped stdout error", err)
	}
	if stdin != nil || stdout != nil || stderr != nil {
		t.Fatalf("openAppServerPipes(err) returned non-nil pipe: stdin=%v stdout=%v stderr=%v; want all nil", stdin, stdout, stderr)
	}

	pr, ok := cmd.Stdin.(*os.File)
	if !ok {
		t.Fatalf("cmd.Stdin = %T; want *os.File stored by StdinPipe", cmd.Stdin)
	}
	done := make(chan error, 1)
	go func() {
		_, readErr := pr.Read(make([]byte, 1))
		done <- readErr
	}()
	select {
	case readErr := <-done:
		if !errors.Is(readErr, io.EOF) {
			t.Fatalf("read cmd.Stdin = %v; want io.EOF (write end closed on later-pipe failure)", readErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read cmd.Stdin blocked; stdin write end was not closed on later-pipe failure (fd leak)")
	}
}

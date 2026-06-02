package workspace

import (
	"bytes"
	"io"
	"regexp"
	"sync"
)

// credentialURLRe matches the basic-auth `userinfo@` segment of a URL embedded
// in arbitrary text — e.g. git's `fatal: unable to access
// 'https://user:token@host/...'` stderr line. It captures the scheme through
// `://` so redactCredentials can drop the userinfo while keeping the scheme and
// host. This is the free-text analogue of workflow.MaskCloneURL: that helper
// accepts a single well-formed URL string, whereas git interleaves the clone
// URL with surrounding prose on one line, so a substring match is required
// here. The userinfo class excludes `/` so a `@` in a path/query (e.g.
// `https://host/a@b`) is never mistaken for basic-auth userinfo.
var credentialURLRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^/@\s]+@`)

// redactCredentials strips basic-auth userinfo from every URL embedded in s so a
// clone_url's `user:token@` (the agent's push credential) never reaches a log
// or error string — AGENTS.md secret-masking convention, #595.
func redactCredentials(s string) string {
	return credentialURLRe.ReplaceAllString(s, "$1")
}

// credentialRedactingWriter forwards writes to w with basic-auth userinfo
// stripped from any embedded URL. It buffers a trailing partial line so a clone
// URL split across two Write calls is still redacted before it reaches w, and
// Flush emits the final unterminated line. A URL never spans a newline, so
// line-buffering is sufficient to catch every embedded credential. It is
// concurrency-safe because git wires a command's stdout and stderr to the same
// writer.
type credentialRedactingWriter struct {
	w   io.Writer
	mu  sync.Mutex
	buf []byte
}

func (c *credentialRedactingWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf = append(c.buf, p...)
	for {
		i := bytes.IndexByte(c.buf, '\n')
		if i < 0 {
			break
		}
		line := c.buf[:i+1]
		if _, err := io.WriteString(c.w, redactCredentials(string(line))); err != nil {
			return len(p), err
		}
		c.buf = c.buf[i+1:]
	}
	return len(p), nil
}

// Flush writes any buffered partial line (with credentials redacted) to the
// underlying writer. Callers must call it once the wrapped command has exited.
func (c *credentialRedactingWriter) Flush() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.buf) == 0 {
		return nil
	}
	_, err := io.WriteString(c.w, redactCredentials(string(c.buf)))
	c.buf = nil
	return err
}

// Compile-time check that credentialRedactingWriter satisfies io.Writer.
var _ io.Writer = (*credentialRedactingWriter)(nil)

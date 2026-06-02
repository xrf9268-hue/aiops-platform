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
// host. This is the free-text analogue of workflow.MaskCloneURL and must agree
// with its splitting rule: MaskCloneURL strips userinfo up to the *last* `@`
// (url.Parse, RFC 3986), so a password that itself contains `@` is fully
// removed. The userinfo class `[^/?#\s]*` includes `@` and is greedy, so the
// trailing `@` matches the last `@` in the authority (before the first
// `/`?`#`/whitespace that ends it) — matching MaskCloneURL rather than stopping
// at the first `@` and leaking the password tail. A `@` in a path/query (after
// the authority delimiter) is therefore never mistaken for basic-auth userinfo.
var credentialURLRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^/?#\s]*@`)

// redactCredentials strips basic-auth userinfo from every URL embedded in s so a
// clone_url's `user:token@` (the agent's push credential) never reaches a log
// or error string — AGENTS.md secret-masking convention, #595.
func redactCredentials(s string) string {
	return credentialURLRe.ReplaceAllString(s, "$1")
}

// credentialRedactingWriter forwards writes to w with basic-auth userinfo
// stripped from any embedded URL. It buffers a trailing partial segment so a
// clone URL split across two Write calls is still redacted before it reaches w,
// flushing each segment terminated by `\n` or `\r`; the `\r` boundary keeps git
// progress (`Receiving objects: …\r`) streaming and bounds buffer growth. A URL
// contains neither `\n` nor `\r`, so segment-flushing never splits a credential.
// Flush emits the final unterminated segment.
//
// The mutex guards this instance's buffer against os/exec's per-stream copy
// goroutine (one goroutine writes here while Flush may run from the caller).
// runGitRedacted gives stdout and stderr separate instances, so the lock is
// per-instance, not shared across the two streams.
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
		i := bytes.IndexAny(c.buf, "\n\r")
		if i < 0 {
			break
		}
		segment := c.buf[:i+1]
		if _, err := io.WriteString(c.w, redactCredentials(string(segment))); err != nil {
			return len(p), err
		}
		c.buf = c.buf[i+1:]
	}
	return len(p), nil
}

// Flush writes any buffered partial segment (with credentials redacted) to the
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

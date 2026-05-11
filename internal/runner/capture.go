package runner

// CodexOutputCap is the upper bound on bytes the codex runner buffers in
// memory from a single run. Matches workspace.VerifyOutputCap so artifact
// ergonomics are uniform across the verify and runner phases.
const CodexOutputCap = 1 << 20 // 1 MiB

// CodexEventOutputCap bounds the head and tail slices embedded in the
// runner_end event payload. 4 KiB per side keeps the events table cheap
// to query while still surfacing enough context for triage.
const CodexEventOutputCap = 4 << 10 // 4 KiB

// cappedWriter is an io.Writer that buffers up to Cap bytes and silently
// drops the rest while remembering how many bytes were dropped. It is the
// runner-side twin of workspace.cappedBuffer; the duplication avoids an
// import cycle and keeps each package's IO contract local.
type cappedWriter struct {
	Cap     int
	buf     []byte
	dropped int64
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if c.Cap <= 0 {
		c.dropped += int64(len(p))
		return len(p), nil
	}
	remaining := c.Cap - len(c.buf)
	if remaining <= 0 {
		c.dropped += int64(len(p))
		return len(p), nil
	}
	take := len(p)
	if take > remaining {
		take = remaining
	}
	c.buf = append(c.buf, p[:take]...)
	if take < len(p) {
		c.dropped += int64(len(p) - take)
	}
	return len(p), nil
}

// Bytes returns the buffered bytes (post-cap). Callers must not mutate.
func (c *cappedWriter) Bytes() []byte { return c.buf }

// Dropped reports how many bytes were dropped because Cap was reached.
func (c *cappedWriter) Dropped() int64 { return c.dropped }

// headTail returns the first headCap bytes and (when body is longer than
// headCap) the last headCap bytes. tail is empty when the entire body
// fits in head, so callers do not duplicate content. headCap is applied
// to each side independently — total payload size is bounded by 2*headCap.
func headTail(body []byte, headCap int) (head []byte, tail string) {
	if headCap <= 0 || len(body) == 0 {
		return nil, ""
	}
	if len(body) <= headCap {
		return body, ""
	}
	head = body[:headCap]
	tail = string(body[len(body)-headCap:])
	return head, tail
}

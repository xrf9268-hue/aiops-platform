package tracker

import (
	"io"
	"net/http"
)

// drainBodyLimit bounds how many leftover response-body bytes DrainAndClose
// reads before closing. 8 KiB matches the githubSecondaryLimitBody probe
// bound: real tracker error payloads are far smaller, and a misbehaving
// server that streams forever must not be able to stall the poll loop just
// to save one connection.
const drainBodyLimit = 8 << 10

// DrainAndClose reads up to drainBodyLimit leftover bytes from resp's body
// before closing it. Go's HTTP/1.1 transport only returns a connection to
// the idle pool once the body has been read to EOF; closing an unread body
// tears the connection down, so every undrained non-2xx response costs a
// fresh TCP (+TLS) handshake exactly when the tracker is least healthy
// (#762). It composes with partial readers such as the GitHub
// secondary-rate-limit body probe (#761): whatever a classifier already
// consumed, this drains only the remainder. The Gitea tracker client reuses
// this helper rather than growing a parallel one (clean-code rule 3); it
// already depends on this package for its typed-error categories.
func DrainAndClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainBodyLimit))
	_ = resp.Body.Close()
}

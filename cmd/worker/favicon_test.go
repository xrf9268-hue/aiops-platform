package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/orchestrator"
)

// TestFaviconRouteServesEmbeddedDigestedPNG locks the Symphony#90-style favicon
// wiring: /favicon.png serves the committed embedded PNG (so it works in the
// un-built fallback case too), and the cache-bust digest the dashboard HTML
// advertises is sha256(bytes)[:12] — one source of truth, so the URL changes iff
// the bytes do.
func TestFaviconRouteServesEmbeddedDigestedPNG(t *testing.T) {
	server := newStateHTTPServer("127.0.0.1", 0, func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, nil
	})

	req := newLoopbackRequest(http.MethodGet, "http://127.0.0.1:4000/favicon.png?v="+faviconDigest(), nil)
	w := httptest.NewRecorder()
	server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /favicon.png status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("favicon content type = %q, want image/png", got)
	}
	body := w.Body.Bytes()
	if !bytes.HasPrefix(body, []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatalf("favicon body is not a PNG (first bytes %x)", body[:min(8, len(body))])
	}
	if !bytes.Equal(body, faviconPNG) {
		t.Fatalf("served favicon (%d bytes) != embedded faviconPNG (%d bytes)", len(body), len(faviconPNG))
	}
	sum := sha256.Sum256(body)
	if got, want := faviconDigest(), hex.EncodeToString(sum[:])[:12]; got != want {
		t.Fatalf("faviconDigest() = %q, want %q (sha256[:12] of served bytes)", got, want)
	}
}

// TestFaviconRouteRejectsNonGet keeps the favicon handler consistent with the
// other dashboard handlers' method guard.
func TestFaviconRouteRejectsNonGet(t *testing.T) {
	server := newStateHTTPServer("127.0.0.1", 0, func(context.Context) (orchestrator.StateView, error) {
		return orchestrator.StateView{}, nil
	})

	req := newLoopbackRequest(http.MethodPost, "http://127.0.0.1:4000/favicon.png", nil)
	w := httptest.NewRecorder()
	server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /favicon.png status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
	if got := w.Header().Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("Allow header = %q, want %q", got, "GET, HEAD")
	}
}

package main

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"log"
	"net/http"
	"sync"
	"time"
)

// dashboardDist contains the React + Vite build served by the worker status
// surface. Keep /api/v1/state as the canonical data source; the dashboard is
// an operator-facing client for that API, per SPEC §13.7.1.
//
//go:embed dashboard/dist dashboard/fallback.html
var dashboardDist embed.FS

// faviconPNG is the worker-status favicon: the v3 brand mark (italic "a" + the
// console's live caret) rasterized to a 128×128 PNG. It is committed under
// dashboard/public/ — not produced by the Vite build — so it embeds and serves
// identically whether or not the dashboard bundle was built, mirroring upstream
// Symphony's embedded static-asset favicon (openai/symphony#90).
//
//go:embed dashboard/public/favicon.png
var faviconPNG []byte

// faviconVersionPlaceholder is the token the dashboard HTML carries in its
// favicon <link>; dashboardHTML rewrites it to faviconDigest() at serve time so
// the cache-bust digest has no hand-maintained copy that can drift (clean-code
// rule 3: one source of truth — the PNG bytes).
const faviconVersionPlaceholder = "__FAVICON_V__"

// faviconDigest is the content digest used to cache-bust the favicon URL
// (`/favicon.png?v=<digest>`), per Symphony#90. It is the first 12 hex chars of
// the PNG's SHA-256, computed once from the embedded bytes, so changing the PNG
// changes the URL automatically.
var faviconDigest = sync.OnceValue(func() string {
	sum := sha256.Sum256(faviconPNG)
	return hex.EncodeToString(sum[:])[:12]
})

// dashboardHTML is the served status-page document with the favicon digest
// templated in. Resolved once: the embed is fixed at build time, so the chosen
// source (built index.html vs. committed fallback.html) and the digest never
// change across requests.
var dashboardHTML = sync.OnceValue(func() []byte {
	path := dashboardIndexPath()
	raw, err := fs.ReadFile(dashboardDist, path)
	if err != nil {
		log.Printf("read dashboard HTML %q: %v", path, err)
		return nil
	}
	return bytes.ReplaceAll(raw, []byte(faviconVersionPlaceholder), []byte(faviconDigest()))
})

func dashboardHTMLHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		doc := dashboardHTML()
		if len(doc) == 0 {
			// fallback.html is committed and always embedded, so an empty doc
			// means the embed itself is broken — surface it, don't serve blank.
			writeAPIError(w, http.StatusInternalServerError, "dashboard_unavailable", "dashboard html unavailable")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// The document carries the live favicon digest, so don't let a proxy or
		// browser pin a copy past a favicon swap; the assets it references are
		// content-addressed and cached aggressively on their own.
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(doc))
	})
}

func dashboardIndexPath() string {
	if f, err := dashboardDist.Open("dashboard/dist/index.html"); err == nil {
		_ = f.Close()
		return "dashboard/dist/index.html"
	}
	return "dashboard/fallback.html"
}

// faviconHandler serves the embedded favicon. The digest in the request's query
// string is the cache-bust (Symphony#90), so the bytes here are immutable for a
// given URL and safe to cache for a year.
func faviconHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("ETag", `"`+faviconDigest()+`"`)
		http.ServeContent(w, r, "favicon.png", time.Time{}, bytes.NewReader(faviconPNG))
	})
}

func dashboardAssetHandler() http.Handler {
	dist, err := fs.Sub(dashboardDist, "dashboard/dist")
	if err != nil {
		log.Printf("dashboard asset filesystem unavailable: %v", err)
		return http.NotFoundHandler()
	}
	return http.FileServer(http.FS(dist))
}

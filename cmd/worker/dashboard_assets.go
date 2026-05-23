package main

import (
	"embed"
	"io/fs"
	"log"
	"net/http"
)

// dashboardDist contains the React + Vite build served by the worker status
// surface. Keep /api/v1/state as the canonical data source; the dashboard is
// an operator-facing client for that API, per SPEC §13.7.1.
//
//go:embed dashboard/dist
var dashboardDist embed.FS

func dashboardHTMLHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.ServeFileFS(w, r, dashboardDist, "dashboard/dist/index.html")
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

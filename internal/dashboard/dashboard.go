// Package dashboard serves the embedded web UI (spec 3.8) on the dashboard
// port: static SPA plus a pass-through to the OpenAI-compatible API (so the
// chat playground works same-origin) and the admin JSON API.
package dashboard

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/lollm/lollm/internal/api"
)

//go:embed static
var staticFS embed.FS

// New builds the dashboard handler: the embedded UI at "/", the gateway API
// under /v1 (unchanged paths, normal API-key auth), and admin JSON under /api.
func New(version string, s *api.Server) http.Handler {
	apiHandler := s.Handler()
	adminHandler := s.AdminHandler()

	r := chi.NewRouter()
	r.Get("/", serveIndex(version))
	r.Get("/index.html", serveIndex(version))
	r.Get("/favicon.ico", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	r.Handle("/healthz", apiHandler)
	r.Handle("/v1/*", apiHandler)    // path reaches the API mux intact
	r.Handle("/api/*", adminHandler) // admin routes carry the /api prefix
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
	})
	return r
}

// serveIndex returns the SPA shell with the version injected.
func serveIndex(version string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := fs.ReadFile(staticFS, "static/index.html")
		if err != nil {
			http.Error(w, "dashboard UI missing", http.StatusInternalServerError)
			return
		}
		page := strings.Replace(string(b), "__VERSION__", version, 1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(page))
	}
}

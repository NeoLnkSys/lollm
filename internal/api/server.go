// Package api implements the OpenAI-compatible gateway HTTP API (spec 3.1):
// POST /v1/chat/completions (SSE + non-streaming), GET /v1/models, plus the
// internal auth, request-id, and fallback machinery around them.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/lollm/lollm/internal/config"
	"github.com/lollm/lollm/internal/db"
	"github.com/lollm/lollm/internal/health"
	"github.com/lollm/lollm/internal/providers"
	"github.com/lollm/lollm/internal/proxy"
	"github.com/lollm/lollm/internal/routing"
)

// Server carries the gateway's runtime dependencies.
type Server struct {
	cfg       *config.Config
	store     *db.Store
	masterKey []byte
	engine    *routing.Engine
	proxyMgr  *proxy.Manager
	adapters  map[string]providers.Adapter
	log       *slog.Logger
	version   string
	events    *health.Bus // optional; set via UseEventBus before Handler()

	sessions     *sessionStore // dashboard login cookie sessions
	loginLimiter *loginLimiter // per-IP failed-login throttling
}

// UseEventBus attaches the status event bus (health checker + dashboard).
func (s *Server) UseEventBus(b *health.Bus) { s.events = b }

// New builds the API server and its provider adapters.
func New(cfg *config.Config, store *db.Store, masterKey []byte, log *slog.Logger, version string) *Server {
	proxyMgr := proxy.NewManager(func(ctx context.Context, poolID string) ([]string, error) {
		pool, err := store.GetProxyPool(ctx, poolID)
		if err != nil {
			return nil, err
		}
		return pool.Proxies, nil
	})

	adapters := make(map[string]providers.Adapter, len(providers.Names()))
	for _, name := range providers.Names() {
		meta, _ := providers.Lookup(name)
		adapters[name] = providers.NewOpenAICompat(meta, masterKey, proxyMgr)
	}

	return &Server{
		cfg:          cfg,
		store:        store,
		masterKey:    masterKey,
		engine:       routing.NewEngine(),
		proxyMgr:     proxyMgr,
		adapters:     adapters,
		log:          log,
		version:      version,
		sessions:     newSessionStore(),
		loginLimiter: newLoginLimiter(),
	}
}

// adapterFor returns the adapter for a provider. Unknown providers get an
// ad-hoc "custom" adapter (the connection must carry its own base_url).
func (s *Server) adapterFor(provider string) providers.Adapter {
	if a, ok := s.adapters[provider]; ok {
		return a
	}
	meta, _ := providers.Lookup("custom")
	return providers.NewOpenAICompat(meta, s.masterKey, s.proxyMgr)
}

// Handler builds the HTTP routing tree.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(s.middlewareRequestID)
	r.Use(s.middlewareRecover)

	r.Get("/healthz", s.handleHealthz)

	r.Route("/v1", func(r chi.Router) {
		r.Use(s.middlewareAuth)
		r.Get("/models", s.handleModels)
		r.Post("/chat/completions", s.handleChatCompletions)
	})

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeOpenAIError(w, http.StatusNotFound, "unknown endpoint: "+r.Method+" "+r.URL.Path, "invalid_request_error", "not_found")
	})
	return r
}

// --- middleware ----------------------------------------------------------------

type ctxKey int

const (
	ctxRequestID ctxKey = iota
	ctxAPIKeyID
)

// RequestID returns the request id from the context.
func RequestID(ctx context.Context) string {
	v, _ := ctx.Value(ctxRequestID).(string)
	return v
}

func (s *Server) middlewareRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxRequestID, id)))
	})
}

func (s *Server) middlewareRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic recovered", "request_id", RequestID(r.Context()), "panic", rec)
				writeOpenAIError(w, http.StatusInternalServerError, "internal gateway error", "server_error", "internal_error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// --- health ---------------------------------------------------------------------

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": s.version,
		"time":    time.Now().UTC().Format(time.RFC3339),
	})
}

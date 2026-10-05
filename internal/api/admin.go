package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/lollm/lollm/internal/auth"
	"github.com/lollm/lollm/internal/backup"
	"github.com/lollm/lollm/internal/db"
	"github.com/lollm/lollm/internal/secret"
)

// productName is this release's display name (kept in sync with cli.Codename;
// the api package cannot import cli — import cycle).
const productName = "LoLLM Synapse"

// AdminHandler builds the dashboard's management JSON API (spec 3.8). It is
// mounted on the dashboard port only — the public API port stays a clean
// OpenAI-compatible surface. When a dashboard admin token is configured
// (settings: dashboard.admin_token_hash), every call must carry it as a
// Bearer token or X-LoLLM-Token header.
func (s *Server) AdminHandler() http.Handler {
	r := chi.NewRouter()
	r.Use(s.middlewareRequestID)
	r.Use(s.middlewareRecover)
	r.Use(s.middlewareAdminAuth)

	r.Get("/api/version", s.handleAdminVersion)
	r.Get("/api/overview", s.handleAdminOverview)

	r.Get("/api/connections", s.handleAdminListConnections)
	r.Post("/api/connections", s.handleAdminCreateConnection)
	r.Put("/api/connections/{id}", s.handleAdminUpdateConnection)
	r.Delete("/api/connections/{id}", s.handleAdminDeleteConnection)
	r.Post("/api/connections/{id}/test", s.handleAdminTestConnection)

	r.Get("/api/combos", s.handleAdminListCombos)
	r.Post("/api/combos", s.handleAdminCreateCombo)
	r.Put("/api/combos/{id}", s.handleAdminUpdateCombo)
	r.Delete("/api/combos/{id}", s.handleAdminDeleteCombo)

	r.Get("/api/agent-configs", s.handleAdminListAgentConfigs)
	r.Post("/api/agent-configs", s.handleAdminCreateAgentConfig)
	r.Put("/api/agent-configs/{id}", s.handleAdminUpdateAgentConfig)
	r.Delete("/api/agent-configs/{id}", s.handleAdminDeleteAgentConfig)

	r.Get("/api/api-keys", s.handleAdminListAPIKeys)
	r.Post("/api/api-keys", s.handleAdminCreateAPIKey)
	r.Patch("/api/api-keys/{id}", s.handleAdminPatchAPIKey)
	r.Delete("/api/api-keys/{id}", s.handleAdminDeleteAPIKey)

	r.Get("/api/usage", s.handleAdminUsage)
	r.Get("/api/models", s.handleAdminModels)
	r.Get("/api/proxy-pools", s.handleAdminListProxyPools)

	r.Get("/api/settings", s.handleAdminGetSettings)
	r.Put("/api/settings", s.handleAdminPutSettings)

	r.Post("/api/dashboard-token", s.handleAdminDashboardToken)

	r.Get("/api/export", s.handleAdminExport)
	r.Post("/api/import", s.handleAdminImport)

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown admin endpoint"})
	})
	return r
}

// --- auth ------------------------------------------------------------------------

// middlewareAdminAuth enforces the optional dashboard admin token.
func (s *Server) middlewareAdminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hash, _, err := s.store.GetSetting(r.Context(), db.SettingDashboardTokenHash)
		if err == nil && hash != "" {
			tok := r.Header.Get("X-LoLLM-Token")
			if h := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(h), "bearer ") {
				tok = strings.TrimSpace(h[7:])
			}
			if !auth.VerifyKeyHash(tok, hash) {
				writeJSON(w, http.StatusUnauthorized, map[string]any{
					"error": "dashboard admin token required",
					"code":  "dashboard_token_required",
				})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// handleAdminDashboardToken generates or clears the admin token.
// Generating a new token requires the current token (when one is set).
func (s *Server) handleAdminDashboardToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Action string `json:"action"` // "generate" | "clear"
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	ctx := r.Context()
	current, _, _ := s.store.GetSetting(ctx, db.SettingDashboardTokenHash)

	if in.Action == "clear" {
		if current == "" {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": "no token was set"})
			return
		}
		if err := s.store.SetSetting(ctx, db.SettingDashboardTokenHash, ""); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	// generate (default)
	tok, err := auth.GenerateAdminToken()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if err := s.store.SetSetting(ctx, db.SettingDashboardTokenHash, auth.HashKey(tok)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": tok})
}

// --- version & overview ------------------------------------------------------------

func (s *Server) handleAdminVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"name": productName, "version": s.version})
}

func (s *Server) handleAdminOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logs, _ := s.store.ListUsageLogs(ctx, 1000)
	conns, _ := s.store.ListConnections(ctx)

	cutoff := time.Now().Add(-24 * time.Hour)
	var requests, ok24, pTokens, cTokens, saved, agentCalls int
	var latencySum, latencyN int64
	for _, l := range logs {
		if l.CreatedAt.Before(cutoff) || l.StatusCode == 0 && l.Error == "" && l.LatencyMs == 0 {
			// still count rows inside the window; skip pre-window rows
		}
		if l.CreatedAt.Before(cutoff) {
			continue
		}
		if l.StatusCode == 0 && l.Error == "" && l.LatencyMs == 0 {
			continue // rows with no outcome recorded
		}
		requests++
		if l.StatusCode >= 200 && l.StatusCode < 400 {
			ok24++
		}
		pTokens += l.PromptTokens
		cTokens += l.CompletionTokens
		saved += l.TokensSaved
		if l.AgentRole != "" {
			agentCalls++
		}
		latencySum += int64(l.LatencyMs)
		latencyN++
	}
	status := map[string]int{}
	for _, c := range conns {
		st := c.Status
		if st == "" {
			st = "active"
		}
		status[st]++
	}
	avg := 0.0
	if latencyN > 0 {
		avg = float64(latencySum) / float64(latencyN)
	}
	successRate := 0.0
	if requests > 0 {
		successRate = float64(ok24) / float64(requests) * 100
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"requests_24h":   requests,
		"success_rate":   successRate,
		"prompt_tokens":  pTokens,
		"completion":     cTokens,
		"tokens_saved":   saved,
		"agent_calls":    agentCalls,
		"avg_latency_ms": avg,
		"connections":    status,
		"total_conn":     len(conns),
	})
}

// --- connections ------------------------------------------------------------------

type adminConnection struct {
	ID                string     `json:"id"`
	Name              string     `json:"name"`
	Provider          string     `json:"provider"`
	BaseURL           string     `json:"base_url"`
	ProxyPoolID       string     `json:"proxy_pool_id"`
	APIKeySet         bool       `json:"api_key_set"`
	Priority          int        `json:"priority"`
	Weight            int        `json:"weight"`
	IsActive          bool       `json:"is_active"`
	Status            string     `json:"status"`
	ConsecutiveErrors int        `json:"consecutive_errors"`
	BackoffUntil      *time.Time `json:"backoff_until"`
	LatencyEMAMs      float64    `json:"latency_ema_ms"`
	Models            []string   `json:"models"`
	LastUsedAt        *time.Time `json:"last_used_at"`
}

func toAdminConnection(c *db.Connection) adminConnection {
	st := c.Status
	if st == "" {
		st = "active"
	}
	return adminConnection{
		ID: c.ID, Name: c.Name, Provider: c.Provider, BaseURL: c.BaseURL,
		ProxyPoolID: c.ProxyPoolID, APIKeySet: c.APIKeyEncrypted != "",
		Priority: c.Priority, Weight: c.Weight, IsActive: c.IsActive, Status: st,
		ConsecutiveErrors: c.ConsecutiveErrors, BackoffUntil: c.BackoffUntil,
		LatencyEMAMs: c.LatencyEMAMs, Models: c.Models(), LastUsedAt: c.LastUsedAt,
	}
}

type connectionInput struct {
	Name        string   `json:"name"`
	Provider    string   `json:"provider"`
	APIKey      string   `json:"api_key"` // plaintext; empty on update = keep
	BaseURL     string   `json:"base_url"`
	ProxyPoolID string   `json:"proxy_pool_id"`
	Priority    int      `json:"priority"`
	Weight      int      `json:"weight"`
	IsActive    *bool    `json:"is_active"`
	Models      []string `json:"models"`
}

func (s *Server) handleAdminListConnections(w http.ResponseWriter, r *http.Request) {
	conns, err := s.store.ListConnections(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := make([]adminConnection, 0, len(conns))
	for _, c := range conns {
		out = append(out, toAdminConnection(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"connections": out})
}

func (s *Server) adminConnectionFromInput(in *connectionInput, existing *db.Connection) (*db.Connection, error) {
	c := &db.Connection{
		Name: strings.TrimSpace(in.Name), Provider: strings.TrimSpace(in.Provider),
		BaseURL: strings.TrimSpace(in.BaseURL), ProxyPoolID: strings.TrimSpace(in.ProxyPoolID),
		Priority: in.Priority, Weight: in.Weight,
	}
	if in.IsActive != nil {
		c.IsActive = *in.IsActive
	} else {
		c.IsActive = true
	}
	if c.Weight <= 0 {
		c.Weight = 1
	}
	if existing != nil { // update: carry over what wasn't resent
		c.ID = existing.ID
		c.Status = existing.Status
		c.APIKeyEncrypted = existing.APIKeyEncrypted
		if in.Name == "" {
			c.Name = existing.Name
		}
		if in.Provider == "" {
			c.Provider = existing.Provider
		}
		if in.BaseURL == "" && existing.BaseURL != "" {
			c.BaseURL = existing.BaseURL
		}
		if in.ProxyPoolID == "" && existing.ProxyPoolID != "" {
			c.ProxyPoolID = existing.ProxyPoolID
		}
		if in.IsActive == nil {
			c.IsActive = existing.IsActive
		}
		if in.Models == nil {
			c.ModelsJSON = existing.ModelsJSON
		}
	}
	if in.Models != nil {
		b, err := json.Marshal(in.Models)
		if err != nil {
			return nil, err
		}
		c.ModelsJSON = string(b)
	}
	if strings.TrimSpace(in.APIKey) != "" {
		enc, err := secret.EncryptString(s.masterKey, strings.TrimSpace(in.APIKey))
		if err != nil {
			return nil, err
		}
		c.APIKeyEncrypted = enc
	}
	if c.Name == "" || c.Provider == "" {
		return nil, errBadInput("name and provider are required")
	}
	return c, nil
}

type badInputError string

func (e badInputError) Error() string { return string(e) }

func errBadInput(msg string) error { return badInputError(msg) }

func (s *Server) handleAdminCreateConnection(w http.ResponseWriter, r *http.Request) {
	var in connectionInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	c, err := s.adminConnectionFromInput(&in, nil)
	if err != nil {
		writeAdminError(w, err)
		return
	}
	if err := s.store.CreateConnection(r.Context(), c); err != nil {
		writeAdminError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toAdminConnection(c))
}

func (s *Server) handleAdminUpdateConnection(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	existing, err := s.store.GetConnection(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "connection not found"})
		return
	}
	var in connectionInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	c, err := s.adminConnectionFromInput(&in, existing)
	if err != nil {
		writeAdminError(w, err)
		return
	}
	if err := s.store.UpdateConnection(r.Context(), c); err != nil {
		writeAdminError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAdminConnection(c))
}

func (s *Server) handleAdminDeleteConnection(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteConnection(r.Context(), chi.URLParam(r, "id")); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAdminTestConnection runs a live probe against the provider.
func (s *Server) handleAdminTestConnection(w http.ResponseWriter, r *http.Request) {
	conn, err := s.store.GetConnection(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "connection not found"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	start := time.Now()
	perr := s.Probe(ctx, conn)
	latency := time.Since(start).Milliseconds()
	if perr != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "latency_ms": latency, "error": perr.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "latency_ms": latency})
}

// --- combos -----------------------------------------------------------------------

type comboInput struct {
	Name             string          `json:"name"`
	Strategy         string          `json:"strategy"`
	Compression      string          `json:"compression"`
	AgentModeEnabled bool            `json:"agent_mode_enabled"`
	Models           []db.ComboModel `json:"models"`
}

func normalizeComboStrategy(s string) string {
	switch s {
	case db.StrategyRoundRobin, db.StrategySequential, db.StrategySticky, db.StrategyFusion:
		return s
	default:
		return db.StrategyHealthAware
	}
}

func normalizeCompression(c string) string {
	switch c {
	case "partial", "full":
		return c
	default:
		return "" // inherit global setting / off
	}
}

func (s *Server) handleAdminListCombos(w http.ResponseWriter, r *http.Request) {
	combos, err := s.store.ListCombos(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"combos": combos})
}

func (s *Server) handleAdminCreateCombo(w http.ResponseWriter, r *http.Request) {
	var in comboInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		writeAdminError(w, errBadInput("name is required"))
		return
	}
	c := &db.Combo{
		Name: strings.TrimSpace(in.Name), Strategy: normalizeComboStrategy(in.Strategy),
		Compression: normalizeCompression(in.Compression), AgentModeEnabled: in.AgentModeEnabled,
		Models: in.Models,
	}
	if len(c.Models) == 0 {
		writeAdminError(w, errBadInput("combo needs at least one model entry"))
		return
	}
	if err := s.store.CreateCombo(r.Context(), c); err != nil {
		writeAdminError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) handleAdminUpdateCombo(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	existing, err := s.store.GetCombo(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "combo not found"})
		return
	}
	var in comboInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	c := &db.Combo{
		ID: existing.ID, CreatedAt: existing.CreatedAt,
		Name: strings.TrimSpace(in.Name), Strategy: normalizeComboStrategy(in.Strategy),
		Compression: normalizeCompression(in.Compression), AgentModeEnabled: in.AgentModeEnabled,
		Models: in.Models,
	}
	if c.Name == "" {
		c.Name = existing.Name
	}
	if c.Models == nil {
		c.Models = existing.Models
	}
	if len(c.Models) == 0 {
		writeAdminError(w, errBadInput("combo needs at least one model entry"))
		return
	}
	if err := s.store.UpdateCombo(r.Context(), c); err != nil {
		writeAdminError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handleAdminDeleteCombo(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteCombo(r.Context(), chi.URLParam(r, "id")); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- agent configs ------------------------------------------------------------------

type agentConfigInput struct {
	Name              string         `json:"name"`
	Mode              string         `json:"mode"`
	MaxRounds         int            `json:"max_rounds"`
	HideInternalSteps bool           `json:"hide_internal_steps"`
	Roles             []db.AgentRole `json:"roles"`
}

func (s *Server) handleAdminListAgentConfigs(w http.ResponseWriter, r *http.Request) {
	cfgs, err := s.store.ListAgentConfigs(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent_configs": cfgs})
}

func normalizeAgentMode(m string) string {
	switch m {
	case db.AgentModeDebate, db.AgentModeParallel:
		return m
	default:
		return db.AgentModeCollaborative
	}
}

func (s *Server) handleAdminCreateAgentConfig(w http.ResponseWriter, r *http.Request) {
	var in agentConfigInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	a := &db.AgentConfig{
		Name: strings.TrimSpace(in.Name), Mode: normalizeAgentMode(in.Mode),
		MaxRounds: in.MaxRounds, HideInternalSteps: in.HideInternalSteps, Roles: in.Roles,
	}
	if a.Name == "" || len(a.Roles) == 0 {
		writeAdminError(w, errBadInput("name and at least one role are required"))
		return
	}
	if err := s.store.CreateAgentConfig(r.Context(), a); err != nil {
		writeAdminError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

func (s *Server) handleAdminUpdateAgentConfig(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	existing, err := s.store.GetAgentConfig(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "agent config not found"})
		return
	}
	var in agentConfigInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	a := &db.AgentConfig{
		ID: existing.ID, CreatedAt: existing.CreatedAt,
		Name: strings.TrimSpace(in.Name), Mode: normalizeAgentMode(in.Mode),
		MaxRounds: in.MaxRounds, HideInternalSteps: in.HideInternalSteps, Roles: in.Roles,
	}
	if a.Name == "" {
		a.Name = existing.Name
	}
	if a.Roles == nil {
		a.Roles = existing.Roles
	}
	if len(a.Roles) == 0 {
		writeAdminError(w, errBadInput("at least one role is required"))
		return
	}
	if err := s.store.UpdateAgentConfig(r.Context(), a); err != nil {
		writeAdminError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) handleAdminDeleteAgentConfig(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteAgentConfig(r.Context(), chi.URLParam(r, "id")); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- api keys -----------------------------------------------------------------------

func (s *Server) handleAdminListAPIKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.store.ListAPIKeys(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"api_keys": keys})
}

func (s *Server) handleAdminCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		in.Name = "dashboard"
	}
	plain, err := auth.GenerateAPIKey()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	k := &db.APIKey{
		Name: strings.TrimSpace(in.Name), KeyHash: auth.HashKey(plain),
		KeyPrefix: auth.DisplayPrefix(plain), IsActive: true,
	}
	if err := s.store.CreateAPIKey(r.Context(), k); err != nil {
		writeAdminError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"api_key": k, "key": plain})
}

func (s *Server) handleAdminPatchAPIKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IsActive *bool `json:"is_active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.IsActive == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": `{"is_active": bool} required`})
		return
	}
	id := chi.URLParam(r, "id")
	if err := s.store.SetAPIKeyActive(r.Context(), id, *in.IsActive); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	k, err := s.store.GetAPIKey(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	writeJSON(w, http.StatusOK, k)
}

func (s *Server) handleAdminDeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteAPIKey(r.Context(), chi.URLParam(r, "id")); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- usage / models / proxy pools / settings -------------------------------------------

func (s *Server) handleAdminUsage(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	logs, err := s.store.ListUsageLogs(r.Context(), limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"usage": logs})
}

func (s *Server) handleAdminModels(w http.ResponseWriter, r *http.Request) {
	conns, err := s.store.ListConnections(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	combos, err := s.store.ListCombos(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	byProvider := map[string][]string{}
	for _, c := range conns {
		if !c.IsActive {
			continue
		}
		byProvider[c.Provider] = append(byProvider[c.Provider], c.Models()...)
	}
	names := make([]string, 0, len(combos))
	for _, c := range combos {
		names = append(names, c.Name)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"combos":    names,
		"agents":    []string{"agent-auto", "agent-debate", "agent-parallel"},
		"providers": byProvider,
	})
}

func (s *Server) handleAdminListProxyPools(w http.ResponseWriter, r *http.Request) {
	pools, err := s.store.ListProxyPools(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"proxy_pools": pools})
}

var adminSettingWhitelist = map[string]bool{
	db.SettingCompressionDefault:  true,
	db.SettingHealthCheckEnabled:  true,
	db.SettingHealthCheckInterval: true,
}

func (s *Server) handleAdminGetSettings(w http.ResponseWriter, r *http.Request) {
	all, err := s.store.AllSettings(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	pub := map[string]string{}
	for k, v := range all {
		if adminSettingWhitelist[k] {
			pub[k] = v
		}
	}
	tokenSet := false
	if h, _, _ := s.store.GetSetting(r.Context(), db.SettingDashboardTokenHash); h != "" {
		tokenSet = true
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": pub, "dashboard_token_set": tokenSet})
}

func (s *Server) handleAdminPutSettings(w http.ResponseWriter, r *http.Request) {
	var in map[string]string
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	for k := range in {
		if !adminSettingWhitelist[k] {
			writeAdminError(w, errBadInput("setting "+k+" is not editable here"))
			return
		}
	}
	for k, v := range in {
		switch k {
		case db.SettingCompressionDefault:
			if v != "off" && v != "partial" && v != "full" {
				writeAdminError(w, errBadInput("compression.default must be off|partial|full"))
				return
			}
		case db.SettingHealthCheckEnabled:
			if v != "true" && v != "false" {
				writeAdminError(w, errBadInput("health_check.enabled must be true|false"))
				return
			}
		case db.SettingHealthCheckInterval:
			if !isPositiveInt(v) {
				writeAdminError(w, errBadInput("health_check.interval_seconds must be a positive number"))
				return
			}
		}
	}
	for k, v := range in {
		if err := s.store.SetSetting(r.Context(), k, v); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAdminExport streams a backup.json (spec 3.8: Export / Import).
// Dashboard exports are either secret-free (default) or plaintext
// (?secrets=1); password-sealed backups are a CLI feature.
func (s *Server) handleAdminExport(w http.ResponseWriter, r *http.Request) {
	includeSecrets := r.URL.Query().Get("secrets") == "1"
	b, err := backup.Build(r.Context(), s.store, s.masterKey, backup.Options{IncludeSecrets: includeSecrets})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="backup.json"`)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(b)
}

// handleAdminImport restores an uploaded backup.json. Query: ?replace=1.
// A password-sealed backup also needs the X-LoLLM-Backup-Password header.
func (s *Server) handleAdminImport(w http.ResponseWriter, r *http.Request) {
	var b backup.Backup
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20)).Decode(&b); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid backup JSON: " + err.Error()})
		return
	}
	if b.Format != backup.Format {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "not a " + backup.Format + " file"})
		return
	}
	if b.Secrets == backup.SecretsPBKDF2 {
		pass := r.Header.Get("X-LoLLM-Backup-Password")
		if pass == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "backup is password-sealed; provide X-LoLLM-Backup-Password"})
			return
		}
		if err := b.DecryptSecrets(pass); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
	}
	stats, err := backup.Restore(r.Context(), s.store, s.masterKey, &b,
		backup.RestoreOptions{Replace: r.URL.Query().Get("replace") == "1"})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "stats": stats})
}

// --- helpers ------------------------------------------------------------------------

func isPositiveInt(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n > 0
}

func writeAdminError(w http.ResponseWriter, err error) {
	if _, ok := err.(badInputError); ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
}

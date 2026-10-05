package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/lollm/lollm/internal/auth"
)

// Dashboard password auth: a simple password (default "edoll123") protects
// the management API. Logging in from the dashboard UI sets a session cookie
// (7 days, sliding). Programmatic clients may keep sending the password in
// the X-LoLLM-Token (or Bearer) header.

const (
	// DefaultDashboardPassword is seeded on first run so the dashboard is
	// never wide open. Change it from Settings → Password Dashboard.
	DefaultDashboardPassword = "edoll123"

	settingDashboardPassword = "dashboard.password_hash"
	sessionCookie            = "lollm_session"
	sessionTTL               = 7 * 24 * time.Hour
)

// sessionStore holds active dashboard sessions in memory (restart = re-login).
type sessionStore struct {
	mu sync.Mutex
	m  map[string]time.Time
}

func newSessionStore() *sessionStore {
	return &sessionStore{m: map[string]time.Time{}}
}

func (st *sessionStore) create() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "" // extremely unlikely; caller treats "" as no-session
	}
	id := hex.EncodeToString(b)
	st.mu.Lock()
	st.m[id] = time.Now().Add(sessionTTL)
	st.mu.Unlock()
	return id
}

func (st *sessionStore) valid(id string) bool {
	if id == "" {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	exp, ok := st.m[id]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(st.m, id)
		return false
	}
	st.m[id] = time.Now().Add(sessionTTL) // sliding
	return true
}

func (st *sessionStore) drop(id string) {
	st.mu.Lock()
	delete(st.m, id)
	st.mu.Unlock()
}

func (st *sessionStore) dropAll() {
	st.mu.Lock()
	st.m = map[string]time.Time{}
	st.mu.Unlock()
}

// dashboardPasswordHash returns the stored password hash, seeding the default
// password on first use so every deployment starts protected.
func (s *Server) dashboardPasswordHash(ctx context.Context) string {
	h, _, err := s.store.GetSetting(ctx, settingDashboardPassword)
	if err == nil && h != "" {
		return h
	}
	def := auth.HashKey(DefaultDashboardPassword)
	_ = s.store.SetSetting(ctx, settingDashboardPassword, def)
	return def
}

func (s *Server) passwordIsDefault(ctx context.Context) bool {
	return s.dashboardPasswordHash(ctx) == auth.HashKey(DefaultDashboardPassword)
}

// middlewareDashboardAuth guards the admin API: session cookie first, then
// the password itself in X-LoLLM-Token / Bearer (for scripts & curl).
func (s *Server) middlewareDashboardAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hash := s.dashboardPasswordHash(r.Context())
		if hash == "" { // no password configured (shouldn't happen): open
			next.ServeHTTP(w, r)
			return
		}
		if c, err := r.Cookie(sessionCookie); err == nil && s.sessions.valid(c.Value) {
			next.ServeHTTP(w, r)
			return
		}
		tok := r.Header.Get("X-LoLLM-Token")
		if h := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(h), "bearer ") {
			tok = strings.TrimSpace(h[7:])
		}
		if auth.VerifyKeyHash(tok, hash) {
			next.ServeHTTP(w, r)
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error":          "dashboard password required",
			"code":           "dashboard_password_required",
			"default_active": s.passwordIsDefault(r.Context()),
		})
	})
}

// handleDashboardLogin exchanges the dashboard password for a session cookie.
func (s *Server) handleDashboardLogin(w http.ResponseWriter, r *http.Request) {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip == "" {
		ip = r.RemoteAddr
	}
	if !s.loginLimiter.allow(ip) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": "terlalu banyak percobaan gagal — tunggu sebentar",
			"code":  "login_rate_limited",
		})
		return
	}

	var in struct {
		Password string `json:"password"`
	}
	if err := decodeJSONBody(w, r, &in, 4<<10); err != nil {
		return
	}
	hash := s.dashboardPasswordHash(r.Context())
	if !auth.VerifyKeyHash(strings.TrimSpace(in.Password), hash) {
		s.loginLimiter.fail(ip)
		time.Sleep(400 * time.Millisecond) // slow down brute force
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error":          "password salah",
			"code":           "dashboard_password_required",
			"default_active": s.passwordIsDefault(r.Context()),
		})
		return
	}
	s.loginLimiter.ok(ip)

	id := s.sessions.create()
	if id == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "session error"})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: id, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		MaxAge: int(sessionTTL.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"default_active": s.passwordIsDefault(r.Context()),
	})
}

// handleDashboardLogout drops the session cookie.
func (s *Server) handleDashboardLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.drop(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDashboardPassword changes the dashboard password (and logs everyone
// out — sessions are invalidated).
func (s *Server) handleDashboardPassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := decodeJSONBody(w, r, &in, 4<<10); err != nil {
		return
	}
	hash := s.dashboardPasswordHash(r.Context())
	if !auth.VerifyKeyHash(strings.TrimSpace(in.Current), hash) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "password lama salah"})
		return
	}
	np := strings.TrimSpace(in.New)
	if len(np) < 4 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "password baru minimal 4 karakter"})
		return
	}
	if err := s.store.SetSetting(r.Context(), settingDashboardPassword, auth.HashKey(np)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.sessions.dropAll()
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": "semua sesi dilogout — login ulang dengan password baru"})
}

// decodeJSONBody decodes a small JSON request body, answering 400 itself on
// malformed input.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, v any, max int64) error {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, max)).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return err
	}
	return nil
}

// loginLimiter is a tiny per-IP failed-attempt tracker.
type loginLimiter struct {
	mu       sync.Mutex
	failures map[string]*loginAttempt
}

type loginAttempt struct {
	count int
	until time.Time
}

func newLoginLimiter() *loginLimiter { return &loginLimiter{failures: map[string]*loginAttempt{}} }

func (l *loginLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.failures[ip]
	return a == nil || time.Now().After(a.until)
}

func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.failures[ip]
	if a == nil {
		a = &loginAttempt{}
		l.failures[ip] = a
	}
	a.count++
	if a.count >= 5 {
		a.until = time.Now().Add(time.Minute)
		a.count = 0
	}
}

func (l *loginLimiter) ok(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, ip)
}

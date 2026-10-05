package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Connection statuses (spec section 3.2).
const (
	StatusActive      = "active"
	StatusUnavailable = "unavailable"
	StatusRateLimited = "rate_limited"
)

// Combo strategies (spec section 3.4).
const (
	StrategyHealthAware = "health_aware"
	StrategyRoundRobin  = "round_robin"
	StrategySequential  = "sequential"
	StrategySticky      = "sticky"
	StrategyFusion      = "fusion"
)

// Agent modes (spec section 3.7).
const (
	AgentModeCollaborative = "collaborative"
	AgentModeDebate        = "debate"
	AgentModeParallel      = "parallel"
)

// Well-known setting keys (settings table).
const (
	SettingCompressionDefault  = "compression.default"  // off | partial | full
	SettingHealthCheckEnabled  = "health_check.enabled" // true | false
	SettingHealthCheckInterval = "health_check.interval_seconds"
	SettingDashboardTokenHash  = "dashboard.admin_token_hash"
)

// Connection is one account / API key at one provider.
type Connection struct {
	ID                string     `json:"id"`
	Name              string     `json:"name"`
	Provider          string     `json:"provider"`
	APIKeyEncrypted   string     `json:"-"` // AES-256-GCM ciphertext; never exposed
	BaseURL           string     `json:"base_url,omitempty"`
	Priority          int        `json:"priority"`  // smaller = higher priority
	Weight            int        `json:"weight"`    // weighted rotation
	IsActive          bool       `json:"is_active"` // user-enabled toggle
	Status            string     `json:"status"`    // active | unavailable | rate_limited
	LastUsedAt        *time.Time `json:"last_used_at,omitempty"`
	ConsecutiveErrors int        `json:"consecutive_errors"`
	BackoffUntil      *time.Time `json:"backoff_until,omitempty"`
	LatencyEMAMs      float64    `json:"latency_ema_ms"`
	ProxyPoolID       string     `json:"proxy_pool_id,omitempty"`
	ModelsJSON        string     `json:"-"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// Models parses the connection's model catalog override.
func (c *Connection) Models() []string {
	var ms []string
	if c.ModelsJSON != "" {
		_ = json.Unmarshal([]byte(c.ModelsJSON), &ms)
	}
	return ms
}

// ComboModel is one entry in a combo's model list.
type ComboModel struct {
	ConnectionID string `json:"connection_id"`
	Model        string `json:"model"`
	Priority     int    `json:"priority"`
}

// Combo is a named group of models/connections routable under one name.
type Combo struct {
	ID               string       `json:"id"`
	Name             string       `json:"name"`
	Strategy         string       `json:"strategy"`
	Models           []ComboModel `json:"models"`
	AgentModeEnabled bool         `json:"agent_mode_enabled"`
	AgentConfigID    string       `json:"agent_config_id"`
	Compression      string       `json:"compression"` // "": inherit global; off|partial|full
	CreatedAt        time.Time    `json:"created_at"`
	UpdatedAt        time.Time    `json:"updated_at"`
}

// ProxyPool is a named list of HTTP/SOCKS5 proxy URLs.
type ProxyPool struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Proxies   []string  `json:"proxies"`
	CreatedAt time.Time `json:"created_at"`
}

// APIKey is an internal key used by clients to authenticate to the gateway.
// Only the hash is stored; KeyPrefix is safe for display.
type APIKey struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	KeyHash    string     `json:"-"`
	KeyPrefix  string     `json:"key_prefix"`
	IsActive   bool       `json:"is_active"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// UsageLog records one proxied LLM call (or one internal agent step).
type UsageLog struct {
	ID               string    `json:"id"`
	RequestID        string    `json:"request_id"`
	APIKeyID         string    `json:"api_key_id"`
	ComboName        string    `json:"combo_name"`
	ConnectionID     string    `json:"connection_id"`
	Model            string    `json:"model"`
	AgentRole        string    `json:"agent_role"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	TokensSaved      int       `json:"tokens_saved"`
	LatencyMs        int       `json:"latency_ms"`
	StatusCode       int       `json:"status_code"`
	Error            string    `json:"error"`
	CreatedAt        time.Time `json:"created_at"`
}

// AgentRole is one role in an Agent Mode pipeline.
type AgentRole struct {
	Name         string `json:"name"`
	Model        string `json:"model"` // may reference a combo name (nested routing)
	SystemPrompt string `json:"system_prompt"`
}

// AgentConfig defines an Agent Mode pipeline (spec section 3.7).
type AgentConfig struct {
	ID                string      `json:"id"`
	Name              string      `json:"name"`
	Mode              string      `json:"mode"`
	Roles             []AgentRole `json:"roles"`
	MaxRounds         int         `json:"max_rounds"`
	HideInternalSteps bool        `json:"hide_internal_steps"`
	CreatedAt         time.Time   `json:"created_at"`
	UpdatedAt         time.Time   `json:"updated_at"`
}

// NewID returns a random prefixed identifier, e.g. "conn_1f0e…".
func NewID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic("db: crypto/rand unavailable: " + err.Error())
	}
	return prefix + hex.EncodeToString(b)
}

// --- SQLite datetime helpers -----------------------------------------------
// All datetimes are stored as UTC text "YYYY-MM-DD HH:MM:SS".

func fmtTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func timeArg(t *time.Time) any {
	if t == nil {
		return nil
	}
	return fmtTime(*t)
}

func parseTimePtr(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	if t, err := time.Parse(timeLayout, s.String); err == nil {
		u := t.UTC()
		return &u
	}
	return nil
}

func parseTime(s sql.NullString) time.Time {
	if t := parseTimePtr(s); t != nil {
		return *t
	}
	return time.Time{}
}

// nowPair returns the current UTC time (truncated to a second) and its
// canonical string form, so struct fields and DB rows agree exactly.
func nowPair() (time.Time, string) {
	t := time.Now().UTC().Truncate(time.Second)
	return t, t.Format(timeLayout)
}

// --- write helpers -----------------------------------------------------------

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func defStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func nzInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

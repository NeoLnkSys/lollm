-- LoLLM initial schema (spec section 6, with documented extensions).
-- Extensions beyond the spec: connections.latency_ema_ms (health-aware scoring),
-- api_keys.key_prefix (safe display), usage_logs.agent_role + tokens_saved.

CREATE TABLE proxy_pools (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  proxies_json TEXT NOT NULL DEFAULT '[]',
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE agent_configs (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  mode TEXT NOT NULL DEFAULT 'collaborative'
    CHECK (mode IN ('collaborative', 'debate', 'parallel')),
  roles_json TEXT NOT NULL DEFAULT '[]',
  max_rounds INTEGER NOT NULL DEFAULT 2,
  hide_internal_steps INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE connections (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  provider TEXT NOT NULL,
  api_key_encrypted TEXT NOT NULL,           -- AES-256-GCM ciphertext, never plain
  base_url TEXT,
  priority INTEGER NOT NULL DEFAULT 100,     -- smaller = higher priority
  weight INTEGER NOT NULL DEFAULT 1,         -- for weighted rotation
  is_active INTEGER NOT NULL DEFAULT 1,
  status TEXT NOT NULL DEFAULT 'active'
    CHECK (status IN ('active', 'unavailable', 'rate_limited')),
  last_used_at TEXT,
  consecutive_errors INTEGER NOT NULL DEFAULT 0,
  backoff_until TEXT,
  latency_ema_ms REAL NOT NULL DEFAULT 0,    -- exponential moving average
  proxy_pool_id TEXT REFERENCES proxy_pools(id) ON DELETE SET NULL,
  models_json TEXT NOT NULL DEFAULT '[]',    -- model catalog override
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_connections_provider ON connections(provider);
CREATE INDEX idx_connections_routing ON connections(is_active, status);

CREATE TABLE combos (
  id TEXT PRIMARY KEY,
  name TEXT UNIQUE NOT NULL,
  strategy TEXT NOT NULL DEFAULT 'health_aware'
    CHECK (strategy IN ('health_aware', 'round_robin', 'sequential', 'sticky', 'fusion')),
  models_json TEXT NOT NULL,                 -- [{connection_id, model, priority}]
  agent_mode_enabled INTEGER NOT NULL DEFAULT 0,
  agent_config_id TEXT REFERENCES agent_configs(id) ON DELETE SET NULL,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE api_keys (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL DEFAULT '',
  key_hash TEXT NOT NULL UNIQUE,             -- SHA-256 hex; plain never stored
  key_prefix TEXT NOT NULL DEFAULT '',        -- safe display prefix
  is_active INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  last_used_at TEXT
);

CREATE TABLE settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE usage_logs (
  id TEXT PRIMARY KEY,
  request_id TEXT NOT NULL,
  api_key_id TEXT,
  combo_name TEXT,
  connection_id TEXT,
  model TEXT,
  agent_role TEXT,                            -- planner/reviewer/finalizer/...
  prompt_tokens INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  tokens_saved INTEGER NOT NULL DEFAULT 0,    -- estimated tokens saved by compression
  latency_ms INTEGER NOT NULL DEFAULT 0,
  status_code INTEGER NOT NULL DEFAULT 0,
  error TEXT,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_usage_logs_created ON usage_logs(created_at DESC);
CREATE INDEX idx_usage_logs_request ON usage_logs(request_id);
CREATE INDEX idx_usage_logs_connection ON usage_logs(connection_id);

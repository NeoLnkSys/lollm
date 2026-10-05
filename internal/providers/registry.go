package providers

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lollm/lollm/internal/db"
)

// AuthStyle selects how the provider expects its API key.
type AuthStyle string

const (
	AuthBearer AuthStyle = "bearer" // Authorization: Bearer <key> (default)
	AuthNone   AuthStyle = "none"   // no auth header (local Ollama)
)

// Meta describes one provider: where its OpenAI-compatible API lives and how
// it authenticates. The chat endpoint is always {base}/chat/completions and
// the models endpoint {base}/models.
type Meta struct {
	Name            string
	DefaultBaseURL  string // "" together with RequiresBaseURL = user must set base_url
	RequiresBaseURL bool
	AuthStyle       AuthStyle
	ExtraHeaders    map[string]string
	Notes           string
}

var registry = map[string]Meta{
	"openrouter": {
		Name:           "openrouter",
		DefaultBaseURL: "https://openrouter.ai/api/v1",
		ExtraHeaders:   map[string]string{"X-Title": "LoLLM"},
	},
	"openai": {
		Name:           "openai",
		DefaultBaseURL: "https://api.openai.com/v1",
	},
	"groq": {
		Name:           "groq",
		DefaultBaseURL: "https://api.groq.com/openai/v1",
	},
	"gemini": {
		Name:           "gemini",
		DefaultBaseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
		Notes:          "uses Google's OpenAI-compatible endpoint; native generateContent adapter planned for v1.1",
	},
	"mistral": {
		Name:           "mistral",
		DefaultBaseURL: "https://api.mistral.ai/v1",
	},
	"together": {
		Name:           "together",
		DefaultBaseURL: "https://api.together.xyz/v1",
	},
	"fireworks": {
		Name:           "fireworks",
		DefaultBaseURL: "https://api.fireworks.ai/inference/v1",
	},
	"cerebras": {
		Name:           "cerebras",
		DefaultBaseURL: "https://api.cerebras.ai/v1",
	},
	"poolside": {
		Name:           "poolside",
		DefaultBaseURL: "https://inference.poolside.ai/v1",
		Notes:          "OpenAI-compatible endpoint (inference.poolside.ai; the old api.poolside.ai domain no longer resolves)",
	},
	"ollama": {
		Name:           "ollama",
		DefaultBaseURL: "https://ollama.com/v1",
		Notes:          "Ollama Cloud OpenAI-compatible API (ollama.com/v1; api.ollama.com 301-redirects here). For a local server set base_url to http://localhost:11434/v1 and leave the API key empty",
	},
	"cloudflare-ai": {
		Name:            "cloudflare-ai",
		RequiresBaseURL: true,
		Notes:           "set base_url to https://api.cloudflare.com/client/v4/accounts/<ACCOUNT_ID>/ai/v1",
	},
	"custom": {
		Name:            "custom",
		RequiresBaseURL: true,
		Notes:           "any OpenAI-compatible endpoint; set base_url",
	},
}

// aliases maps alternative spellings to canonical provider names.
var aliases = map[string]string{
	"openai-compatible": "custom",
	"openai_compatible": "custom",
	"cloudflare":        "cloudflare-ai",
	"google":            "gemini",
	"open-router":       "openrouter",
}

// Lookup returns the metadata for a provider name (case-insensitive, alias-aware).
func Lookup(name string) (Meta, bool) {
	key := strings.ToLower(strings.TrimSpace(name))
	if canonical, ok := aliases[key]; ok {
		key = canonical
	}
	m, ok := registry[key]
	return m, ok
}

// Names returns all canonical provider names, sorted.
func Names() []string {
	out := make([]string, 0, len(registry))
	for name := range registry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// BaseURL resolves the API base for a connection: an explicit connection
// base_url always wins over the provider default.
func (m Meta) BaseURL(conn *db.Connection) (string, error) {
	if conn.BaseURL != "" {
		return strings.TrimRight(conn.BaseURL, "/"), nil
	}
	if m.DefaultBaseURL != "" {
		return m.DefaultBaseURL, nil
	}
	return "", fmt.Errorf("provider %q has no default base URL — set the connection's base_url (e.g. %s)",
		m.Name, m.Notes)
}

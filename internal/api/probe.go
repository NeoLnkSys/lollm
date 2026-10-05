package api

import (
	"context"

	"github.com/lollm/lollm/internal/db"
	"github.com/lollm/lollm/internal/providers"
)

// Probe reports whether a connection can currently serve traffic. It is the
// health checker's entry point into the provider adapters.
//
// Probe strategy per provider (learned from live validation):
//   - cloudflare-ai: no OpenAI-style GET /models exists (405) → prove with a
//     minimal real chat completion
//   - ollama: GET /models succeeds even for chat-rejected keys → prove with
//     a minimal real chat completion
//   - everything else: an authenticated GET /models (free, fast)
func (s *Server) Probe(ctx context.Context, conn *db.Connection) error {
	adapter := s.adapterFor(conn.Provider)
	switch conn.Provider {
	case "cloudflare-ai", "ollama":
		model := probeChatModel(conn)
		body := map[string]any{
			"model":      model,
			"max_tokens": 1,
			"messages":   []any{map[string]any{"role": "user", "content": "ping"}},
		}
		_, err := adapter.Chat(ctx, conn, &providers.ChatRequest{Body: body, Model: model, Stream: false})
		return err
	default:
		_, err := adapter.ListModels(ctx, conn)
		return err
	}
}

// probeChatModel picks a cheap model for chat probes: a small known model if
// the connection's catalog has one, else the first catalog entry, else a
// Cloudflare default.
func probeChatModel(conn *db.Connection) string {
	models := conn.Models()
	for _, prefer := range []string{"gpt-oss:20b", "gpt-oss:120b"} {
		for _, m := range models {
			if m == prefer {
				return m
			}
		}
	}
	if len(models) > 0 {
		return models[0]
	}
	return "@cf/meta/llama-3.3-70b-instruct-fp8-fast"
}

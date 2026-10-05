package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"github.com/lollm/lollm/internal/auth"
)

// APIKeyID returns the authenticated internal API key id from the context.
func APIKeyID(ctx context.Context) string {
	v, _ := ctx.Value(ctxAPIKeyID).(string)
	return v
}

// middlewareAuth enforces the internal API key (Bearer token or x-api-key
// header) on /v1 endpoints. Keys are looked up by SHA-256 hash.
func (s *Server) middlewareAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			writeOpenAIError(w, http.StatusUnauthorized,
				"Missing API key. Pass the internal LoLLM key as 'Authorization: Bearer <key>' (or the x-api-key header).",
				"invalid_request_error", "invalid_api_key")
			return
		}
		key, err := s.store.GetAPIKeyByHash(r.Context(), auth.HashKey(token))
		if err != nil || !key.IsActive {
			writeOpenAIError(w, http.StatusUnauthorized,
				"Invalid or revoked API key.", "invalid_request_error", "invalid_api_key")
			return
		}
		if err := s.store.TouchAPIKeyUsed(r.Context(), key.ID); err != nil {
			s.log.Warn("touch api key failed", "err", err)
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxAPIKeyID, key.ID)))
	})
}

func bearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); len(h) > 7 && (h[:7] == "Bearer " || h[:7] == "bearer ") {
		return h[7:]
	}
	return r.Header.Get("x-api-key")
}

// --- shared response helpers ------------------------------------------------------

// openAIError mirrors the OpenAI error payload so SDKs parse it natively.
type openAIError struct {
	Error openAIErrorBody `json:"error"`
}

type openAIErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

func writeOpenAIError(w http.ResponseWriter, status int, message, errType, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(openAIError{Error: openAIErrorBody{
		Message: message, Type: errType, Code: code,
	}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "req_" + hex.EncodeToString(b)
}

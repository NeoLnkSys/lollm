package api

import (
	"net/http"

	"github.com/lollm/lollm/internal/compression"
	"github.com/lollm/lollm/internal/db"
)

// effectiveCompression resolves the compression mode for one request
// (spec section 3.5), highest priority first:
//
//  1. X-LoLLM-Compression header (off | partial | full)
//  2. the combo's compression column ("" = inherit)
//  3. the compression.default setting (seeded to "off")
//
// Unknown header values are ignored (fall through) rather than rejected — a
// typo must not break a request.
func (s *Server) effectiveCompression(r *http.Request, combo *db.Combo) compression.Mode {
	if v := r.Header.Get("X-LoLLM-Compression"); v != "" {
		if m, ok := compression.ParseMode(v); ok {
			return m
		}
	}
	if combo != nil && combo.Compression != "" {
		if m, ok := compression.ParseMode(combo.Compression); ok {
			return m
		}
	}
	if v, ok, _ := s.store.GetSetting(r.Context(), db.SettingCompressionDefault); ok {
		if m, ok2 := compression.ParseMode(v); ok2 {
			return m
		}
	}
	return compression.Off
}

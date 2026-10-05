package api

import (
	"net/http"
)

// handleModels serves GET /v1/models: combo names plus every active
// connection's model catalog, deduplicated (combos win).
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}

	seen := map[string]bool{}
	var data []modelEntry
	add := func(id, owner string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		data = append(data, modelEntry{ID: id, Object: "model", OwnedBy: owner})
	}

	combos, err := s.store.ListCombos(r.Context())
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "database error", "server_error", "internal_error")
		return
	}
	for _, c := range combos {
		add(c.Name, "lollm")
	}

	conns, err := s.store.ListConnections(r.Context())
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "database error", "server_error", "internal_error")
		return
	}
	for _, c := range conns {
		if !c.IsActive {
			continue
		}
		for _, m := range c.Models() {
			add(m, c.Provider)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

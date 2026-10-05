package api

import (
	"context"

	"github.com/lollm/lollm/internal/db"
	"github.com/lollm/lollm/internal/providers"
)

// chatWithRetries wraps one adapter.Chat call with per-connection retries.
//
// Rate limits are often scoped to a single API key or a single source IP:
//   - a multi-key connection rotates to the NEXT KEY on each retry
//     (applyAuth round-robins per request);
//   - a connection behind a proxy pool (>1 proxy) also rotates to the NEXT
//     PROXY on each retry (the rotating transport advances per request),
//     bypassing IP-based limits.
//
// Only rate-limit/quota errors are retried, and only while another key or
// proxy is available. Other errors (auth, bad request, network) fall through
// immediately to the normal cross-connection combo fallback. Failed retries
// are logged (one line each) but do not spam the usage log: only the final
// outcome of the connection is recorded there.
func (s *Server) chatWithRetries(ctx context.Context, adapter providers.Adapter, conn *db.Connection,
	req *providers.ChatRequest, requestID, comboName, model string) (*providers.ChatResponse, error) {

	resp, err := adapter.Chat(ctx, conn, req)
	if err == nil {
		return resp, nil
	}
	pe := toProviderError(err)
	if pe.Kind != providers.KindRateLimited && pe.Kind != providers.KindQuota {
		return resp, err
	}

	maxTries := len(providers.KeysOf(s.masterKey, conn)) // 1 = single key
	if conn.ProxyPoolID != "" {
		if pool, perr := s.store.GetProxyPool(ctx, conn.ProxyPoolID); perr == nil && len(pool.Proxies) > 1 && maxTries < 2 {
			maxTries = 2 // single key, but a proxy rotation may dodge an IP-based limit
		}
	}
	if maxTries > 4 {
		maxTries = 4
	}

	for try := 2; try <= maxTries; try++ {
		s.log.Warn("rate-limited: rotating key/proxy, retrying same connection",
			"request_id", requestID, "combo", comboName, "connection", conn.Name,
			"model", model, "try", try, "of", maxTries, "kind", pe.Kind)
		resp, err = adapter.Chat(ctx, conn, req)
		if err == nil {
			return resp, nil
		}
		pe = toProviderError(err)
		if pe.Kind != providers.KindRateLimited && pe.Kind != providers.KindQuota {
			return resp, err
		}
	}
	return resp, err
}

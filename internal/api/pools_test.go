package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lollm/lollm/internal/db"
	"github.com/lollm/lollm/internal/providers"
)

// Dashboard proxy-pool management: create/list/update/delete + live probe.
func TestAdminProxyPoolCRUD(t *testing.T) {
	s, store, _, _ := newTestAPISrv(t)
	h := s.AdminHandler()

	// Invalid proxy URL is rejected.
	code, body := doReq(t, h, "POST", "/api/proxy-pools",
		`{"name":"p1","proxies":["not a url"]}`, nil)
	if code != 400 {
		t.Fatalf("invalid url must 400: %d %s", code, body)
	}
	// Empty proxy list is rejected.
	code, body = doReq(t, h, "POST", "/api/proxy-pools", `{"name":"p1","proxies":[]}`, nil)
	if code != 400 {
		t.Fatalf("empty list must 400: %d %s", code, body)
	}

	// Create.
	code, body = doReq(t, h, "POST", "/api/proxy-pools",
		`{"name":"pool-a","proxies":["http://u:pw@h1:8080","socks5://h2:1080"]}`, nil)
	if code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	var created struct {
		Pool struct {
			ID      string   `json:"id"`
			Proxies []string `json:"proxies"`
		} `json:"proxy_pool"`
	}
	json.Unmarshal([]byte(body), &created)
	if created.Pool.ID == "" || len(created.Pool.Proxies) != 2 {
		t.Fatalf("bad create response: %s", body)
	}

	// Update replaces the list.
	code, body = doReq(t, h, "PUT", "/api/proxy-pools/"+created.Pool.ID,
		`{"name":"pool-a","proxies":["http://h3:3128"]}`, nil)
	if code != 200 || !strings.Contains(body, "h3:3128") {
		t.Fatalf("update: %d %s", code, body)
	}

	// Delete.
	code, _ = doReq(t, h, "DELETE", "/api/proxy-pools/"+created.Pool.ID, "", nil)
	if code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if pools, _ := store.ListProxyPools(context.Background()); len(pools) != 0 {
		t.Fatal("pool still present")
	}
	code, _ = doReq(t, h, "GET", "/api/proxy-pools", "", nil)
	if code != 200 {
		t.Fatalf("list: %d", code)
	}
}

// The per-proxy probe must report ok + latency through a working proxy.
// A plain HTTP server doubles as an HTTP proxy for a non-CONNECT request.
func TestAdminProxyPoolProbe(t *testing.T) {
	s, store, _, _ := newTestAPISrv(t)
	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer px.Close()

	created := &db.ProxyPool{Name: "probe-pool", Proxies: []string{px.URL}}
	if err := store.CreateProxyPool(context.Background(), created); err != nil {
		t.Fatal(err)
	}
	pool := created.ID
	code, body := doReq(t, s.AdminHandler(), "GET",
		"/api/proxy-pools/"+pool+"/test?target=http://example.com/ping", "", nil)
	if code != 200 || !strings.Contains(body, `"ok":true`) || !strings.Contains(body, `"latency_ms"`) {
		t.Fatalf("probe: %d %s", code, body)
	}

	// Single-proxy filter via ?url=.
	code, body = doReq(t, s.AdminHandler(), "GET",
		"/api/proxy-pools/"+pool+"/test?url=http://127.0.0.1:1/dead&target=http://example.com/ping", "", nil)
	if code != 200 || strings.Contains(body, `"ok":true`) {
		t.Fatalf("single probe of dead proxy must fail: %d %s", code, body)
	}
}

// The combo picker's model source: live catalog from the provider.
func TestAdminConnectionModels(t *testing.T) {
	s, store, _, masterKey := newTestAPISrv(t)
	m := providers.NewMockServer(providers.MockOK())
	defer m.Close()

	conn := addMockConn(t, store, masterKey, "upstream", m, []string{"mock-model"}, true)
	code, body := doReq(t, s.AdminHandler(), "GET", "/api/connections/"+conn.ID+"/models", "", nil)
	if code != 200 || !strings.Contains(body, `"source":"live"`) || !strings.Contains(body, "mock-model-tools") {
		t.Fatalf("models: %d %s", code, body)
	}

	// Unknown connection → 404.
	code, _ = doReq(t, s.AdminHandler(), "GET", "/api/connections/nope/models", "", nil)
	if code != 404 {
		t.Fatalf("unknown conn must 404: %d", code)
	}
}

package dashboard

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lollm/lollm/internal/api"
	"github.com/lollm/lollm/internal/auth"
	"github.com/lollm/lollm/internal/config"
	"github.com/lollm/lollm/internal/db"
	"github.com/lollm/lollm/internal/secret"
)

// newDash builds a dashboard handler over a fresh, fully wired API server.
func newDash(t *testing.T) (http.Handler, *api.Server, string) {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	store := db.NewStore(d)
	masterKey, err := secret.LoadOrCreateMasterKey(filepath.Join(dir, ".master.key"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := api.New(config.Default(), store, masterKey, log, "test-version")

	plain, err := auth.GenerateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateAPIKey(context.Background(), &db.APIKey{
		Name: "dash", KeyHash: auth.HashKey(plain), KeyPrefix: auth.DisplayPrefix(plain), IsActive: true,
	}); err != nil {
		t.Fatal(err)
	}
	return New("test-version", s), s, plain
}

func TestDashboardServesUI(t *testing.T) {
	h, _, _ := newDash(t)
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("index status %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "LoLLM") || !strings.Contains(string(body), "test-version") {
		t.Fatalf("index must be the SPA with the version injected: %d bytes", len(body))
	}
	if strings.Contains(string(body), "__VERSION__") {
		t.Fatal("version placeholder not replaced")
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("content type: %s", ct)
	}
}

func TestDashboardProxiesV1(t *testing.T) {
	h, _, key := newDash(t)
	ts := httptest.NewServer(h)
	defer ts.Close()

	// Without a key → 401 (auth happens on the API mux).
	resp, err := ts.Client().Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /v1 must 401, got %d", resp.StatusCode)
	}

	// With the key → 200 through the dashboard origin.
	req, _ := http.NewRequest("GET", ts.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp2, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != 200 || !strings.Contains(string(body), `"object":"list"`) {
		t.Fatalf("authenticated /v1 through dashboard: %d %s", resp2.StatusCode, body)
	}
}

func TestDashboardAdminAPI(t *testing.T) {
	h, _, _ := newDash(t)
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/api/version")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "test-version") {
		t.Fatalf("admin api: %d %s", resp.StatusCode, body)
	}
}

func TestDashboardHealthz(t *testing.T) {
	h, _, _ := newDash(t)
	ts := httptest.NewServer(h)
	defer ts.Close()
	resp, err := ts.Client().Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz: %d", resp.StatusCode)
	}
}

func TestDashboardUnknownPathRedirects(t *testing.T) {
	h, _, _ := newDash(t)
	ts := httptest.NewServer(h)
	defer ts.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(ts.URL + "/whatever")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("unknown path should redirect to /, got %d", resp.StatusCode)
	}
}

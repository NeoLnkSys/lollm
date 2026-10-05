package api

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
)

// dashTS menjalankan admin handler di httptest server + client ber-cookie jar.
func dashTS(t *testing.T, s *Server) (*httptest.Server, *http.Client) {
	t.Helper()
	ts := httptest.NewServer(s.AdminHandler())
	t.Cleanup(ts.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return ts, &http.Client{Jar: jar}
}

func login(t *testing.T, client *http.Client, url, password string) *http.Response {
	t.Helper()
	resp, err := client.Post(url+"/api/login", "application/json",
		strings.NewReader(`{"password":"`+password+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// Login password default (edoll123) → cookie sesi → akses admin; header
// X-LoLLM-Token & Bearer tetap berfungsi untuk akses programatik.
func TestDashboardPasswordLogin(t *testing.T) {
	s, _, _, _ := newTestAPISrv(t)
	h := s.AdminHandler()

	// Tanpa kredensial → 401 + kode + penanda default aktif.
	code, body := doReq(t, h, "GET", "/api/version", "", map[string]string{"X-LoLLM-Token": "salah"})
	if code != 401 || !strings.Contains(body, "dashboard_password_required") || !strings.Contains(body, `"default_active":true`) {
		t.Fatalf("expected 401 dashboard_password_required (default active), got %d %s", code, body)
	}

	// Header X-LoLLM-Token dengan password default → 200.
	if code, _ = doReq(t, h, "GET", "/api/version", "", map[string]string{"X-LoLLM-Token": "edoll123"}); code != 200 {
		t.Fatalf("X-LoLLM-Token default password failed: %d", code)
	}
	// Bentuk Bearer juga berlaku.
	if code, _ = doReq(t, h, "GET", "/api/version", "", map[string]string{"Authorization": "Bearer edoll123"}); code != 200 {
		t.Fatalf("Bearer default password failed: %d", code)
	}

	// Flow login via cookie.
	ts, client := dashTS(t, s)
	if resp := login(t, client, ts.URL, "password-ngaco"); resp.StatusCode != 401 {
		t.Fatalf("wrong password must 401, got %d", resp.StatusCode)
	}
	if resp := login(t, client, ts.URL, "edoll123"); resp.StatusCode != 200 {
		t.Fatalf("default password must 200, got %d", resp.StatusCode)
	}
	resp, err := client.Get(ts.URL + "/api/version")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("session cookie must grant access, got %d", resp.StatusCode)
	}

	// Logout memutus sesi.
	if resp, err := client.Post(ts.URL+"/api/logout", "application/json", nil); err != nil || resp.StatusCode != 200 {
		t.Fatalf("logout failed: %v %d", err, resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	resp, err = client.Get(ts.URL + "/api/version")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("after logout access must be 401, got %d", resp.StatusCode)
	}
}

// Ganti password: butuh password lama yang benar, menghapus semua sesi, dan
// menonaktifkan penanda default.
func TestDashboardPasswordChange(t *testing.T) {
	s, _, _, _ := newTestAPISrv(t)
	ts, client := dashTS(t, s)
	if resp := login(t, client, ts.URL, "edoll123"); resp.StatusCode != 200 {
		t.Fatalf("login: %d", resp.StatusCode)
	}

	// Password lama salah → 400.
	resp, err := client.Post(ts.URL+"/api/dashboard-password", "application/json",
		strings.NewReader(`{"current":"bukan","new":"baru123"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("wrong current password must 400, got %d", resp.StatusCode)
	}

	// Password baru terlalu pendek → 400.
	resp, err = client.Post(ts.URL+"/api/dashboard-password", "application/json",
		strings.NewReader(`{"current":"edoll123","new":"abc"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("short new password must 400, got %d", resp.StatusCode)
	}

	// Ganti benar → 200, sesi lama hangus.
	resp, err = client.Post(ts.URL+"/api/dashboard-password", "application/json",
		strings.NewReader(`{"current":"edoll123","new":"baru123"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("password change failed: %d", resp.StatusCode)
	}
	resp, err = client.Get(ts.URL + "/api/version")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("old session must be dropped after password change, got %d", resp.StatusCode)
	}

	// Password lama tak berlaku; password baru bisa login & default nonaktif.
	h := s.AdminHandler()
	if code, _ := doReq(t, h, "GET", "/api/version", "", map[string]string{"X-LoLLM-Token": "edoll123"}); code != 401 {
		t.Fatalf("old password must be rejected, got %d", code)
	}
	code, body := doReq(t, h, "GET", "/api/version", "", map[string]string{"X-LoLLM-Token": "baru123"})
	if code != 200 {
		t.Fatalf("new password must work, got %d", code)
	}
	if strings.Contains(body, "default_active") { // hanya muncul di 401 — sanity
		t.Fatal("unexpected default_active in success body")
	}
	code, body = doReq(t, h, "GET", "/api/version", "", map[string]string{"X-LoLLM-Token": "salah"})
	if code != 401 || !strings.Contains(body, `"default_active":false`) {
		t.Fatalf("default_active must be false after change: %d %s", code, body)
	}
}

package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// forwardProxy is a minimal HTTP forward proxy for tests: plain-http requests
// arrive in absolute-form and are re-sent to the origin.
func forwardProxy(counter *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host == "" { // not absolute-form → not a proxied request
			http.Error(w, "expected absolute-form request", http.StatusBadRequest)
			return
		}
		counter.Add(1)
		req, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		req.Header = r.Header.Clone()
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
}

func TestManagerRoutesThroughPool(t *testing.T) {
	var proxyHits atomic.Int64
	proxySrv := forwardProxy(&proxyHits)
	defer proxySrv.Close()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("origin-ok"))
	}))
	defer target.Close()

	mgr := NewManager(func(ctx context.Context, poolID string) ([]string, error) {
		return []string{proxySrv.URL}, nil
	})

	resp, err := mgr.Client("pool_x").Get(target.URL + "/ping")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "origin-ok" {
		t.Fatalf("unexpected body: %q", body)
	}
	if proxyHits.Load() == 0 {
		t.Fatal("request must have gone through the proxy")
	}
}

func TestManagerRotatesProxies(t *testing.T) {
	var hitsA, hitsB atomic.Int64
	proxyA := forwardProxy(&hitsA)
	defer proxyA.Close()
	proxyB := forwardProxy(&hitsB)
	defer proxyB.Close()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer target.Close()

	mgr := NewManager(func(ctx context.Context, poolID string) ([]string, error) {
		return []string{proxyA.URL, proxyB.URL}, nil
	})
	client := mgr.Client("pool_rot")

	for i := 0; i < 6; i++ {
		resp, err := client.Get(target.URL + "/x")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if hitsA.Load() != 3 || hitsB.Load() != 3 {
		t.Fatalf("rotation must be even: A=%d B=%d", hitsA.Load(), hitsB.Load())
	}
}

func TestManagerDirectForEmptyAndUnknownPool(t *testing.T) {
	var proxyHits atomic.Int64
	proxySrv := forwardProxy(&proxyHits)
	defer proxySrv.Close()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("direct-ok"))
	}))
	defer target.Close()

	// No pool id → direct.
	mgr := NewManager(func(ctx context.Context, poolID string) ([]string, error) {
		return []string{proxySrv.URL}, nil
	})
	resp, err := mgr.Client("").Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if proxyHits.Load() != 0 {
		t.Fatal("empty pool id must go direct")
	}

	// Resolver error → direct fallback.
	mgr2 := NewManager(func(ctx context.Context, poolID string) ([]string, error) {
		return nil, context.DeadlineExceeded
	})
	resp, err = mgr2.Client("pool_missing").Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if proxyHits.Load() != 0 {
		t.Fatal("unresolvable pool must fall back to direct")
	}

	// Nil resolver → always direct.
	mgr3 := NewManager(nil)
	if got := mgr3.Client("anything"); got != mgr3.Client("") {
		t.Fatal("nil resolver manager should always hand out the direct client")
	}
}

func TestManagerCachesClientsPerPool(t *testing.T) {
	mgr := NewManager(func(ctx context.Context, poolID string) ([]string, error) {
		return []string{"http://127.0.0.1:1"}, nil
	})
	if mgr.Client("pool_a") != mgr.Client("pool_a") {
		t.Fatal("clients must be cached per pool")
	}
	if mgr.Client("pool_a") == mgr.Client("pool_b") {
		t.Fatal("different pools must get different clients")
	}
}

func TestInvalidateDropsCachedClient(t *testing.T) {
	mgr := NewManager(func(ctx context.Context, poolID string) ([]string, error) {
		return []string{"http://127.0.0.1:1"}, nil
	})
	c1 := mgr.Client("pool_a")
	if c1 == nil {
		t.Fatal("nil client")
	}
	mgr.Invalidate("pool_a")
	if mgr.Client("pool_a") == c1 {
		t.Fatal("cached client must be dropped after Invalidate")
	}
	// Invalidate of an unknown/empty pool is a no-op.
	mgr.Invalidate("")
	mgr.Invalidate("never-seen")
}

func TestNewProxiedClientValidation(t *testing.T) {
	if _, err := NewProxiedClient("not a url"); err == nil {
		t.Fatal("malformed URL must be rejected")
	}
	if _, err := NewProxiedClient("socks5://h:1080"); err != nil {
		t.Fatalf("valid socks5 URL rejected: %v", err)
	}
}

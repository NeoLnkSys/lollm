// Package proxy maps proxy pools to HTTP clients. A connection assigned to a
// pool has all its upstream traffic routed through the pool's proxies
// (HTTP/HTTPS/SOCKS5, rotated round-robin per request).
package proxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// Resolver fetches the proxy URLs of a pool (typically a DB lookup).
type Resolver func(ctx context.Context, poolID string) ([]string, error)

// Manager caches one HTTP client per pool. A manager with a nil resolver
// always answers with the direct client.
type Manager struct {
	resolve Resolver

	mu      sync.Mutex
	clients map[string]*http.Client
	direct  *http.Client
}

// NewManager builds a proxy client manager.
func NewManager(resolve Resolver) *Manager {
	return &Manager{resolve: resolve, clients: map[string]*http.Client{}}
}

// Client returns the HTTP client for a pool id ("" or unresolvable pool →
// direct connection).
func (m *Manager) Client(poolID string) *http.Client {
	if poolID == "" {
		return m.directClient()
	}

	m.mu.Lock()
	if c, ok := m.clients[poolID]; ok {
		m.mu.Unlock()
		return c
	}
	m.mu.Unlock()

	// Resolve outside the lock (may hit the database).
	var urls []string
	if m.resolve != nil {
		var err error
		urls, err = m.resolve(context.Background(), poolID)
		if err != nil || len(urls) == 0 {
			// Unresolvable pool: fall back to direct rather than failing hard.
			return m.directClient()
		}
	} else {
		return m.directClient()
	}

	client := &http.Client{Transport: rotatingTransport(urls)}

	m.mu.Lock()
	// Another goroutine may have won the race; either client is fine.
	c, ok := m.clients[poolID]
	if !ok {
		m.clients[poolID] = client
		c = client
	}
	m.mu.Unlock()
	return c
}

// rotatingTransport builds a transport whose Proxy hook rotates through the
// pool's URLs. Go's http.Transport natively supports http, https, and socks5
// proxy URLs.
func rotatingTransport(urls []string) *http.Transport {
	parsed := make([]*url.URL, 0, len(urls))
	for _, u := range urls {
		if p, err := url.Parse(u); err == nil && p.Scheme != "" && p.Host != "" {
			parsed = append(parsed, p)
		}
	}
	if len(parsed) == 0 {
		return baseTransport()
	}
	var counter atomic.Uint64
	return withBaseTransport(func(*http.Request) (*url.URL, error) {
		return parsed[int(counter.Add(1)-1)%len(parsed)], nil
	})
}

// Invalidate drops the cached client for a pool. Call after the pool's proxy
// list changes so new traffic is routed through the updated proxies.
func (m *Manager) Invalidate(poolID string) {
	if poolID == "" {
		return
	}
	m.mu.Lock()
	delete(m.clients, poolID)
	m.mu.Unlock()
}

// NewProxiedClient builds an HTTP client that routes all traffic through a
// single proxy URL (http/https/socks5). Used by the dashboard's per-proxy
// live test. The returned error is non-nil for malformed proxy URLs.
func NewProxiedClient(proxyURL string) (*http.Client, error) {
	p, err := url.Parse(proxyURL)
	if err != nil || p.Scheme == "" || p.Host == "" {
		return nil, fmt.Errorf("invalid proxy URL %q", proxyURL)
	}
	return &http.Client{
		Transport: withBaseTransport(func(*http.Request) (*url.URL, error) { return p, nil }),
	}, nil
}

func (m *Manager) directClient() *http.Client {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.direct == nil {
		m.direct = &http.Client{Transport: baseTransport()}
	}
	return m.direct
}

func baseTransport() *http.Transport { return withBaseTransport(nil) }

func withBaseTransport(proxy func(*http.Request) (*url.URL, error)) *http.Transport {
	return &http.Transport{
		Proxy:                 proxy,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
}

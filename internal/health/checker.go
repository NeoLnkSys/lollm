package health

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/lollm/lollm/internal/db"
)

// Prober reports whether a connection can currently serve traffic. It is
// implemented by the API server (which owns the provider adapters).
type Prober interface {
	Probe(ctx context.Context, conn *db.Connection) error
}

// ProxyStatus is the last known state of one proxy URL in a pool.
type ProxyStatus struct {
	PoolID    string
	PoolName  string
	URL       string
	Up        bool
	LastCheck time.Time
	LastError string
}

const (
	defaultInterval = 60 * time.Second
	minInterval     = 5 * time.Second
	probeTimeout    = 20 * time.Second
	proxyTimeout    = 10 * time.Second
)

// Checker is the background health watcher: every interval it re-probes every
// user-enabled-but-unhealthy connection (restoring the ones that answer) and
// pings every proxy pool URL.
type Checker struct {
	store  *db.Store
	prober Prober
	log    *slog.Logger
	bus    *Bus

	mu      sync.Mutex
	proxies map[string]ProxyStatus // key: poolID + "\x00" + url
}

// NewChecker builds a health checker.
func NewChecker(store *db.Store, prober Prober, log *slog.Logger, bus *Bus) *Checker {
	return &Checker{
		store:   store,
		prober:  prober,
		log:     log,
		bus:     bus,
		proxies: map[string]ProxyStatus{},
	}
}

// Run loops until ctx is cancelled, honoring the live settings
// (health_check.enabled, health_check.interval_seconds) on every cycle.
func (c *Checker) Run(ctx context.Context) {
	for {
		interval := c.settingInterval(ctx)
		if c.settingEnabled(ctx) {
			c.CheckOnce(ctx)
		}
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// CheckOnce performs one full pass: connections then proxy pools.
func (c *Checker) CheckOnce(ctx context.Context) {
	interval := c.settingInterval(ctx)
	c.checkConnections(ctx, interval)
	c.checkProxies(ctx)
}

// checkConnections probes every user-enabled connection whose status is not
// active (regardless of backoff — a probe is one lightweight request and an
// early recovery is a win). Disabled connections (is_active=false) are never
// touched: the user turned them off on purpose.
func (c *Checker) checkConnections(ctx context.Context, interval time.Duration) {
	conns, err := c.store.ListConnections(ctx)
	if err != nil {
		c.log.Warn("health check: list connections failed", "err", err)
		return
	}
	for _, conn := range conns {
		if !conn.IsActive || conn.Status == db.StatusActive {
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		err := c.prober.Probe(pctx, conn)
		cancel()
		if err == nil {
			if err := c.store.MarkConnectionResult(ctx, conn.ID, true, "", nil, 0); err != nil {
				c.log.Warn("health check: restore failed", "connection", conn.Name, "err", err)
				continue
			}
			c.bus.Publish(EventConnectionRecovered, conn.ID, conn.Name,
				"probe succeeded — status restored to active")
			c.log.Info("health check: connection recovered",
				"connection", conn.Name, "provider", conn.Provider, "was", conn.Status)
			continue
		}
		// Still down: re-arm the backoff so routing keeps skipping it until
		// the next probe, without inflating the request error streak.
		until := time.Now().UTC().Add(interval)
		if err := c.store.SetConnectionStatus(ctx, conn.ID, conn.Status, &until); err != nil {
			c.log.Warn("health check: re-arm backoff failed", "connection", conn.Name, "err", err)
		}
		c.log.Debug("health check: connection still down",
			"connection", conn.Name, "provider", conn.Provider, "err", err.Error())
	}
}

// checkProxies pings every proxy URL in every pool directly. Any HTTP
// response counts as reachable; only network failures mark a proxy down.
// Transitions are published to the event bus.
func (c *Checker) checkProxies(ctx context.Context) {
	pools, err := c.store.ListProxyPools(ctx)
	if err != nil {
		c.log.Warn("health check: list proxy pools failed", "err", err)
		return
	}
	client := &http.Client{Timeout: proxyTimeout}
	for _, pool := range pools {
		for _, url := range pool.Proxies {
			up, detail := probeURL(ctx, client, url)
			key := pool.ID + "\x00" + url

			c.mu.Lock()
			prev, had := c.proxies[key]
			status := ProxyStatus{
				PoolID: pool.ID, PoolName: pool.Name, URL: url,
				Up: up, LastCheck: time.Now().UTC(), LastError: detail,
			}
			c.proxies[key] = status
			c.mu.Unlock()

			switch {
			case !had && !up:
				c.bus.Publish(EventProxyDown, pool.ID, pool.Name, url+" unreachable: "+detail)
			case had && prev.Up && !up:
				c.bus.Publish(EventProxyDown, pool.ID, pool.Name, url+" went unreachable: "+detail)
			case had && !prev.Up && up:
				c.bus.Publish(EventProxyUp, pool.ID, pool.Name, url+" reachable again")
			}
			if !up {
				c.log.Warn("health check: proxy unreachable", "pool", pool.Name, "url", url, "err", detail)
			}
		}
	}
}

// ProxyHealth returns a snapshot of the last known proxy states.
func (c *Checker) ProxyHealth() []ProxyStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ProxyStatus, 0, len(c.proxies))
	for _, s := range c.proxies {
		out = append(out, s)
	}
	return out
}

func probeURL(ctx context.Context, client *http.Client, url string) (up bool, errDetail string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err.Error()
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err.Error()
	}
	resp.Body.Close()
	// Any HTTP answer — even 4xx/5xx — proves the relay resolves, terminates
	// TLS, and answers; routing through it may still fail, but it is alive.
	return true, ""
}

// --- live settings -----------------------------------------------------------------

func (c *Checker) settingInterval(ctx context.Context) time.Duration {
	v, ok, err := c.store.GetSetting(ctx, db.SettingHealthCheckInterval)
	if err != nil || !ok {
		return defaultInterval
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs < int(minInterval.Seconds()) {
		return defaultInterval
	}
	return time.Duration(secs) * time.Second
}

func (c *Checker) settingEnabled(ctx context.Context) bool {
	v, ok, err := c.store.GetSetting(ctx, db.SettingHealthCheckEnabled)
	if err != nil || !ok {
		return true
	}
	return v != "false"
}

package health

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lollm/lollm/internal/db"
)

// fakeProber scripts probe results per connection id and records probes.
type fakeProber struct {
	mu      sync.Mutex
	results map[string]error
	probed  []string
}

func (f *fakeProber) Probe(_ context.Context, conn *db.Connection) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probed = append(f.probed, conn.ID)
	return f.results[conn.ID]
}

func (f *fakeProber) probedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.probed...)
}

func newTestChecker(t *testing.T) (*Checker, *fakeProber, *Bus, *db.Store) {
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
	prober := &fakeProber{results: map[string]error{}}
	bus := NewBus()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewChecker(store, prober, log, bus), prober, bus, store
}

func conn(id string, active bool, status string) *db.Connection {
	return &db.Connection{
		ID: id, Name: id, Provider: "openrouter", APIKeyEncrypted: "x",
		IsActive: active, Status: status, Priority: 10, Weight: 1,
	}
}

func nextEvent(t *testing.T, bus *Bus) Event {
	t.Helper()
	ch, cancel := bus.Subscribe()
	defer cancel()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
		return Event{}
	}
}

func TestUnhealthyConnectionRecovers(t *testing.T) {
	c, prober, bus, store := newTestChecker(t)
	ctx := context.Background()

	down := conn("down", true, db.StatusRateLimited)
	if err := store.CreateConnection(ctx, down); err != nil {
		t.Fatal(err)
	}
	// Subscribe before the check so the event cannot race past us.
	ch, cancel := bus.Subscribe()
	defer cancel()

	prober.results["down"] = nil // probe succeeds
	c.CheckOnce(ctx)

	got, err := store.GetConnection(ctx, "down")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != db.StatusActive || got.ConsecutiveErrors != 0 || got.BackoffUntil != nil {
		t.Fatalf("connection must be recovered: %+v", got)
	}
	select {
	case ev := <-ch:
		if ev.Type != EventConnectionRecovered || ev.ID != "down" {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("recovery event missing")
	}
}

func TestStillDownConnectionStaysDown(t *testing.T) {
	c, prober, _, store := newTestChecker(t)
	ctx := context.Background()

	down := conn("down", true, db.StatusUnavailable)
	store.CreateConnection(ctx, down)
	prober.results["down"] = errors.New("dial fail")

	c.CheckOnce(ctx)

	got, _ := store.GetConnection(ctx, "down")
	if got.Status != db.StatusUnavailable {
		t.Fatalf("still-down connection must keep its status: %+v", got)
	}
	if got.BackoffUntil == nil || !got.BackoffUntil.After(time.Now().UTC()) {
		t.Fatalf("backoff must be re-armed for the next probe: %+v", got)
	}
	// Probe failures must NOT inflate the request error streak.
	if got.ConsecutiveErrors != 0 {
		t.Fatalf("probe failure must not touch the error streak: %+v", got)
	}
}

func TestDisabledAndHealthyConnectionsSkipped(t *testing.T) {
	c, prober, _, store := newTestChecker(t)
	ctx := context.Background()

	store.CreateConnection(ctx, conn("off", false, db.StatusUnavailable)) // user-disabled
	store.CreateConnection(ctx, conn("ok", true, db.StatusActive))        // healthy
	store.CreateConnection(ctx, conn("sick", true, db.StatusRateLimited)) // target

	prober.results["sick"] = nil
	c.CheckOnce(ctx)

	probed := prober.probedIDs()
	if len(probed) != 1 || probed[0] != "sick" {
		t.Fatalf("only the unhealthy enabled connection must be probed: %v", probed)
	}
}

func TestProxyPoolChecksAndEvents(t *testing.T) {
	c, _, bus, store := newTestChecker(t)
	ctx := context.Background()

	good := httptest.NewServer(nil) // any response = reachable
	defer good.Close()

	pool := &db.ProxyPool{Name: "relays", Proxies: []string{good.URL, "http://127.0.0.1:1"}}
	if err := store.CreateProxyPool(ctx, pool); err != nil {
		t.Fatal(err)
	}

	// Subscribe BEFORE the check: the bus only delivers to live subscribers.
	ch, cancel := bus.Subscribe()
	c.CheckOnce(ctx)

	health := c.ProxyHealth()
	states := map[string]ProxyStatus{}
	for _, s := range health {
		states[s.URL] = s
	}
	if !states[good.URL].Up {
		t.Fatalf("live relay must be up: %+v", states)
	}
	if states["http://127.0.0.1:1"].Up {
		t.Fatal("dead relay must be down")
	}

	// First observation of a down proxy publishes exactly one event.
	var ev Event
	select {
	case ev = <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy.down event missing")
	}
	if ev.Type != EventProxyDown || ev.ID != pool.ID {
		t.Fatalf("unexpected event: %+v", ev)
	}
	select {
	case ev := <-ch:
		t.Fatalf("exactly one event expected on first observation: %+v", ev)
	case <-time.After(300 * time.Millisecond):
	}

	// A second check with unchanged states must not re-publish transitions.
	cancel()
	ch2, cancel2 := bus.Subscribe()
	defer cancel2()
	c.CheckOnce(ctx)
	select {
	case ev := <-ch2:
		t.Fatalf("no duplicate events expected: %+v", ev)
	case <-time.After(300 * time.Millisecond):
	}

	// A relay coming back up publishes proxy.up.
	if err := store.DeleteProxyPool(ctx, pool.ID); err != nil {
		t.Fatal(err)
	}
	pool2 := &db.ProxyPool{Name: "relays2", Proxies: []string{good.URL}}
	store.CreateProxyPool(ctx, pool2)
	// Pre-seed a previous "down" state for good.URL under pool2 so the up
	// transition fires.
	c.mu.Lock()
	c.proxies[pool2.ID+"\x00"+good.URL] = ProxyStatus{PoolID: pool2.ID, URL: good.URL, Up: false}
	c.mu.Unlock()

	c.CheckOnce(ctx)
	select {
	case ev = <-ch2:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy.up event missing")
	}
	if ev.Type != EventProxyUp {
		t.Fatalf("expected proxy.up, got %+v", ev)
	}
}

func TestBusRecent(t *testing.T) {
	bus := NewBus()
	for i := 0; i < 130; i++ {
		bus.Publish("t", "id", "name", "d")
	}
	recent := bus.Recent(0)
	if len(recent) != recentCap {
		t.Fatalf("recent must cap at %d: %d", recentCap, len(recent))
	}
	if bus.Recent(5)[0].ID != "id" {
		t.Fatal("Recent(n) must return the newest n events")
	}
}

func TestNilBusIsSafe(t *testing.T) {
	var bus *Bus
	bus.Publish("x", "y", "z", "w") // must not panic
	if bus.Recent(10) != nil {
		t.Fatal("nil bus has no events")
	}
	ch, cancel := bus.Subscribe()
	cancel()
	if _, ok := <-ch; ok {
		t.Fatal("nil bus channel must be closed")
	}
}

func TestSettingsDriveIntervalAndEnablement(t *testing.T) {
	c, _, _, store := newTestChecker(t)
	ctx := context.Background()

	// Defaults when nothing is set.
	if got := c.settingInterval(ctx); got != defaultInterval {
		t.Fatalf("default interval: %v", got)
	}
	if !c.settingEnabled(ctx) {
		t.Fatal("health check must default to enabled")
	}

	// Live settings.
	store.SetSetting(ctx, db.SettingHealthCheckInterval, "15")
	store.SetSetting(ctx, db.SettingHealthCheckEnabled, "false")
	if got := c.settingInterval(ctx); got != 15*time.Second {
		t.Fatalf("interval from settings: %v", got)
	}
	if c.settingEnabled(ctx) {
		t.Fatal("health check must be disableable")
	}

	// Nonsense values fall back to defaults.
	store.SetSetting(ctx, db.SettingHealthCheckInterval, "banana")
	if got := c.settingInterval(ctx); got != defaultInterval {
		t.Fatalf("bad interval must fall back: %v", got)
	}
	store.SetSetting(ctx, db.SettingHealthCheckInterval, "1") // below minimum
	if got := c.settingInterval(ctx); got != defaultInterval {
		t.Fatalf("below-minimum interval must fall back: %v", got)
	}
}

// Package health implements the background health watcher (spec 3.2/3.4):
// it periodically probes unhealthy connections and restores them to active,
// checks proxy pool reachability, and publishes status events on an in-memory
// bus (consumed later by the dashboard SSE feed).
package health

import (
	"sync"
	"time"
)

// Event types published on the bus.
const (
	EventConnectionRecovered = "connection.recovered"
	EventConnectionFailed    = "connection.failed"
	EventProxyUp             = "proxy.up"
	EventProxyDown           = "proxy.down"
)

// Event is one status transition worth surfacing to the dashboard.
type Event struct {
	Type   string
	ID     string
	Name   string
	Detail string
	At     time.Time
}

const (
	subBuffer = 64 // per-subscriber channel buffer
	recentCap = 100
)

// Bus is a tiny in-memory pub/sub. A nil *Bus is usable: Publish no-ops and
// Subscribe returns a closed channel, so callers without a bus need no nil
// checks.
type Bus struct {
	mu     sync.Mutex
	subs   map[chan Event]struct{}
	recent []Event
}

// NewBus builds an event bus.
func NewBus() *Bus {
	return &Bus{subs: make(map[chan Event]struct{})}
}

// Publish broadcasts an event to all subscribers (non-blocking; events are
// dropped for subscribers that cannot keep up) and keeps the most recent
// events for late joiners.
func (b *Bus) Publish(typ, id, name, detail string) {
	if b == nil {
		return
	}
	ev := Event{Type: typ, ID: id, Name: name, Detail: detail, At: time.Now().UTC()}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.recent = append(b.recent, ev)
	if len(b.recent) > recentCap {
		b.recent = b.recent[len(b.recent)-recentCap:]
	}
	for ch := range b.subs {
		select {
		case ch <- ev:
		default: // subscriber too slow — drop rather than block the gateway
		}
	}
}

// Subscribe returns a channel of future events plus a cancel function.
func (b *Bus) Subscribe() (<-chan Event, func()) {
	if b == nil {
		ch := make(chan Event)
		close(ch)
		return ch, func() {}
	}
	ch := make(chan Event, subBuffer)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	cancel := func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
	return ch, cancel
}

// Recent returns a copy of the most recent events (oldest first), for
// dashboard initial state.
func (b *Bus) Recent(n int) []Event {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if n <= 0 || n > len(b.recent) {
		n = len(b.recent)
	}
	out := make([]Event, n)
	copy(out, b.recent[len(b.recent)-n:])
	return out
}

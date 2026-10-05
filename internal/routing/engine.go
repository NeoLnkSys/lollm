package routing

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lollm/lollm/internal/db"
)

// Scoring weights for the health_aware strategy (tunable later via settings).
const (
	wPriority  = 2.0 // 1/priority — the combo author's intent
	wFreshness = 1.0 // least-recently-used first (load balancing)
	wErrors    = 1.0 // avoid error-prone connections
	wLatency   = 1.0 // avoid slow connections
)

// Engine plans routes. It is safe for concurrent use.
type Engine struct {
	now func() time.Time

	mu        sync.Mutex
	rrCounter map[string]*atomic.Uint64 // combo-level round-robin cursor
	wrrState  map[string][]wrrEntry     // smooth WRR state per group
	sticky    map[string]stickyEntry    // sessionKey -> pinned group+connection
}

type stickyEntry struct {
	groupKey string
	connID   string
	expires  time.Time
}

type wrrEntry struct {
	weight  int
	current int
}

// NewEngine builds a routing engine using the system clock.
func NewEngine() *Engine {
	return NewEngineWithClock(time.Now)
}

// NewEngineWithClock builds an engine with an injectable clock (tests).
func NewEngineWithClock(now func() time.Time) *Engine {
	return &Engine{
		now:       now,
		rrCounter: make(map[string]*atomic.Uint64),
		wrrState:  make(map[string][]wrrEntry),
		sticky:    make(map[string]stickyEntry),
	}
}

// Resolve turns a combo into an ordered list of routable groups for the
// request, applying the mandatory skip rules (spec section 3.3/3.4):
// inactive, non-active status, or backing-off connections are never included;
// models without the requested capabilities are skipped as well.
func (e *Engine) Resolve(combo *db.Combo, conns []*db.Connection, req *Request) ([]*Group, *Report, error) {
	report := &Report{Total: len(combo.Models)}
	now := e.now()

	byID := make(map[string]*db.Connection, len(conns))
	for _, c := range conns {
		byID[c.ID] = c
	}

	// Build groups of eligible connections per provider+model.
	groups := make(map[string]*Group)
	var order []*Group
	for _, m := range combo.Models {
		conn, ok := byID[m.ConnectionID]
		if !ok {
			report.Skipped = append(report.Skipped, SkipReason{
				ConnectionID: m.ConnectionID, Model: m.Model, Reason: "connection not found (deleted?)"})
			continue
		}
		if reason := e.skipReason(conn, m.Model, req, now); reason != "" {
			report.Skipped = append(report.Skipped, SkipReason{
				ConnectionID: conn.ID, Model: m.Model, Reason: reason})
			continue
		}

		key := conn.Provider + "|" + m.Model
		g, ok := groups[key]
		if !ok {
			g = &Group{Provider: conn.Provider, Model: m.Model, Priority: m.Priority}
			groups[key] = g
			order = append(order, g)
		}
		g.Connections = append(g.Connections, conn)
		if m.Priority < g.Priority {
			g.Priority = m.Priority
		}
		report.Eligible++
	}
	if len(order) == 0 {
		return nil, report, ErrNoCandidates
	}

	// Order groups according to the combo strategy.
	strategy := combo.Strategy
	if strategy == "" {
		strategy = db.StrategyHealthAware
	}
	switch strategy {
	case db.StrategySequential:
		sortByPriority(order)
	case db.StrategyRoundRobin:
		sortByPriority(order)
		rotate(order, int(e.rrNext(combo.Name)))
	case db.StrategySticky:
		sortByPriority(order)
		e.pinStickyGroup(order, req)
	case db.StrategyHealthAware, db.StrategyFusion:
		// fusion fans out to several groups at request time (phase P7);
		// ordering uses the same health scoring as health_aware.
		sortByScore(order, now)
	default:
		sortByScore(order, now)
	}
	return order, report, nil
}

// PickFromGroup selects one connection within a group via smooth weighted
// round-robin (multi-account rotation). exclude removes connections that
// already failed during this request's fallback chain. For sticky combos,
// the session's pinned connection is honored while it stays in the pool.
func (e *Engine) PickFromGroup(g *Group, sessionKey string, exclude map[string]bool) *db.Connection {
	pool := make([]*db.Connection, 0, len(g.Connections))
	for _, c := range g.Connections {
		if !exclude[c.ID] {
			pool = append(pool, c)
		}
	}
	if len(pool) == 0 {
		return nil
	}

	if sessionKey != "" {
		if c := e.stickyPick(g, sessionKey, pool); c != nil {
			return c
		}
		c := e.wrrPick(g.Key(), pool)
		e.pinConnection(sessionKey, g.Key(), c.ID)
		return c
	}
	return e.wrrPick(g.Key(), pool)
}

// BuildAdHocCombo builds an implicit health-aware combo from every connection
// that lists the model in its catalog. Used when a request names a plain model
// instead of a combo.
func BuildAdHocCombo(model string, conns []*db.Connection) *db.Combo {
	combo := &db.Combo{Name: "model:" + model, Strategy: db.StrategyHealthAware}
	for _, c := range conns {
		for _, m := range c.Models() {
			if m == model {
				combo.Models = append(combo.Models, db.ComboModel{
					ConnectionID: c.ID, Model: m, Priority: 100,
				})
			}
		}
	}
	return combo
}

// --- eligibility -------------------------------------------------------------

// skipReason returns "" when the connection is routable for this model+request,
// or a human-readable reason why not.
func (e *Engine) skipReason(c *db.Connection, model string, req *Request, now time.Time) string {
	if !c.IsActive {
		return "connection is disabled (is_active=false)"
	}
	if c.Status != db.StatusActive {
		return "connection status: " + c.Status
	}
	if c.BackoffUntil != nil && c.BackoffUntil.After(now) {
		return "backing off until " + c.BackoffUntil.UTC().Format(time.RFC3339)
	}
	if !supports(ModelCapabilities(model), req.Capabilities) {
		return "model does not support requested capability: " + capabilityGap(model, req)
	}
	return ""
}

func capabilityGap(model string, req *Request) string {
	caps := ModelCapabilities(model)
	var missing []string
	if req.Capabilities.Vision && !caps.Vision {
		missing = append(missing, "vision")
	}
	if req.Capabilities.Audio && !caps.Audio {
		missing = append(missing, "audio")
	}
	if req.Capabilities.ToolCalling && !caps.ToolCalling {
		missing = append(missing, "tool_calling")
	}
	if req.Capabilities.StructuredOutput && !caps.StructuredOutput {
		missing = append(missing, "structured_output")
	}
	return strings.Join(missing, ",")
}

// --- ordering ------------------------------------------------------------------

func sortByPriority(groups []*Group) {
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].Priority != groups[j].Priority {
			return groups[i].Priority < groups[j].Priority
		}
		return groups[i].Key() < groups[j].Key()
	})
}

func sortByScore(groups []*Group, now time.Time) {
	sort.SliceStable(groups, func(i, j int) bool {
		si, sj := groupScore(groups[i], now), groupScore(groups[j], now)
		if si != sj {
			return si > sj
		}
		if groups[i].Priority != groups[j].Priority {
			return groups[i].Priority < groups[j].Priority
		}
		return groups[i].Key() < groups[j].Key()
	})
}

// groupScore computes the health_aware score of a group: the best score among
// its connections.
func groupScore(g *Group, now time.Time) float64 {
	best := 0.0
	for _, c := range g.Connections {
		if s := connScore(c, g.Priority, now); s > best {
			best = s
		}
	}
	return best
}

// connScore: higher is better.
//
//	2/priority            — the combo author's priority intent dominates
//	+ idle freshness      — least-recently-used first (saturates at 30s idle)
//	+ 1/(1+error streak)  — avoid flaky connections
//	+ 1/(1+latency_s)     — avoid slow connections
func connScore(c *db.Connection, priority int, now time.Time) float64 {
	score := wPriority / float64(max(priority, 1))

	if c.LastUsedAt == nil {
		score += wFreshness // never used = freshest
	} else {
		idle := now.Sub(*c.LastUsedAt).Seconds()
		if idle < 0 {
			idle = 0
		}
		score += wFreshness * min(idle/30.0, 1.0)
	}

	score += wErrors / float64(c.ConsecutiveErrors+1)
	score += wLatency / (1.0 + c.LatencyEMAMs/1000.0)
	return score
}

// --- rotation -------------------------------------------------------------------

func (e *Engine) rrNext(key string) uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.rrCounter[key]
	if !ok {
		c = &atomic.Uint64{}
		e.rrCounter[key] = c
	}
	return c.Add(1) - 1
}

// wrrPick implements smooth weighted round-robin (the nginx algorithm) over
// the pool, using each connection's Weight.
func (e *Engine) wrrPick(key string, pool []*db.Connection) *db.Connection {
	e.mu.Lock()
	defer e.mu.Unlock()

	state := e.wrrState[key]
	if len(state) != len(pool) || weightsChanged(state, pool) {
		state = make([]wrrEntry, len(pool))
		for i, c := range pool {
			state[i].weight = max(c.Weight, 1)
		}
		e.wrrState[key] = state
	}

	total := 0
	best := 0
	for i := range state {
		state[i].current += state[i].weight
		total += state[i].weight
		if state[i].current > state[best].current {
			best = i
		}
	}
	state[best].current -= total
	return pool[best]
}

func weightsChanged(state []wrrEntry, pool []*db.Connection) bool {
	for i, c := range pool {
		if state[i].weight != max(c.Weight, 1) {
			return true
		}
	}
	return false
}

// --- sticky sessions --------------------------------------------------------------

// pinStickyGroup moves the session's pinned group (if any and still routable)
// to the front, so the conversation keeps hitting the same target.
func (e *Engine) pinStickyGroup(order []*Group, req *Request) {
	if req.SessionKey == "" {
		return
	}
	e.mu.Lock()
	entry, ok := e.sticky[req.SessionKey]
	if ok && entry.expires.Before(e.now()) {
		delete(e.sticky, req.SessionKey)
		ok = false
	}
	e.mu.Unlock()
	if !ok {
		return
	}
	for i, g := range order {
		if g.Key() == entry.groupKey {
			if i > 0 {
				copy(order[1:i+1], order[:i])
				order[0] = g
			}
			return
		}
	}
}

func (e *Engine) stickyPick(g *Group, sessionKey string, pool []*db.Connection) *db.Connection {
	e.mu.Lock()
	defer e.mu.Unlock()
	entry, ok := e.sticky[sessionKey]
	if !ok || entry.expires.Before(e.now()) {
		return nil
	}
	if entry.groupKey != g.Key() {
		return nil
	}
	for _, c := range pool {
		if c.ID == entry.connID {
			entry.expires = e.now().Add(StickyTTL)
			e.sticky[sessionKey] = entry
			return c
		}
	}
	return nil
}

func (e *Engine) pinConnection(sessionKey, groupKey, connID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sticky[sessionKey] = stickyEntry{
		groupKey: groupKey,
		connID:   connID,
		expires:  e.now().Add(StickyTTL),
	}
}

func rotate[T any](s []T, by int) {
	if len(s) < 2 {
		return
	}
	by %= len(s)
	if by <= 0 {
		return
	}
	tmp := make([]T, len(s))
	copy(tmp, s)
	copy(s, tmp[by:])
	copy(s[len(s)-by:], tmp[:by])
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

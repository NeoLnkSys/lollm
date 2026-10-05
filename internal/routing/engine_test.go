package routing

import (
	"testing"
	"time"

	"github.com/lollm/lollm/internal/db"
)

// conn is a terse connection builder for tests.
func conn(id, provider string, mut ...func(*db.Connection)) *db.Connection {
	c := &db.Connection{
		ID: id, Name: id, Provider: provider,
		IsActive: true, Status: db.StatusActive, Priority: 100, Weight: 1,
	}
	for _, m := range mut {
		m(c)
	}
	return c
}

func inactive(c *db.Connection)    { c.IsActive = false }
func rateLimited(c *db.Connection) { c.Status = db.StatusRateLimited }
func unavailable(c *db.Connection) { c.Status = db.StatusUnavailable }
func prio(p int) func(*db.Connection) {
	return func(c *db.Connection) { c.Priority = p }
}

func backoff(d time.Duration) func(*db.Connection) {
	return func(c *db.Connection) {
		t := time.Now().UTC().Add(d)
		c.BackoffUntil = &t
	}
}

func errStreak(n int) func(*db.Connection) {
	return func(c *db.Connection) { c.ConsecutiveErrors = n }
}

func usedAgo(d time.Duration) func(*db.Connection) {
	return func(c *db.Connection) {
		t := time.Now().UTC().Add(-d)
		c.LastUsedAt = &t
	}
}

func combo(strategy string, models ...db.ComboModel) *db.Combo {
	return &db.Combo{Name: "test", Strategy: strategy, Models: models}
}

func entry(c *db.Connection, model string, priority int) db.ComboModel {
	return db.ComboModel{ConnectionID: c.ID, Model: model, Priority: priority}
}

var plainReq = &Request{}

func mustResolve(t *testing.T, e *Engine, c *db.Combo, conns []*db.Connection, req *Request) []*Group {
	t.Helper()
	groups, report, err := e.Resolve(c, conns, req)
	if err != nil {
		t.Fatalf("Resolve failed: %v (report: %+v)", err, report)
	}
	return groups
}

// --- eligibility (the mandatory skip rules) -----------------------------------

func TestResolveSkipsUnhealthyConnections(t *testing.T) {
	e := NewEngine()
	conns := []*db.Connection{
		conn("ok", "openrouter"),
		conn("disabled", "openrouter", inactive),
		conn("rl", "openrouter", rateLimited),
		conn("unavail", "openrouter", unavailable),
		conn("backoff", "openrouter", backoff(10*time.Minute)),
		conn("recovered", "openrouter", backoff(-time.Minute)), // backoff expired
	}
	c := combo(db.StrategySequential)
	for _, cc := range conns {
		c.Models = append(c.Models, entry(cc, "m", 10))
	}

	groups, report, err := e.Resolve(c, conns, plainReq)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	ids := map[string]bool{}
	for _, g := range groups {
		for _, cc := range g.Connections {
			ids[cc.ID] = true
		}
	}
	if !ids["ok"] || !ids["recovered"] {
		t.Fatalf("healthy connections missing: %v", ids)
	}
	for _, bad := range []string{"disabled", "rl", "unavail", "backoff"} {
		if ids[bad] {
			t.Fatalf("unhealthy connection %s must be skipped", bad)
		}
	}
	if report.Total != 6 || report.Eligible != 2 || len(report.Skipped) != 4 {
		t.Fatalf("report mismatch: %+v", report)
	}
}

func TestResolveMissingConnectionIsSkipped(t *testing.T) {
	e := NewEngine()
	c := combo(db.StrategySequential, db.ComboModel{ConnectionID: "ghost", Model: "m", Priority: 1})
	_, report, err := e.Resolve(c, nil, plainReq)
	if err != ErrNoCandidates {
		t.Fatalf("expected ErrNoCandidates, got %v", err)
	}
	if len(report.Skipped) != 1 || report.Skipped[0].ConnectionID != "ghost" {
		t.Fatalf("ghost connection should be reported: %+v", report)
	}
}

func TestResolveAllUnhealthyReturnsNoCandidates(t *testing.T) {
	e := NewEngine()
	c1 := conn("a", "groq", rateLimited)
	c2 := conn("b", "groq", backoff(5*time.Minute))
	c := combo(db.StrategyHealthAware, entry(c1, "llama-3.3-70b-versatile", 1), entry(c2, "llama-3.1-8b-instant", 2))
	_, _, err := e.Resolve(c, []*db.Connection{c1, c2}, plainReq)
	if err != ErrNoCandidates {
		t.Fatalf("expected ErrNoCandidates, got %v", err)
	}
}

// --- capability adapter ---------------------------------------------------------

func TestResolveCapabilityFilter(t *testing.T) {
	e := NewEngine()
	gemini := conn("g", "openrouter")
	llama := conn("l", "groq") // text-only in the catalog
	conns := []*db.Connection{gemini, llama}
	c := combo(db.StrategySequential,
		entry(llama, "llama-3.3-70b-versatile", 1),
		entry(gemini, "google/gemini-2.5-flash", 2))

	// Vision request must skip the text-only model.
	groups := mustResolve(t, e, c, conns, &Request{Capabilities: Capabilities{Vision: true}})
	if len(groups) != 1 || groups[0].Model != "google/gemini-2.5-flash" {
		t.Fatalf("vision request must only route to gemini: %+v", groups)
	}

	// Plain text request can use both.
	groups = mustResolve(t, e, c, conns, plainReq)
	if len(groups) != 2 {
		t.Fatalf("text request should see both models: %+v", groups)
	}

	// Tool-calling request: both catalog entries support tools.
	groups = mustResolve(t, e, c, conns, &Request{Capabilities: Capabilities{ToolCalling: true}})
	if len(groups) != 2 {
		t.Fatalf("tool request should pass both: %+v", groups)
	}
}

func TestUnknownModelIsPermissive(t *testing.T) {
	// Unknown models must never be blocked (false-allow beats false-deny).
	caps := ModelCapabilities("totally-unknown-model-x")
	if !caps.Vision || !caps.ToolCalling {
		t.Fatalf("unknown model should be assumed capable: %+v", caps)
	}
	// Vendor prefix is stripped for lookup.
	caps = ModelCapabilities("anthropic/llama-3.3-70b-versatile")
	if caps.Vision {
		t.Fatalf("llama-3.3 must stay text-only even with vendor prefix: %+v", caps)
	}
}

func TestDetectCapabilities(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "what is this?"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "x"}},
			}},
		},
		"tools":           []any{map[string]any{"type": "function"}},
		"response_format": map[string]any{"type": "json_schema"},
	}
	caps := DetectCapabilities(body)
	if !caps.Vision || !caps.ToolCalling || !caps.StructuredOutput {
		t.Fatalf("detection failed: %+v", caps)
	}
	if caps.Audio {
		t.Fatal("no audio parts present")
	}

	audioBody := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "input_audio"},
		}}},
	}
	if !DetectCapabilities(audioBody).Audio {
		t.Fatal("input_audio part should set Audio")
	}

	if !(DetectCapabilities(map[string]any{}) == Capabilities{}) {
		t.Fatal("empty body must detect no capabilities")
	}
}

// --- grouping --------------------------------------------------------------------

func TestResolveGroupsSameProviderModel(t *testing.T) {
	e := NewEngine()
	or1 := conn("or1", "openrouter")
	or2 := conn("or2", "openrouter")
	groq := conn("gq", "groq")
	conns := []*db.Connection{or1, or2, groq}
	c := combo(db.StrategySequential,
		entry(or1, "anthropic/claude-sonnet-4", 1),
		entry(or2, "anthropic/claude-sonnet-4", 1), // same target, second account
		entry(groq, "llama-3.3-70b-versatile", 2))

	groups := mustResolve(t, e, c, conns, plainReq)
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups (claude group + llama group), got %d", len(groups))
	}
	if groups[0].Key() != "openrouter|anthropic/claude-sonnet-4" || len(groups[0].Connections) != 2 {
		t.Fatalf("multi-account group wrong: %+v", groups[0])
	}
}

// --- strategies --------------------------------------------------------------------

func TestSequentialOrdering(t *testing.T) {
	e := NewEngine()
	c1 := conn("c1", "openrouter")
	c2 := conn("c2", "groq")
	c3 := conn("c3", "mistral")
	conns := []*db.Connection{c1, c2, c3}
	c := combo(db.StrategySequential,
		entry(c3, "m3", 30),
		entry(c1, "m1", 10),
		entry(c2, "m2", 20))

	for i, want := range []string{"openrouter|m1", "groq|m2", "mistral|m3"} {
		groups := mustResolve(t, e, c, conns, plainReq)
		if groups[i].Key() != want {
			t.Fatalf("position %d: got %s want %s", i, groups[i].Key(), want)
		}
	}
}

func TestRoundRobinRotation(t *testing.T) {
	e := NewEngine()
	c1 := conn("c1", "openrouter")
	c2 := conn("c2", "groq")
	c3 := conn("c3", "mistral")
	conns := []*db.Connection{c1, c2, c3}
	c := combo(db.StrategyRoundRobin,
		entry(c1, "m1", 10), entry(c2, "m2", 20), entry(c3, "m3", 30))

	first := mustResolve(t, e, c, conns, plainReq)[0].Key()
	seen := map[string]int{first: 1}
	for i := 0; i < 8; i++ {
		got := mustResolve(t, e, c, conns, plainReq)[0].Key()
		seen[got]++
	}
	if len(seen) != 3 {
		t.Fatalf("round_robin should rotate through all groups: %v", seen)
	}
	for k, n := range seen {
		if n != 3 { // 9 calls / 3 groups
			t.Fatalf("round_robin distribution uneven for %s: %d", k, n)
		}
	}
}

func TestHealthAwarePrefersPriorityWhenEqualHealth(t *testing.T) {
	e := NewEngine()
	p1 := conn("p1", "openrouter")
	p2 := conn("p2", "groq")
	conns := []*db.Connection{p1, p2}
	c := combo(db.StrategyHealthAware,
		entry(p2, "m2", 50),
		entry(p1, "m1", 1))

	groups := mustResolve(t, e, c, conns, plainReq)
	if groups[0].Key() != "openrouter|m1" {
		t.Fatalf("priority 1 should lead when equally healthy: %+v", groups[0])
	}
}

func TestHealthAwareAvoidsFlakyAndSlow(t *testing.T) {
	e := NewEngine()
	flaky := conn("flaky", "openrouter", errStreak(10), usedAgo(0), func(c *db.Connection) { c.LatencyEMAMs = 5000 })
	clean := conn("clean", "groq")
	conns := []*db.Connection{flaky, clean}
	// Same priority — health must decide.
	c := combo(db.StrategyHealthAware,
		entry(flaky, "m1", 10), entry(clean, "m2", 10))

	groups := mustResolve(t, e, c, conns, plainReq)
	if groups[0].Key() != "groq|m2" {
		t.Fatalf("clean connection should outrank flaky+slow: %+v", groups[0])
	}
}

func TestHealthAwarePrefersLeastRecentlyUsed(t *testing.T) {
	e := NewEngine()
	justUsed := conn("just", "openrouter", usedAgo(0))
	idle := conn("idle", "groq", usedAgo(10*time.Minute))
	conns := []*db.Connection{justUsed, idle}
	// Different models so they form separate groups; equal priority.
	c := combo(db.StrategyHealthAware,
		entry(justUsed, "m1", 10), entry(idle, "m2", 10))

	groups := mustResolve(t, e, c, conns, plainReq)
	if groups[0].Connections[0].ID != "idle" {
		t.Fatalf("idle connection should be preferred for load balancing: %+v", groups[0])
	}
}

func TestStickyPinsGroupPerSession(t *testing.T) {
	base := time.Now()
	now := base
	e := NewEngineWithClock(func() time.Time { return now })

	p1 := conn("p1", "openrouter")
	p2 := conn("p2", "groq")
	conns := []*db.Connection{p1, p2}
	c := combo(db.StrategySticky,
		entry(p1, "m1", 1), entry(p2, "m2", 2))

	// Full routing flow: Resolve orders groups, PickFromGroup pins the
	// session to the group+connection actually used.
	g1 := mustResolve(t, e, c, conns, &Request{SessionKey: "conv-A"})
	if g1[0].Key() != "openrouter|m1" {
		t.Fatalf("healthy priority-1 group should serve first: %s", g1[0].Key())
	}
	e.PickFromGroup(g1[0], "conv-A", nil) // pins openrouter|m1

	// The pinned provider goes rate-limited: the session falls back to p2
	// and (by design) re-pins there — sticky LBs migrate with the session.
	p1.Status = db.StatusRateLimited
	g2 := mustResolve(t, e, c, conns, &Request{SessionKey: "conv-A"})
	if g2[0].Key() != "groq|m2" {
		t.Fatalf("fallback should use p2: %s", g2[0].Key())
	}
	e.PickFromGroup(g2[0], "conv-A", nil) // re-pins groq|m2
	p1.Status = db.StatusActive

	// p1 is healthy again, but the session sticks to its pinned group.
	g3 := mustResolve(t, e, c, conns, &Request{SessionKey: "conv-A"})
	if g3[0].Key() != "groq|m2" {
		t.Fatalf("session A should stick to re-pinned p2: %s", g3[0].Key())
	}

	// After the TTL the pin expires and priority order returns.
	now = base.Add(2 * StickyTTL)
	g4 := mustResolve(t, e, c, conns, &Request{SessionKey: "conv-A"})
	if g4[0].Key() != "openrouter|m1" {
		t.Fatalf("expired pin should revert to priority order: %s", g4[0].Key())
	}
}

// --- multi-account rotation (WRR) ---------------------------------------------------

func TestWeightedRoundRobinDistribution(t *testing.T) {
	e := NewEngine()
	light := conn("light", "openrouter", func(c *db.Connection) { c.Weight = 1 })
	heavy := conn("heavy", "openrouter", func(c *db.Connection) { c.Weight = 3 })
	g := &Group{Provider: "openrouter", Model: "m", Connections: []*db.Connection{light, heavy}}

	counts := map[string]int{}
	for i := 0; i < 400; i++ {
		c := e.PickFromGroup(g, "", nil)
		if c == nil {
			t.Fatal("pick returned nil")
		}
		counts[c.ID]++
	}
	if counts["light"] != 100 || counts["heavy"] != 300 {
		t.Fatalf("weights 1:3 must split 25/75, got %v", counts)
	}
}

func TestPickFromGroupExcludesFailedConnections(t *testing.T) {
	e := NewEngine()
	c1 := conn("c1", "openrouter")
	c2 := conn("c2", "openrouter")
	g := &Group{Provider: "openrouter", Model: "m", Connections: []*db.Connection{c1, c2}}

	first := e.PickFromGroup(g, "", nil)
	second := e.PickFromGroup(g, "", map[string]bool{first.ID: true})
	if second == nil || second.ID == first.ID {
		t.Fatalf("fallback pick must return a different connection: first=%v second=%v", first, second)
	}
	if got := e.PickFromGroup(g, "", map[string]bool{"c1": true, "c2": true}); got != nil {
		t.Fatalf("all excluded must return nil, got %v", got)
	}
}

func TestStickyPinsConnectionWithinGroup(t *testing.T) {
	e := NewEngine()
	c1 := conn("c1", "openrouter")
	c2 := conn("c2", "openrouter")
	g := &Group{Provider: "openrouter", Model: "m", Connections: []*db.Connection{c1, c2}}

	first := e.PickFromGroup(g, "conv-A", nil)
	for i := 0; i < 10; i++ {
		if got := e.PickFromGroup(g, "conv-A", nil); got.ID != first.ID {
			t.Fatalf("sticky session must keep the same connection: %s != %s", got.ID, first.ID)
		}
	}
	// A different session should be free to rotate.
	other := e.PickFromGroup(g, "conv-B", nil)
	if other == nil {
		t.Fatal("second session must also get a connection")
	}
}

// --- ad-hoc combos (plain model names) --------------------------------------------

func TestBuildAdHocCombo(t *testing.T) {
	or := conn("or", "openrouter", func(c *db.Connection) {
		c.ModelsJSON = `["anthropic/claude-sonnet-4", "google/gemini-2.5-flash"]`
	})
	gq := conn("gq", "groq", func(c *db.Connection) {
		c.ModelsJSON = `["llama-3.3-70b-versatile"]`
	})

	combo := BuildAdHocCombo("google/gemini-2.5-flash", []*db.Connection{or, gq})
	if len(combo.Models) != 1 || combo.Models[0].ConnectionID != "or" {
		t.Fatalf("ad-hoc combo wrong: %+v", combo.Models)
	}
	if combo.Strategy != db.StrategyHealthAware {
		t.Fatalf("ad-hoc combo must be health_aware")
	}

	// Ad-hoc routing works end to end.
	e := NewEngine()
	groups := mustResolve(t, e, combo, []*db.Connection{or, gq}, plainReq)
	if len(groups) != 1 || groups[0].Connections[0].ID != "or" {
		t.Fatalf("ad-hoc resolve failed: %+v", groups)
	}
}

func TestResolveReportHasSkipReasons(t *testing.T) {
	e := NewEngine()
	c1 := conn("c1", "groq", rateLimited)
	c := combo(db.StrategySequential, entry(c1, "llama-3.3-70b-versatile", 1))
	_, report, _ := e.Resolve(c, []*db.Connection{c1}, &Request{Capabilities: Capabilities{Vision: true}})
	if len(report.Skipped) != 1 {
		t.Fatalf("expected 1 skip, got %+v", report.Skipped)
	}
	if report.Skipped[0].Reason == "" {
		t.Fatal("skip reason must be populated")
	}
}

// Package routing is the LoLLM routing engine (spec section 3.4): it turns a
// combo + the current connection states into an ordered list of routable
// targets, skipping anything that is inactive, unhealthy, backing off, or
// incapable of serving the request.
package routing

import (
	"errors"
	"time"

	"github.com/lollm/lollm/internal/db"
)

// Capabilities describes what a request needs from a model.
type Capabilities struct {
	Vision           bool
	Audio            bool
	ToolCalling      bool
	StructuredOutput bool
}

// Request is the routing view of an incoming chat request.
type Request struct {
	// Capabilities the request needs (detected from the OpenAI body).
	Capabilities Capabilities
	// SessionKey identifies a conversation for sticky routing ("" = none).
	SessionKey string
}

// Group is one routable target: a provider+model pair plus every eligible
// connection (account/key) that can serve it. Multiple connections in a group
// is how multi-account rotation works.
type Group struct {
	Provider    string
	Model       string
	Priority    int              // best (lowest) priority among members
	Connections []*db.Connection // eligible connections, state-checked
}

// Key identifies the group ("provider|model").
func (g *Group) Key() string { return g.Provider + "|" + g.Model }

// SkipReason records why a combo entry was not routable (diagnostics for the
// API layer and dashboard).
type SkipReason struct {
	ConnectionID string
	Model        string
	Reason       string
}

// Report summarizes one Resolve call.
type Report struct {
	Total    int // combo entries considered
	Eligible int // entries that passed all checks
	Skipped  []SkipReason
}

// ErrNoCandidates is returned when no combo entry is currently routable.
var ErrNoCandidates = errors.New("routing: no eligible connection/model for this request")

// StickyTTL is how long a sticky session pin survives without use.
const StickyTTL = 30 * time.Minute

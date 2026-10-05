// Package providers translates OpenAI-compatible requests to upstream LLM
// providers and returns OpenAI-format responses. Almost every supported
// provider exposes an OpenAI-compatible API, so one generic adapter plus a
// metadata table covers them all; native adapters can be added later.
package providers

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/lollm/lollm/internal/db"
)

// ChatRequest is a decoded OpenAI-compatible chat request, ready to be
// translated for a specific provider.
type ChatRequest struct {
	// Body is the client's decoded request body (never nil). The adapter
	// rewrites "model" with Model below; all other fields pass through.
	Body map[string]any
	// Model is the target model name at the provider (from the routing engine).
	Model string
	// Stream mirrors body["stream"].
	Stream bool
}

// ChatResponse is a provider response already in OpenAI format.
type ChatResponse struct {
	Status int
	// Stream is true when Body is an SSE event stream.
	Stream bool
	// Body is the raw response body: an SSE stream (Stream=true) or JSON
	// (Stream=false), already OpenAI-shaped. The caller must Close it.
	Body io.ReadCloser
}

// ErrKind classifies provider failures for the routing/health layers.
type ErrKind string

const (
	KindRateLimited   ErrKind = "rate_limited"
	KindAuth          ErrKind = "auth"
	KindQuota         ErrKind = "quota"
	KindServer        ErrKind = "server"
	KindNetwork       ErrKind = "network"
	KindTimeout       ErrKind = "timeout"
	KindBadRequest    ErrKind = "bad_request"
	KindUnprocessable ErrKind = "unprocessable" // 422: payload ditolak provider ini, provider lain mungkin menerimanya
	KindNotFound      ErrKind = "not_found"
	KindCancelled     ErrKind = "cancelled"
	KindUnknown       ErrKind = "unknown"
)

// Fallbackable reports whether a request that failed with this kind should be
// retried on the next candidate. Per spec 3.4 that is 429 / 5xx / timeout /
// quota — plus auth / not-found / network errors, which are specific to one
// connection and cured by moving to another account or provider. Bad requests
// (the client's own malformed payload) and cancellations are not retried.
func (k ErrKind) Fallbackable() bool {
	switch k {
	case KindBadRequest, KindCancelled:
		return false
	case KindUnprocessable:
		return true
	}
	return true
}

// ProviderError is a normalized upstream failure.
type ProviderError struct {
	Kind       ErrKind
	Status     int    // HTTP status; 0 for network/timeout errors
	Code       string // provider-specific error code, if any
	Message    string
	RetryAfter *time.Duration // set for 429 responses carrying Retry-After
}

func (e *ProviderError) Error() string {
	switch {
	case e.Status > 0 && e.Code != "":
		return fmt.Sprintf("provider error (status %d, code %q): %s", e.Status, e.Code, e.Message)
	case e.Status > 0:
		return fmt.Sprintf("provider error (status %d): %s", e.Status, e.Message)
	default:
		return fmt.Sprintf("provider error (%s): %s", e.Kind, e.Message)
	}
}

// Adapter translates OpenAI-compatible requests for one provider family and
// returns OpenAI-format responses.
type Adapter interface {
	Name() string
	// Chat performs one chat completion (streaming or not). Upstream failures
	// return a *ProviderError; success returns an open ChatResponse.
	Chat(ctx context.Context, conn *db.Connection, req *ChatRequest) (*ChatResponse, error)
	// ListModels lists model ids visible to this connection.
	ListModels(ctx context.Context, conn *db.Connection) ([]string, error)
}

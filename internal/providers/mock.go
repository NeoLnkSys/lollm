package providers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"time"
)

// MockBehavior scripts one response from the mock provider. Behaviors are
// consumed in order, one per request; the last behavior repeats forever.
type MockBehavior struct {
	Status      int               // HTTP status (0 → 200)
	ContentType string            // response content type
	Body        string            // raw body for non-streaming replies
	SSEFrames   []string          // raw SSE frames (written verbatim + blank line)
	Headers     map[string]string // extra response headers
	HangSeconds float64           // sleep before answering (timeout testing)
}

// MockOK is a normal non-streaming completion response.
func MockOK() MockBehavior { return MockOKBody(mockCompletionJSON) }

// MockOKBody returns a custom non-streaming body with status 200.
func MockOKBody(body string) MockBehavior {
	return MockBehavior{Status: 200, ContentType: "application/json", Body: body}
}

// MockSSE streams the given raw SSE frames (default frames when empty).
func MockSSE(frames ...string) MockBehavior {
	if len(frames) == 0 {
		frames = mockDefaultFrames
	}
	return MockBehavior{Status: 200, ContentType: "text/event-stream", SSEFrames: frames}
}

// MockStatus replies with a bare status code and a small JSON error body.
func MockStatus(status int) MockBehavior {
	return MockBehavior{
		Status:      status,
		ContentType: "application/json",
		Body:        `{"error":{"message":"mock status ` + strconv.Itoa(status) + `","code":"mock_error"}}`,
	}
}

// MockRateLimit replies 429 with a Retry-After header.
func MockRateLimit(retryAfterSecs int) MockBehavior {
	b := MockStatus(429)
	b.Headers = map[string]string{"Retry-After": strconv.Itoa(retryAfterSecs)}
	return b
}

// MockHang sleeps before answering 200 (for client timeout tests).
func MockHang(seconds float64) MockBehavior {
	return MockBehavior{Status: 200, ContentType: "application/json",
		Body: mockCompletionJSON, HangSeconds: seconds}
}

// MockRecord captures one request the mock server received.
type MockRecord struct {
	Path       string
	AuthHeader string
	Model      string
	Stream     bool
	Body       map[string]any
	Header     http.Header
}

// Mock is the stateful core of the mock provider: a scripted behavior queue
// plus a request recorder. Use Handler() to serve it anywhere.
type Mock struct {
	mu        sync.Mutex
	behaviors []MockBehavior
	records   []MockRecord
}

// NewMock builds a mock provider core.
func NewMock(behaviors ...MockBehavior) *Mock {
	return &Mock{behaviors: behaviors}
}

// Handler returns the OpenAI-dialect HTTP handler (paths /v1/models and
// /v1/chat/completions, also served without the /v1 prefix).
func (m *Mock) Handler() http.Handler { return http.HandlerFunc(m.handle) }

// Records returns a copy of the received-request log.
func (m *Mock) Records() []MockRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]MockRecord, len(m.records))
	copy(out, m.records)
	return out
}

// Reset clears the request log.
func (m *Mock) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records = nil
}

// MockServer is a Mock running on an httptest listener (test convenience).
type MockServer struct {
	*httptest.Server
	*Mock
}

// NewMockServer starts a mock provider speaking the OpenAI dialect at /v1.
func NewMockServer(behaviors ...MockBehavior) *MockServer {
	m := NewMock(behaviors...)
	return &MockServer{Server: httptest.NewServer(m.Handler()), Mock: m}
}

// BaseURL returns the OpenAI API base for the adapter (ends with /v1).
func (m *MockServer) BaseURL() string { return m.URL + "/v1" }

func (m *Mock) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && (r.URL.Path == "/v1/models" || r.URL.Path == "/models"):
		m.mu.Lock()
		behavior := m.nextBehaviorLocked()
		m.mu.Unlock()
		if behavior.Status >= 400 {
			// Scripted failure applies to /models too (health-probe testing).
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(behavior.Status)
			_, _ = w.Write([]byte(behavior.Body))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"id": "mock-model", "object": "model"},
				{"id": "mock-model-vision", "object": "model"},
				{"id": "mock-model-tools", "object": "model"},
			},
		})
		return
	case r.Method == http.MethodPost && (r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/chat/completions"):
		// fall through to the scripted behavior
	default:
		http.NotFound(w, r)
		return
	}

	// Record the request.
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	m.mu.Lock()
	m.records = append(m.records, MockRecord{
		Path:       r.URL.Path,
		AuthHeader: r.Header.Get("Authorization"),
		Model:      strAt(body, "model"),
		Stream:     boolAt(body, "stream"),
		Body:       body,
		Header:     r.Header.Clone(),
	})
	behavior := m.nextBehaviorLocked()
	m.mu.Unlock()

	if behavior.HangSeconds > 0 {
		time.Sleep(time.Duration(behavior.HangSeconds * float64(time.Second)))
	}

	status := behavior.Status
	if status == 0 {
		status = 200
	}
	for k, v := range behavior.Headers {
		w.Header().Set(k, v)
	}
	if behavior.ContentType != "" {
		w.Header().Set("Content-Type", behavior.ContentType)
	}
	w.WriteHeader(status)

	if len(behavior.SSEFrames) > 0 {
		flusher := w.(http.Flusher)
		for _, frame := range behavior.SSEFrames {
			_, _ = w.Write([]byte(frame + "\n\n"))
			flusher.Flush()
		}
		return
	}
	_, _ = w.Write([]byte(behavior.Body))
}

// nextBehaviorLocked consumes the scripted queue; the last entry repeats.
func (m *Mock) nextBehaviorLocked() MockBehavior {
	if len(m.behaviors) == 0 {
		return MockOK()
	}
	b := m.behaviors[0]
	if len(m.behaviors) > 1 {
		m.behaviors = m.behaviors[1:]
	}
	return b
}

func strAt(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func boolAt(m map[string]any, k string) bool {
	b, _ := m[k].(bool)
	return b
}

const mockCompletionJSON = `{"id":"chatcmpl-mock1","object":"chat.completion","created":1730000000,` +
	`"model":"mock-model","choices":[{"index":0,"message":{"role":"assistant","content":"Hello from mock"},` +
	`"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":6,"total_tokens":18}}`

var mockDefaultFrames = []string{
	`data: {"id":"chatcmpl-mock1","object":"chat.completion.chunk","created":1730000000,"model":"mock-model","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
	`data: {"id":"chatcmpl-mock1","object":"chat.completion.chunk","created":1730000000,"model":"mock-model","choices":[{"index":0,"delta":{"content":"Hello "},"finish_reason":null}]}`,
	`data: {"id":"chatcmpl-mock1","object":"chat.completion.chunk","created":1730000000,"model":"mock-model","choices":[{"index":0,"delta":{"content":"from mock"},"finish_reason":null}]}`,
	`data: {"id":"chatcmpl-mock1","object":"chat.completion.chunk","created":1730000000,"model":"mock-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	`data: {"id":"chatcmpl-mock1","object":"chat.completion.chunk","created":1730000000,"model":"mock-model","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":6,"total_tokens":18}}`,
	`data: [DONE]`,
}

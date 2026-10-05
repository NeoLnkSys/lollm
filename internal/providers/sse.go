package providers

import (
	"bufio"
	"io"
	"strings"
)

// SSEEvent is one parsed server-sent event.
type SSEEvent struct {
	Name string // "message" when no event: line was present
	Data string // data lines joined with "\n"
}

// SSEScanner parses an SSE stream (text/event-stream) event by event.
// It is synchronous on purpose: the caller drives the loop, which keeps
// cancellation and cleanup in one place (no leaked goroutines).
type SSEScanner struct {
	s    *bufio.Scanner
	cur  SSEEvent
	name string
	data []string
	err  error
}

// NewSSE wraps an SSE body stream. Lines longer than 4 MiB are treated as an
// error (upstream frames are far smaller; this is a runaway guard).
func NewSSE(r io.Reader) *SSEScanner {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64*1024), 4*1024*1024)
	return &SSEScanner{s: s}
}

// Next advances to the next complete event, returning false at EOF or error.
func (sc *SSEScanner) Next() bool {
	sc.cur = SSEEvent{}
	dispatched := false
	for sc.s.Scan() {
		line := sc.s.Text()
		switch {
		case line == "":
			// Blank line = event boundary. Only dispatch when data exists;
			// stray blank lines are ignored.
			if len(sc.data) > 0 {
				sc.dispatch()
				dispatched = true
			}
			sc.name = ""
			sc.data = nil
			if dispatched {
				return true
			}
		case strings.HasPrefix(line, ":"):
			// comment / keep-alive — ignore
		case strings.HasPrefix(line, "event:"):
			sc.name = trimField(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			sc.data = append(sc.data, trimField(line[len("data:"):]))
		default:
			// id:, retry:, and unknown fields are ignored
		}
	}
	sc.err = sc.s.Err()
	// Stream ended without a trailing blank line: flush any pending event —
	// exactly once (clear the buffer so subsequent Next() calls stay false).
	if sc.err == nil && len(sc.data) > 0 {
		sc.dispatch()
		sc.data = nil
		return true
	}
	return false
}

func (sc *SSEScanner) dispatch() {
	sc.cur = SSEEvent{Name: sc.name, Data: strings.Join(sc.data, "\n")}
	if sc.cur.Name == "" {
		sc.cur.Name = "message"
	}
}

// Event returns the event most recently returned by Next.
func (sc *SSEScanner) Event() SSEEvent { return sc.cur }

// Err returns the first non-EOF error encountered.
func (sc *SSEScanner) Err() error { return sc.err }

// trimField strips a single optional leading space after the field colon,
// per the SSE spec.
func trimField(s string) string {
	if strings.HasPrefix(s, " ") {
		return s[1:]
	}
	return s
}

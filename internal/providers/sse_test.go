package providers

import (
	"strings"
	"testing"
)

func TestSSEBasicParsing(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"a":1}`,
		``,
		`data: {"b":2}`,
		``,
		`data: [DONE]`,
		``,
		"",
	}, "\n")

	sc := NewSSE(strings.NewReader(stream))
	var events []SSEEvent
	for sc.Next() {
		events = append(events, sc.Event())
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d: %+v", len(events), events)
	}
	if events[0].Data != `{"a":1}` || events[1].Data != `{"b":2}` || events[2].Data != "[DONE]" {
		t.Fatalf("data mismatch: %+v", events)
	}
	for _, e := range events {
		if e.Name != "message" {
			t.Fatalf("default event name must be message: %+v", e)
		}
	}
}

func TestSSEEventNamesAndMultiLineData(t *testing.T) {
	stream := strings.Join([]string{
		`event: lollm-agent-step`,
		`data: line one`,
		`data: line two`,
		``,
		"",
	}, "\n")

	sc := NewSSE(strings.NewReader(stream))
	if !sc.Next() {
		t.Fatalf("expected one event, err=%v", sc.Err())
	}
	e := sc.Event()
	if e.Name != "lollm-agent-step" {
		t.Fatalf("event name: %q", e.Name)
	}
	if e.Data != "line one\nline two" {
		t.Fatalf("multi-line data must join with newline: %q", e.Data)
	}
}

func TestSSECommentsAndKeepAlive(t *testing.T) {
	stream := strings.Join([]string{
		`: keep-alive comment`,
		``,
		`data: {"ok":true}`,
		``,
		"",
	}, "\n")
	sc := NewSSE(strings.NewReader(stream))
	var n int
	for sc.Next() {
		n++
		if sc.Event().Data != `{"ok":true}` {
			t.Fatalf("unexpected data: %q", sc.Event().Data)
		}
	}
	if n != 1 {
		t.Fatalf("comments must not produce events, got %d", n)
	}
}

func TestSSENoTrailingBlankLine(t *testing.T) {
	// Some providers end the stream without a final blank line.
	sc := NewSSE(strings.NewReader("data: [DONE]"))
	if !sc.Next() {
		t.Fatalf("expected the pending event to flush, err=%v", sc.Err())
	}
	if sc.Event().Data != "[DONE]" {
		t.Fatalf("data: %q", sc.Event().Data)
	}
	if sc.Next() {
		t.Fatal("stream must be exhausted")
	}
}

func TestSSECRLF(t *testing.T) {
	sc := NewSSE(strings.NewReader("data: {\"x\":1}\r\n\r\ndata: [DONE]\r\n\r\n"))
	var n int
	for sc.Next() {
		n++
	}
	if n != 2 {
		t.Fatalf("CRLF stream must parse, got %d events (err=%v)", n, sc.Err())
	}
}

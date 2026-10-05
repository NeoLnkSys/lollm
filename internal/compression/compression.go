// Package compression implements token compression for tool-heavy chat
// requests (spec section 3.5): tool_result payloads are trimmed or summarized
// before the request is forwarded upstream, cutting input tokens on coding
// agent workloads by 20–40%+.
//
//	Modes:
//	  off     — pass through untouched
//	  partial — trim long tool_results to head+tail with a truncation marker,
//	            always keeping the newest tool_result intact
//	  full    — partial, plus older tool_results (beyond the last KeepLast)
//	            collapsed to one-line summaries, plus system-message dedupe
//
// Compression only touches what LoLLM sends upstream; the client's request is
// never modified in place before authentication-sensitive handling, and the
// estimated saving is returned so it can be recorded in the usage log.
package compression

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Mode selects the compression level.
type Mode string

const (
	Off     Mode = "off"
	Partial Mode = "partial"
	Full    Mode = "full"
)

// ParseMode parses a mode string (case-insensitive). ok=false for unknown
// values, so callers can ignore typos in headers instead of failing.
func ParseMode(s string) (Mode, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "off":
		return Off, true
	case "partial":
		return Partial, true
	case "full":
		return Full, true
	}
	return Off, false
}

// Options tune compression.
type Options struct {
	Mode      Mode
	Threshold int // tool_result content longer than this gets trimmed
	Head      int // characters kept from the start
	Tail      int // characters kept from the end
	KeepLast  int // full mode: how many newest tool_results survive (trimmed, not summarized)
}

// DefaultOptions returns the standard tuning.
func DefaultOptions() Options {
	return Options{Threshold: 2000, Head: 500, Tail: 500, KeepLast: 3}
}

// Apply compresses the messages of an OpenAI chat request body in place and
// returns the estimated number of input tokens saved (chars/4 heuristic).
func Apply(body map[string]any, opts Options) int {
	// Only explicit modes compress; the zero value ("") behaves like off.
	switch opts.Mode {
	case Partial, Full:
	default:
		return 0
	}
	if body == nil {
		return 0
	}
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return 0
	}
	before := estimateTokens(msgs)

	if opts.Threshold <= 0 {
		opts.Threshold = 2000
	}
	if opts.Head <= 0 {
		opts.Head = 500
	}
	if opts.Tail <= 0 {
		opts.Tail = 500
	}
	if opts.KeepLast <= 0 {
		opts.KeepLast = 3
	}

	toolIdx := collectToolMessages(msgs)
	n := len(toolIdx)
	if opts.Mode == Partial {
		for i, idx := range toolIdx {
			if i == n-1 {
				continue // the newest tool_result stays fully intact
			}
			compressToolContent(msgs[idx], opts, false)
		}
	} else { // Full
		older := n - opts.KeepLast
		for i, idx := range toolIdx {
			if i < older {
				compressToolContent(msgs[idx], opts, true) // one-line summary
			} else {
				compressToolContent(msgs[idx], opts, false) // head+tail trim
			}
		}
		body["messages"] = dedupeSystemMessages(msgs)
	}

	after := estimateTokens(body["messages"].([]any))
	if after >= before {
		return 0
	}
	return before - after
}

// collectToolMessages returns the indices of messages that carry tool
// results: role=="tool" messages, or any message whose content array holds a
// tool_result block (Claude-dialect clients).
func collectToolMessages(msgs []any) []int {
	var out []int
	for i, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role == "tool" {
			out = append(out, i)
			continue
		}
		if parts, ok := msg["content"].([]any); ok {
			for _, p := range parts {
				if part, ok := p.(map[string]any); ok {
					if t, _ := part["type"].(string); t == "tool_result" {
						out = append(out, i)
						break
					}
				}
			}
		}
	}
	return out
}

// compressToolContent trims or summarizes one tool message's content.
// summaryOnly replaces the whole content with a one-line placeholder (full
// mode, old results); otherwise long content is trimmed head+tail (partial).
func compressToolContent(msg any, opts Options, summaryOnly bool) {
	m, ok := msg.(map[string]any)
	if !ok {
		return
	}
	switch content := m["content"].(type) {
	case string:
		m["content"] = compressString(content, opts, summaryOnly)
	case []any:
		for _, p := range content {
			part, ok := p.(map[string]any)
			if !ok {
				continue
			}
			switch t, _ := part["type"].(string); t {
			case "text":
				if s, ok := part["text"].(string); ok {
					part["text"] = compressString(s, opts, summaryOnly)
				}
			case "tool_result":
				if s, ok := part["content"].(string); ok {
					part["content"] = compressString(s, opts, summaryOnly)
				}
			}
		}
	}
}

func compressString(s string, opts Options, summaryOnly bool) string {
	if s == "" {
		return s
	}
	if summaryOnly {
		return summarize(s)
	}
	if len(s) <= opts.Threshold {
		return s
	}
	trimmed := collapseBlankLines(s[:opts.Head]) +
		"\n[… truncated " + strconv.Itoa(len(s)-opts.Head-opts.Tail) + " chars …]\n" +
		collapseBlankLines(s[len(s)-opts.Tail:])
	return trimmed
}

// summarize reduces an old tool_result to a single recognizable line.
func summarize(s string) string {
	first := strings.TrimSpace(s)
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	if len(first) > 120 {
		first = first[:120]
	}
	return "[tool_result omitted by LoLLM: " + first + "]"
}

// collapseBlankLines squashes 3+ consecutive newlines down to one blank line.
func collapseBlankLines(s string) string {
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return s
}

// dedupeSystemMessages removes exact-duplicate system messages, keeping the
// first occurrence (full mode only).
func dedupeSystemMessages(msgs []any) []any {
	seen := map[string]bool{}
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			out = append(out, m)
			continue
		}
		if role, _ := msg["role"].(string); role == "system" {
			key := fmt.Sprint(msg["content"])
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		out = append(out, m)
	}
	return out
}

// estimateTokens estimates tokens for a JSON-serializable value using the
// standard ~4-chars-per-token heuristic.
func estimateTokens(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return (len(b) + 3) / 4
}

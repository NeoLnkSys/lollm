package compression

import (
	"fmt"
	"strings"
	"testing"
)

// buildBody creates a Claude-Code-style payload: system prompt, alternating
// assistant(tool_calls)/user(tool) pairs, and a final user question.
func buildBody(nTools, toolChars int) map[string]any {
	msgs := []any{
		map[string]any{"role": "system", "content": "You are a coding agent."},
	}
	for i := 0; i < nTools; i++ {
		msgs = append(msgs, map[string]any{
			"role": "assistant",
			"tool_calls": []any{map[string]any{
				"id": fmt.Sprintf("call_%d", i), "type": "function",
				"function": map[string]any{"name": "read_file", "arguments": `{\"path\":\"src/file.go\"}`},
			}},
		})
		content := strings.Repeat(fmt.Sprintf("line %04d of tool output %d\n", i, i), 0)
		_ = content
		tool := strings.Repeat(fmt.Sprintf("package main\nfunc f%d() {}\n", i), 1)
		for len(tool) < toolChars {
			tool += fmt.Sprintf("// padding line %d of tool result %d\n", i, i)
		}
		msgs = append(msgs, map[string]any{
			"role": "tool", "tool_call_id": fmt.Sprintf("call_%d", i), "content": tool,
		})
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": "Now summarize the code."})
	return map[string]any{"model": "Auto", "messages": msgs}
}

func toolContents(body map[string]any) []string {
	msgs := body["messages"].([]any)
	var out []string
	for _, m := range msgs {
		msg := m.(map[string]any)
		if role, _ := msg["role"].(string); role == "tool" {
			s, _ := msg["content"].(string)
			out = append(out, s)
		}
	}
	return out
}

func TestPartialTrimsOldKeepsLast(t *testing.T) {
	body := buildBody(3, 5000)
	opts := DefaultOptions()
	opts.Mode = Partial

	saved := Apply(body, opts)
	if saved <= 0 {
		t.Fatal("partial mode must save tokens on long tool results")
	}
	tools := toolContents(body)
	if len(tools) != 3 {
		t.Fatalf("tool messages must not be dropped: %d", len(tools))
	}
	last := tools[len(tools)-1]
	if len(last) < 5000 {
		t.Fatalf("the newest tool_result must stay intact, got %d chars", len(last))
	}
	for i, s := range tools[:len(tools)-1] {
		if len(s) > 1400 { // head 500 + tail 500 + marker + slack
			t.Fatalf("old tool_result %d not trimmed: %d chars", i, len(s))
		}
		if !strings.Contains(s, "[… truncated ") {
			t.Fatalf("trimmed result %d must carry the truncation marker", i)
		}
	}
}

func TestFullSummarizesOlderToolResults(t *testing.T) {
	body := buildBody(6, 4000)
	opts := DefaultOptions()
	opts.Mode = Full

	saved := Apply(body, opts)
	tools := toolContents(body)
	if len(tools) != 6 {
		t.Fatalf("tool messages must not be dropped: %d", len(tools))
	}
	// KeepLast=3: the 3 oldest become one-line summaries…
	for i := 0; i < 3; i++ {
		if len(tools[i]) > 200 {
			t.Fatalf("old tool_result %d should be a short summary, got %d chars", i, len(tools[i]))
		}
		if !strings.Contains(tools[i], "[tool_result omitted by LoLLM:") {
			t.Fatalf("old tool_result %d must be a summary placeholder: %.80s", i, tools[i])
		}
	}
	// …the 3 newest are trimmed but still substantial.
	for i := 3; i < 6; i++ {
		if len(tools[i]) < 1000 || len(tools[i]) > 1400 {
			t.Fatalf("recent tool_result %d should be head+tail trimmed: %d chars", i, len(tools[i]))
		}
	}

	// Realistic savings target (spec: 20–40% on tool-heavy workloads).
	msgs := body["messages"].([]any)
	total := 0
	for _, s := range toolContents(body) {
		total += len(s)
	}
	_ = msgs
	original := 6 * 4000
	savedPct := 100 * saved / (saved + estimateTokens(body["messages"].([]any)))
	if savedPct < 20 {
		t.Fatalf("full mode should save >=20%% on tool-heavy payloads, got %d%% (saved=%d)", savedPct, saved)
	}
	t.Logf("full mode: estimated saving %d tokens (~%d%% of %d original tool chars)", saved, savedPct, original)
}

func TestShortToolResultsUntouched(t *testing.T) {
	body := buildBody(3, 800) // below the 2000 threshold
	for _, mode := range []Mode{Partial, Full} {
		clone := buildBody(3, 800)
		opts := DefaultOptions()
		opts.Mode = mode
		if saved := Apply(clone, opts); saved != 0 {
			t.Fatalf("%s must not touch short tool results (saved=%d)", mode, saved)
		}
	}
	_ = body
}

func TestOffModeNoChange(t *testing.T) {
	body := buildBody(5, 5000)
	before := toolContents(body)[0]
	if saved := Apply(body, DefaultOptions()); saved != 0 {
		t.Fatalf("off mode must not save anything: %d", saved)
	}
	if toolContents(body)[0] != before {
		t.Fatal("off mode must not modify the payload")
	}
}

func TestFullDedupesSystemMessages(t *testing.T) {
	body := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "You are a coding agent."},
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "system", "content": "You are a coding agent."},
		map[string]any{"role": "user", "content": "again"},
	}}
	opts := DefaultOptions()
	opts.Mode = Full
	if saved := Apply(body, opts); saved <= 0 {
		t.Fatal("duplicate system messages must be deduped in full mode")
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("duplicate system message must be removed, got %d messages", len(msgs))
	}
}

func TestContentArrayForm(t *testing.T) {
	long := strings.Repeat("x", 5000)
	body := map[string]any{"messages": []any{
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": []any{
			map[string]any{"type": "text", "text": long},
		}},
		map[string]any{"role": "tool", "tool_call_id": "c2", "content": []any{
			map[string]any{"type": "text", "text": long},
		}},
		map[string]any{"role": "user", "content": "q"},
	}}
	opts := DefaultOptions()
	opts.Mode = Partial
	saved := Apply(body, opts)
	if saved <= 0 {
		t.Fatal("array-form tool content must be compressed")
	}
	first := body["messages"].([]any)[0].(map[string]any)
	text := first["content"].([]any)[0].(map[string]any)["text"].(string)
	if len(text) > 1400 {
		t.Fatalf("array-form text not trimmed: %d", len(text))
	}

	// Claude-dialect tool_result blocks are also recognized.
	body2 := map[string]any{"messages": []any{
		map[string]any{"role": "tool", "content": []any{
			map[string]any{"type": "tool_result", "content": long},
			map[string]any{"type": "tool_result", "content": long},
		}},
		map[string]any{"role": "tool", "content": []any{
			map[string]any{"type": "tool_result", "content": long},
		}},
		map[string]any{"role": "user", "content": "q"},
	}}
	if saved := Apply(body2, opts); saved <= 0 {
		t.Fatal("tool_result blocks must be compressed")
	}
}

func TestNoMessagesNoop(t *testing.T) {
	if saved := Apply(map[string]any{"model": "x"}, DefaultOptions()); saved != 0 {
		t.Fatal("body without messages must be a no-op")
	}
	if saved := Apply(nil, DefaultOptions()); saved != 0 {
		t.Fatal("nil body must be a no-op")
	}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{
		"off": Off, "OFF": Off, " partial ": Partial, "Full": Full,
	} {
		if m, ok := ParseMode(in); !ok || m != want {
			t.Fatalf("ParseMode(%q) = %v,%v want %v", in, m, ok, want)
		}
	}
	if _, ok := ParseMode("banana"); ok {
		t.Fatal("unknown mode must not parse")
	}
	if _, ok := ParseMode(""); ok {
		t.Fatal("empty string must not parse (used for combo inheritance)")
	}
}

func TestRealisticSavingsBothModes(t *testing.T) {
	// The spec's 20–40% target applies to tool-heavy coding payloads.
	for _, mode := range []Mode{Partial, Full} {
		body := buildBody(8, 6000)
		opts := DefaultOptions()
		opts.Mode = mode
		saved := Apply(body, opts)
		msgs := body["messages"].([]any)
		after := estimateTokens(msgs)
		pct := 100 * saved / (saved + after)
		if pct < 20 {
			t.Fatalf("%s mode saved only %d%% (<20) on 8×6000-char tool results", mode, pct)
		}
		t.Logf("%s: payload now ~%d tokens, saved ~%d (%d%%)", mode, after, saved, pct)
	}
}

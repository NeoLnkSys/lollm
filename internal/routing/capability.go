package routing

import "strings"

// Capacity adapter (spec section 3.4): detect what a request needs and what a
// model can do, so routing can skip models that would reject the request.

// ModelCaps describes a model's known capabilities.
type ModelCaps struct {
	Vision           bool
	Audio            bool
	ToolCalling      bool
	StructuredOutput bool
}

// allCaps is the permissive default for unknown models: false-allow (a request
// that slips through fails at the provider with a visible error) instead of
// false-deny (a perfectly good model never gets used).
var allCaps = ModelCaps{Vision: true, Audio: true, ToolCalling: true, StructuredOutput: true}

// modelCatalog lists models with deliberately constrained capabilities.
// Names use the exact strings combos reference (OpenRouter- and native-style).
var modelCatalog = map[string]ModelCaps{
	// Anthropic (Claude 3+ = vision + tools)
	"anthropic/claude-sonnet-4":   {Vision: true, ToolCalling: true, StructuredOutput: true},
	"anthropic/claude-opus-4":     {Vision: true, ToolCalling: true, StructuredOutput: true},
	"anthropic/claude-3.7-sonnet": {Vision: true, ToolCalling: true, StructuredOutput: true},
	"anthropic/claude-3.5-sonnet": {Vision: true, ToolCalling: true, StructuredOutput: true},
	"anthropic/claude-3.5-haiku":  {Vision: true, ToolCalling: true, StructuredOutput: true},
	"claude-sonnet-4":             {Vision: true, ToolCalling: true, StructuredOutput: true},
	"claude-opus-4":               {Vision: true, ToolCalling: true, StructuredOutput: true},
	"claude-3-5-sonnet":           {Vision: true, ToolCalling: true, StructuredOutput: true},

	// OpenAI
	"openai/gpt-4o":      allCaps,
	"openai/gpt-4o-mini": allCaps,
	"gpt-4o":             allCaps,
	"gpt-4o-mini":        allCaps,

	// Google
	"google/gemini-2.5-flash": allCaps,
	"google/gemini-2.5-pro":   allCaps,
	"google/gemini-2.0-flash": allCaps,
	"gemini-2.5-flash":        allCaps,
	"gemini-2.5-pro":          allCaps,
	"gemini-2.0-flash":        allCaps,

	// Meta via Groq — text-only
	"llama-3.3-70b-versatile":           {ToolCalling: true, StructuredOutput: true},
	"llama-3.1-8b-instant":              {ToolCalling: true},
	"meta-llama/llama-3.3-70b-instruct": {ToolCalling: true, StructuredOutput: true},

	// Others
	"mistralai/mistral-small": {ToolCalling: true, StructuredOutput: true},
	"deepseek/deepseek-chat":  {ToolCalling: true, StructuredOutput: true},
}

// ModelCapabilities returns the capabilities of a model. Lookup order:
// exact name, last path segment (drops vendor prefixes like "anthropic/"),
// then keyword heuristics, then the permissive default.
func ModelCapabilities(model string) ModelCaps {
	if caps, ok := modelCatalog[model]; ok {
		return caps
	}
	if base := model[strings.LastIndexByte(model, '/')+1:]; base != model {
		if caps, ok := modelCatalog[base]; ok {
			return caps
		}
	}
	return heuristicCaps(model)
}

func heuristicCaps(model string) ModelCaps {
	m := strings.ToLower(model)
	// Permissive default for unknown models — false-allow beats false-deny:
	// a wrongly-allowed request fails at the provider with a visible error,
	// while a wrongly-denied model would silently never be used.
	caps := ModelCaps{Vision: true, ToolCalling: true, StructuredOutput: true}

	// Known text-only families lose vision even when not in the catalog.
	for _, kw := range []string{
		"llama-3.3", "llama-3.1", "llama-3.2-1b", "llama-3.2-3b",
		"deepseek", "mistral", "codestral", "qwen2.5-coder", "gpt-3.5",
	} {
		if strings.Contains(m, kw) {
			caps.Vision = false
			break
		}
	}

	// Audio-in-chat is rare and explicit: assume absent unless named.
	if strings.Contains(m, "audio") || strings.Contains(m, "whisper") {
		caps.Audio = true
	}
	return caps
}

// supports reports whether the model's capabilities satisfy the request's.
func supports(caps ModelCaps, need Capabilities) bool {
	if need.Vision && !caps.Vision {
		return false
	}
	if need.Audio && !caps.Audio {
		return false
	}
	if need.ToolCalling && !caps.ToolCalling {
		return false
	}
	if need.StructuredOutput && !caps.StructuredOutput {
		return false
	}
	return true
}

// DetectCapabilities inspects a decoded OpenAI-compatible chat request body
// and reports what the request needs. Content may be a plain string or an
// array of typed parts; tools/response_format are top-level fields.
func DetectCapabilities(body map[string]any) Capabilities {
	var caps Capabilities

	if tools, ok := body["tools"].([]any); ok && len(tools) > 0 {
		caps.ToolCalling = true
	}
	if rf, ok := body["response_format"].(map[string]any); ok {
		if t, _ := rf["type"].(string); t == "json_schema" {
			caps.StructuredOutput = true
		}
	}
	if messages, ok := body["messages"].([]any); ok {
		for _, m := range messages {
			msg, ok := m.(map[string]any)
			if !ok {
				continue
			}
			parts, ok := msg["content"].([]any)
			if !ok {
				continue
			}
			for _, p := range parts {
				part, ok := p.(map[string]any)
				if !ok {
					continue
				}
				switch t, _ := part["type"].(string); t {
				case "image_url":
					caps.Vision = true
				case "input_audio":
					caps.Audio = true
				}
			}
		}
	}
	return caps
}

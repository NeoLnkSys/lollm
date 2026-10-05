package api

// Kompatibilitas klien → provider. Beberapa klien (mis. Grok CLI) menyisipkan
// field non-standar ke payload: "model_id" pada pesan assistant, atau field
// spesifik vendor seperti "search_parameters" (xAI) di level atas request.
// Upstream yang ketat (FastAPI/pydantic, Google) menolak field ekstra dengan
// 422 "extra_forbidden" / 400 "Cannot find field". Sebelum request
// diteruskan, payload dibersihkan lewat allowlist field standar OpenAI.

import "slices"

// msgFieldOK adalah allowlist field pesan yang diizinkan diteruskan.
var msgFieldOK = map[string]bool{
	"role":          true,
	"content":       true,
	"name":          true,
	"tool_calls":    true,
	"tool_call_id":  true,
	"function_call": true,
}

// bodyFieldOK adalah allowlist field level-atas request chat completions
// (superset field standar OpenAI + variasi umum seperti max_completion_tokens
// dan reasoning). Field vendor spesifik dibuang sebelum kirim ke upstream.
var bodyFieldOK = []string{
	// inti (dipakai juga internal gateway)
	"messages", "model", "stream",
	// sampling & opsional standar
	"temperature", "top_p", "top_k", "stop", "max_tokens", "max_completion_tokens",
	"n", "frequency_penalty", "presence_penalty", "seed", "logit_bias",
	"logprobs", "top_logprobs", "response_format", "user", "metadata",
	"service_tier", "store", "stream_options", "parallel_tool_calls",
	// tools / function calling
	"tools", "tool_choice", "functions", "function_call",
	// reasoning (OpenAI/groq/openrouter style)
	"reasoning_effort", "reasoning",
}

// sanitizeMessages menghapus field non-standar dari setiap pesan di body.
func sanitizeMessages(body map[string]any) {
	msgs, ok := body["messages"].([]any)
	if !ok {
		return
	}
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		for k := range mm {
			if !msgFieldOK[k] {
				delete(mm, k)
			}
		}
	}
}

// sanitizeBody menghapus field non-standar di level atas request (mis.
// "search_parameters" dari Grok CLI) sebelum diteruskan ke upstream.
func sanitizeBody(body map[string]any) {
	for k := range body {
		if !slices.Contains(bodyFieldOK, k) {
			delete(body, k)
		}
	}
}

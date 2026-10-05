package api

// Kompatibilitas klien → provider. Beberapa klien (mis. Grok CLI) menyisipkan
// field non-standar ke pesan ("model_id" pada pesan assistant), dan beberapa
// upstream yang ketat (FastAPI/pydantic) menolak field ekstra dengan 422
// "extra_forbidden". Sebelum request diteruskan, pesan dibersihkan lewat
// allowlist field standar OpenAI.

// msgFieldOK adalah allowlist field pesan yang diizinkan diteruskan.
var msgFieldOK = map[string]bool{
	"role":          true,
	"content":       true,
	"name":          true,
	"tool_calls":    true,
	"tool_call_id":  true,
	"function_call": true,
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

# Changelog — LoLLM Synapse

## v0.1.0 "Synapse" (2026-10-05)

Rilis pertama. Gateway AI self-hosted dalam satu binary statis.

### Core
- **API OpenAI-compatible**: `POST /v1/chat/completions` (SSE streaming + non-stream), `GET /v1/models`, `/healthz` — 100% kompatibel dengan OpenAI SDK (tinggal ganti `base_url`).
- **Routing health-aware**: combo multi-model dengan fallback otomatis, circuit breaker, weighted/sticky/round-robin/sequential/fusion, skip otomatis koneksi rate-limited/unavailable/backoff, health watcher dengan auto-recovery.
- **8 provider**: OpenRouter, Gemini, Groq, Mistral, Cloudflare AI, Ollama, Poolside, + custom (base_url mana pun). Multi-key per provider.
- **Proxy pools** HTTP/SOCKS5 per koneksi.
- **Token compression** (header/combo/global): `partial` (ramping) & `full` (ringkas) untuk beban kerja coding tool-result — terbukti hemat −78,5% / −94,9% prompt token (tokenizer Gemini asli).
- **Agent Mode** (fitur unggulan): pipeline multi-agent — `collaborative` (planner → reviewer×N → finalizer), `debate` (generator paralel + judge), `parallel` (generator paralel + merger). Aktivasi via model `agent-auto|agent-debate|agent-parallel`, header `X-LoLLM-Agent-Mode`, atau flag combo. Hanya jawaban final yang di-stream; langkah internal opsional disembunyikan; `tool_calls` diteruskan verbatim; perlindungan rekursi.
- **Kunci & keamanan**: API key internal hanya disimpan sebagai hash SHA-256; kunci provider dienkripsi AES-256-GCM dengan master key; admin token opsional untuk dasbor; log tanpa rahasia; batas ukuran body permintaan.

### Dashboard (mobile-first)
- **Chat Playground** — uji coba model/pipeline apa pun langsung dari dasbor (streaming + progres agen secara langsian).
- Kelola Connections (dengan uji koneksi langsung), Combos, Agent Mode, API Keys, Proxy Pools, Usage (hemat token/latensi), Models, Settings.
- Ekspor/impor `backup.json` bawaan dasbor.
- Ikon Lucide SVG, navigasi bawah + bottom-sheet untuk ponsel, tata letak desktop tetap tersedia.

### CLI
- `lollm serve|setup|doctor|key|routes|export-config|import-config|usage|version`.
- Ekspor/impor dengan rahasia opsional (`--secrets`) dan penyegelan kata sandi PBKDF2-AES-256-GCM (`--password`), mode `--replace`, impor aman lintas master key.
- `lollm usage --csv` untuk analitik cepat.

### build
- Binary statis tunggal (Go + SQLite murni via modernc + web UI via `go:embed`), CGO_ENABLED=0.
- Dockerfile multi-stage; `make build|run|test|vet|docker|release`.

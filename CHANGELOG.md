# Changelog

## v0.1.0 "Synapse" — 2026-10-05

Rilis perdana.

- **API OpenAI-compatible** — `/v1/chat/completions` (SSE dan non-stream), `/v1/models`, `/healthz`. Klien cukup mengganti `base_url`.
- **Routing health-aware** — combo multi-provider dengan fallback otomatis, circuit breaker, strategi weighted/sticky/round-robin/sequential, backoff dengan pemulihan otomatis.
- **Provider** — OpenRouter, Gemini, Groq, Mistral, Cloudflare AI, Ollama, Poolside, plus endpoint custom apa pun; multi-key per provider; proxy pool HTTP/SOCKS5.
- **Agent Mode** — pipeline collaborative/debate/parallel; hanya jawaban final yang dikirim ke klien; `tool_calls` diteruskan apa adanya.
- **Token compression** — mode `partial` dan `full` untuk beban kerja coding; penghematan terlihat per-request di usage log.
- **Dashboard mobile-first** — kelola koneksi (dengan test provider), combo, agent config, API key, proxy pool, dan usage; plus chat playground.
- **Backup portabel** — `export-config`/`import-config` dengan opsi penyegelan PBKDF2-AES-256-GCM; aman pindah mesin dengan master key berbeda.
- **CLI** — `serve`, `setup`, `doctor`, `key`, `routes`, `export-config`, `import-config`, `usage`.
- Binary statis tunggal (Go, SQLite murni, UI ter-embed), Dockerfile multi-stage, installer satu baris untuk linux/darwin.

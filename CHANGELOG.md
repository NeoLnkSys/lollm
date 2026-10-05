# Changelog

## v0.1.2 "Synapse" — 2026-10-05

- **Dashboard Material You (MD3)** — sistem warna dinamis dari satu seed hue (pilih swatch atau geser slider di Settings → Tampilan; seluruh tema, termasuk status bar Android, ikut berubah), tombol pill, kartu tonal tanpa border, indikator pill di bottom navigation, bubble chat & bottom sheet membulat khas Google.

## v0.1.1 "Synapse" — 2026-10-05

Fokus: stabilitas combo routing.

- **Kompatibilitas klien** — field non-standar di pesan (mis. `model_id` dari Grok CLI) otomatis dibersihkan sebelum diteruskan ke provider, memperbaiki error 422 `extra_forbidden` dari upstream yang ketat.
- **422 kini fallback-able** — jika satu provider menolak payload, request otomatis dicoba ke provider berikutnya di combo.
- **Agent Mode dihapus** — fokus penuh pada routing dan fallback combo model. (Pengguna v0.1.0: import backup lama tetap aman; entri agent diabaikan.)

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

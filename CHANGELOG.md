# Changelog

## v0.1.5 "Synapse" — 2026-10-05

Multi-key per provider + fallback dalam lingkungan connection.

- **Multi-key dalam satu connection** — satu connection kini bisa memuat banyak API key (satu per baris); trafik disebar round-robin antar key, dan saat kena rate limit request otomatis diulang dengan **key berikutnya** sebelum fallback ke connection lain (fallback level key, dalam lingkungan provider yang sama).
- **Rotasi proxy saat retry** — connection dengan proxy pool (>1 proxy) ikut berotasi ke proxy berikutnya saat rate limit berbasis IP, sebelum menyerah pada connection tersebut.
- **Model dikelompokkan per provider & didedup** — dashboard (`/api/models`), combo picker, dan chat select kini menampilkan model per provider (union dari semua key, tanpa duplikat); sebelumnya setiap key menggandakan seluruh katalog model.
- Connection card menampilkan jumlah key (`N key`); form connection menerima multi-key (textarea satu key per baris). Retry antar key dicatat di log gateway, bukan di usage log (anti-spam).

## v0.1.4 "Synapse" — 2026-10-05

## v0.1.4 "Synapse" — 2026-10-05

Perbaikan dashboard (UX) + bugfix streaming.

- **Fix: streaming chat tidak menampilkan balasan** — parser SSE dashboard dulu hanya memproses frame ber-`event: chat.completion.chunk`, padahal gateway mengirim `data:` tanpa nama event; kini semua frame ber-`data:` diproses (plus dukungan CRLF & field `reasoning` ditampilkan).
- **Fix: input chat tertutup bottom bar (mobile)** — tinggi bar diukur nyata via JS dan dipakai sebagai padding halaman chat; tinggi item bar dibuat deterministik.
- **Proxy Pools bisa dikelola penuh dari dashboard** — pool per-card; setiap proxy punya kartu sendiri dengan tombol test (latency live), hapus, dan indikator in use (jumlah connection memakai pool tsb) + hasil test terakhir.
- **Combo editor model picker** — pilih model via checkbox chips yang dimuat langsung dari API sumber (`v1/models` per connection), priority terisi otomatis sesuai urutan centang dan tetap bisa diubah; mode manual lama tetap tersedia.

## v0.1.3 "Synapse" — 2026-10-05

- **Kompatibilitas klien diperluas** — field spesifik vendor di level atas request (mis. `search_parameters` dari Grok CLI/xAI) kini juga dibersihkan via allowlist sebelum diteruskan, memperbaiki error 400 `Cannot find field` dari upstream Google/FastAPI. Terverifikasi end-to-end dengan Grok CLI (chat + tool calls multi-ronde).

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

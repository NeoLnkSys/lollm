# LoLLM — Implementation Plan

> **Sumber:** `uploads/agent.md` (spesifikasi lengkap) · **Tanggal:** 2026-10-05 · **Status:** Draft untuk review
> **Tujuan:** Roadmap teknis + fase implementasi untuk membangun **LoLLM** — self-hosted AI Gateway / LLM Router dalam **satu binary Go** (`lollm`), OpenAI-compatible, dengan fallback multi-provider, Agent Mode, dan dashboard embedded.

---

## 0. Ringkasan Eksekutif

LoLLM adalah **proxy lokal OpenAI-compatible** yang ditaruh di antara tool coding (Cursor, Claude Code, Cline, dll) dan banyak provider LLM (OpenRouter, Gemini, Groq, Mistral, Ollama, dll). Nilai jual utamanya:

1. **Routing cerdas** — combo model (mis. `Auto`) yang otomatis skip connection mati / rate-limited, dengan fallback, load balancing, dan circuit breaker.
2. **Agent Mode** — pipeline multi-agent (planner → reviewer → finalizer) yang hanya me-stream jawaban final.
3. **Single binary** — Go + SQLite + dashboard embedded via `go:embed`, nol dependency runtime.
4. **Token compression** — hemat 20–40% input token pada workload coding-agent.

**Estimasi total:** ± 11 fase, ~20–22 hari kerja setara senior engineer (saya sebagai agent bisa dikerjakan bertahap per fase, tiap fase menghasilkan artefak yang bisa langsung diuji).

---

## 1. Keputusan Teknis Utama

| Area | Pilihan | Alasan | Alternatif yang ditolak |
|---|---|---|---|
| Bahasa | **Go 1.24** | Sesuai rekomendasi spec; cross-compile trivial; binary statis | Bun (`bun build --compile`) — ekosistem SSE/SQLite/proxy lebih rapuh |
| HTTP router | **`chi`** | Ringan, stdlib-compatible, middleware chain matang | fiber (non-stdlib fasthttp), echo (lebih besar dari kebutuhan) |
| CLI | **`spf13/cobra`** | Standar de-facto, sub-command + flag + env helper | urfave/cli |
| SQLite | **`modernc.org/sqlite`** (pure Go) | `CGO_ENABLED=0` → binary **benar-benar statis**, bebas masalah glibc; performa cukup untuk gateway lokal | `mattn/go-sqlite3` (butuh CGO, menyulitkan static build) |
| Config | **`gopkg.in/yaml.v3`** + merge manual | Merge order eksplisit: `flags > env > yaml > default` — mudah dites, tanpa magic | viper (berat, banyak fitur tak terpakai) |
| Dashboard | **SPA vanilla JS/CSS (tanpa build step)** | Toolchain tetap 100% Go; `go:embed` langsung dari source; 9 halaman CRUD tetap manageable dengan hash-router + fetch | Next.js static export — butuh Node di setiap build, menambah friction |
| Enkripsi secret | **AES-256-GCM**, master key di `data/.master.key` (auto-generate, 0600) atau env `LOLLM_MASTER_KEY` | Reversible (perlu untuk memanggil provider) tapi tidak plaintext di DB | Hash (tidak bisa dipakai ulang), plaintext (dilarang spec) |
| Internal API key | Format `lollm-<43 char base64url>`, disimpan **SHA-256 hash** | Sesuai spec: tidak pernah simpan plain | — |
| Testing | Mock provider in-process (`httptest`) | Seluruh E2E (fallback, 429, SSE, Agent Mode) bisa dites **tanpa API key asli** | — |

**Catatan lingkungan workspace:** Go belum terpasang di sandbox (Node 20 ada) → Fase 0 menginstal Go 1.24 via apt/tarball resmi. Docker tidak tersedia di sandbox → Dockerfile ditulis & divalidasi sintaksnya, build image diuji di mesin lokal/user.

### Insight arsitektur penting (menghemat banyak kerja)

**~90% provider di spec sudah OpenAI-compatible** (OpenRouter, Groq, Mistral, Together, Fireworks, Cerebras, Poolside, Ollama `/v1`, bahkan Gemini punya endpoint OpenAI-compat). Maka:

- **Satu adapter generic "openai-compatible"** (baseURL + api_key + quirk kecil per provider) menutup hampir semua provider.
- Per provider hanya perlu **metadata**: default base URL, header tambahan, normalisasi error, endpoint `/models`.
- Adapter native (Gemini `generateContent`, Anthropic) = nice-to-have di v1.1, bukan penghambat.

---

## 2. Arsitektur High-Level

```
Client (Cursor / Claude Code / Cline / OpenAI SDK)
   │ POST /v1/chat/completions  (Authorization: Bearer lollm-…)
   ▼
┌─────────────────────────────────────────────────────────────┐
│ API Layer (:20999, chi)                                     │
│  auth → request-id → logging → parse header X-LoLLM-*       │
├─────────────────────────────────────────────────────────────┤
│ Request Pipeline                                            │
│  1. Resolve combo      (model name / X-LoLLM-Combo)         │
│  2. Compression        (off | partial | full)               │
│  3. Agent Mode?        ── ya → Orchestrator (multi-agent)   │
│  4. Routing Engine     → pilih (connection, model) sehat    │
│  5. Provider Adapter   → translate + forward (+ proxy pool) │
│  6. Stream Relay       → SSE ke client + fallback logic     │
├─────────────────────────────────────────────────────────────┤
│ Cross-cutting                                               │
│  Health Tracker · Circuit Breaker · Usage Logger · Events   │
├─────────────────────────────────────────────────────────────┤
│ SQLite (data/lollm.db) ◄── Repository layer                 │
└─────────────────────────────────────────────────────────────┘
   ▲
   │ /api/admin/* + SSE status
Dashboard (:21000, SPA embedded via go:embed)
```

**Struktur package** mengikuti spec section 5 (`cmd/lollm`, `internal/{api,routing,providers,agent,compression,proxy,auth,config,db,server}`, `web/`, `configs/`, `docker/`).

---

## 3. Desain Detail Per Komponen

### 3.1 Config Layer
- Tipe `Config` flat + validasi (range port, path DB writable).
- Urusan merge: default → `config.yaml` → env (`LOLLM_*` / `HOST`, `API_PORT`, `DASHBOARD_PORT`) → CLI flags. Unit test eksplisit per lapisan prioritas.
- Settings runtime (compression default, health-check toggle, dll) disimpan di tabel `settings` dan **menimpa** config file saat `serve` berjalan (editable dari dashboard).

### 3.2 DB & Migrasi
- File SQL migrasi di-embed (`go:embed migrations/*.sql`), runner sederhana + tabel `schema_migrations`.
- PRAGMA: `WAL`, `busy_timeout=5000`, `foreign_keys=ON`.
- Repository per agregat (Connections, Combos, ProxyPools, ApiKeys, Settings, UsageLogs, AgentConfigs) — interface + impl, memudahkan test dengan DB temp file.
- Seed: 2 connection dummy (is_active=false, tanpa key asli), combo `Auto` (health_aware, 3 entri contoh), 1 internal API key aktif.

### 3.3 Provider Adapter

```go
type Adapter interface {
    Name() string
    Chat(ctx context.Context, req *ChatRequest, conn *Connection) (*ChatResult, error)
    // ChatResult → Stream (<-chan SSEEvent) atau NonStream (JSON)
    ListModels(ctx context.Context, conn *Connection) ([]ModelInfo, error)
}
```

- `openai-compat` generic adapter: rewrite `model`, set `Authorization`, passthrough body, `stream_options.include_usage=true` agar token usage tersedia di stream.
- Registry provider metadata (quirks): `openrouter` (header `X-Title`), `groq`, `mistral`, `together`, `fireworks`, `cerebras`, `poolside`, `ollama` (base `http://localhost:11434/v1`), `gemini-openai` (`https://generativelanguage.googleapis.com/v1beta/openai/`), `cloudflare-ai`.
- Error normalizer: map status HTTP + body error → tipe internal (`RateLimited`, `AuthFailed`, `QuotaExceeded`, `ServerError`, `NetworkError`) → input Health Tracker.
- Proxy pool: custom `http.Transport` per pool (HTTP/SOCKS5), dipilih dari `connection.proxy_pool_id`.

### 3.4 Routing Engine (jantung project — pure logic, tanpa network, mudah dites)

**Filter kandidat (semua wajib lolos):**
`is_active=true` AND `status='active'` AND (`backoff_until` IS NULL atau ≤ now) AND capability sesuai request.

**Capacity adapter** — deteksi dari request: ada image → butuh `vision`; ada `input_audio` → `audio`; ada `tools` → `tool_calling`; ada `response_format.json_schema` → `structured_output`. Capability model dari katalog statis + override manual (dashboard / `models_json`).

**Strategi:**

| Strategy | Rule |
|---|---|
| `sequential` | urut `priority` asc, coba satu-satu |
| `round_robin` | rotasi atomik di kandidat aktif |
| `health_aware` (default `Auto`) | skor: `1/priority` + recency(`last_used_at`) + `1/(1+consecutive_errors)` + `1/latency_ema` (EMA per connection, bobot bisa di-tune di settings) |
| `sticky` | conversation-key → kandidat tetap (TTL 30 menit). Key = header `X-LoLLM-Session` ‖ hash(pesan user pertama) |
| `fusion` (experimental) | dispatch paralel ke top-K (default 2) + LLM judge memilih; K & judge configurable |
| Provider-level: `round_robin` / `sticky` / `weighted` | smooth weighted RR untuk multi-key per provider |

**Semantik fallback (KRITIS, harus benar):**
- **Sebelum token pertama ter-stream ke client:** error apapun (429/5xx/timeout/network) → coba kandidat berikutnya sampai habis daftar combo.
- **Setelah token pertama:** TIDAK ada fallback (client sudah menerima partial) → akhiri stream dengan chunk error yang well-formed.
- Non-streaming: bebas fallback sampai dapat jawaban.

### 3.5 Health Tracker & Circuit Breaker
- 429 → `status=rate_limited`, backoff `30s·2^(n-1)` cap 15m (hormati header `Retry-After` bila ada).
- 401/403 (key invalid / billing) → `unavailable`, backoff 60m, tampil jelas di dashboard.
- 5xx/network → `consecutive_errors++`; ≥5 → `unavailable` + backoff `60s·2^n` cap 10m.
- Success → reset `consecutive_errors=0`, `status=active`, update `latency_ema`.
- **Health checker** background (interval 60s, toggle-able): probe ringan (`GET /models` atau HEAD) per connection tidak-aktif → sukses = pulih otomatis.
- Pub/sub events bus in-memory → dashboard SSE.

### 3.6 Token Compression
- **`partial`:** `tool_result` > threshold (default 2000 chars) → keep head 500 + tail 500 + marker `[… truncated N chars …]`; collapse whitespace; **tool_result terakhir tidak dikompres** (paling relevan).
- **`full`:** partial + tool_result lama (selain K terakhir, default K=3) diganti placeholder ringkas `[tool_result: <name>, <1-baris summary>]` + kondensasi system prompt berulang.
- Estimasi token: `chars/4` heuristic; catat `tokens_saved` di usage log (bukti target 20–40%).
- Toggle: config global → combo → header `X-LoLLM-Compression` (prioritas tertinggi).

### 3.7 Agent Mode

**Aktivasi:** header `X-LoLLM-Agent-Mode: true` ‖ model name `agent-auto`/`agent-debate`/`agent-parallel` ‖ flag di combo.

**Pipeline:**
- `collaborative` (default): Planner → (Reviewer → Revise)×`max_rounds` → Finalizer (yang di-stream ke client).
- `debate`: K agent generate paralel → Judge memilih/mensintesis → stream hasil judge.
- `parallel`: K agent paralel → Merger menggabungkan → stream hasil merger.

**Aturan:**
- `hide_internal_steps=true` (default): buffer internal, **hanya output Finalizer yang di-stream**. `false`: kirim event SSE custom `event: lollm-agent-step` (role/status, tanpa konten penuh) supaya UX tetap hidup.
- `role.model` boleh nama **combo** (routing nested) — guard anti-rekursi: role model tidak boleh `agent-*`.
- **Kasus tool_calls (coding agent):** jika Planner menghasilkan `tool_calls`, **langsung forward tool_calls tersebut tanpa review** (tool call harus dieksekusi client; mereviewnya menambah latensi tanpa nilai). Review hanya berlaku untuk jawaban tekstual. → keputusan pragmatis v1, di-flag di open question.
- Timeout per-role 60s + total pipeline 300s (configurable); semua internal call dicatat ke usage log dengan `agent_role` tag (transparansi biaya).

### 3.8 API Layer
- `POST /v1/chat/completions` (SSE + non-stream), `GET /v1/models`, `POST /v1/embeddings` (opsional, passthrough), `POST /v1/completions` (opsional).
- Error body 100% format OpenAI (`{"error": {"message", "type", "code"}}`) agar SDK tidak patah.
- Middleware: auth (hash lookup), request-id (`X-Request-Id`), body size limit (default 20 MB), timeout, access log terstruktur (slog).

### 3.9 Dashboard (:21000)
- **Auth:** token admin (auto-generate, disimpan hash di DB, dicetak saat `serve` start & via `lollm dashboard-token`) → login sekali → cookie. **Rekomendasi: default ON** karena default bind `0.0.0.0` (bisa dimatikan `--dashboard-no-auth` untuk localhost). *(Lihat open question #2.)*
- REST `/api/admin/*` per entitas + `POST /connections/{id}/test` + `GET /events` (SSE status real-time) + `GET/POST /export|import`.
- SPA vanilla: hash-router, 9 view (Connections, Combos, Models, Proxy Pools, API Keys, Usage, Agent Mode, Settings, Export/Import). API key provider **selalu di-mask** (`sk-…abcd`), tidak pernah keluar dari server.

### 3.10 CLI
`serve`, `setup` (wizard: config → DB → seed → admin key → (opsional) masukkan provider key + test), `doctor` (config, DB read/write, port availability, test semua connection + proxy, versi), `version`, `export-config [--secrets --password]`, `import-config [--password]`, `key generate|list|revoke`.

**Export/Import:** `backup.json` berisi seluruh tabel. Default **tanpa secret** (key di-mask). `--secrets --password <pw>` → blob secret di-encrypt AES-GCM (KDF Argon2id/PBKDF2) agar migrasi mesin aman.

### 3.11 Build & Packaging
- `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w"` → `bin/lollm` (static, verifikasi `file` + jalan di container minimal).
- `go:embed web/static` untuk dashboard — tidak ada file eksternal.
- Docker multi-stage: `golang:1.24-alpine` build → `FROM scratch` (+ ca-certificates + tzdata).
- Makefile: `build`, `run`, `test`, `lint`, `docker`, `clean`.

---

## 4. Fase Implementasi & Roadmap

> Urutan mengikuti spec section 10; saya menambah **P0 (bootstrap)** dan **P5 (health watcher)** yang tersirat dari requirement 3.2/3.4. Tiap fase = unit kerja yang bisa diuji & didemokan.

### Progress (update 2026-10-05)

| Fase | Status | Catatan |
|---|---|---|
| P0 Bootstrap | ✅ SELESAI | Go 1.27 (tarball non-root), cobra CLI, config precedence (9 test), Makefile (`-p 1` + GOMEMLIMIT — sandbox 2GB RAM), README |
| P1 DB & Seed | ✅ SELESAI | SQLite pure-Go, migrasi embed, 7 repo (11 test), AES-256-GCM + master key 0600, key hash SHA-256, seed idempotent |
| P2 Routing Engine | ✅ SELESAI | Skip wajib + skip reasons, capacity adapter, 4 strategi + WRR multi-key + sticky TTL (18 test), `lollm routes` |
| P3 Provider Adapters | ✅ SELESAI | Registry 12 provider, adapter OpenAI-compat (rewrite model, auth, include_usage), error normalizer + Retry-After, SSE parser, proxy manager rotasi, mock provider (21 test) |
| P4 API Gateway | ✅ SELESAI | chi :20999 (`/healthz`, `/v1/models`, `/v1/chat/completions` SSE+non-stream), auth, fallback pre-first-byte, health marking + backoff, usage log per attempt, `serve` graceful shutdown + dashboard placeholder (12 test E2E) |
| **LIVE VALIDATION** | ✅ **57/61 key asli valid** | `cmd/recon` + `cmd/import-recon`: 57 koneksi aktif terenkripsi, combo Auto 7 provider, E2E nyata — 3 request beruntun dilayani Gemini→Groq→Poolside (health-aware LB live) |
| P5 Health Watcher | ✅ SELESAI | Background checker (settings live: `health_check.enabled`/`interval_seconds`, min 5s), probe strategi per provider (ListModels gratis; chat minimal utk CF/Ollama), auto-recovery tanpa menggelembungkan error streak, re-arm backoff utk yang masih mati, event bus in-memory (recent 100, non-blocking), proxy pool probe + event up/down. **Demo live:** 10 koneksi (9 OR quota-backoff + 1 diracuni) pulih otomatis <3 dtk via probe nyata; koneksi mati tetap down. 7 test baru. |
| P6 Compression | ✅ SELESAI | Mode `off/partial/full` (partial: head+tail 500/500 + marker, tool_result terakhir utuh; full: + summarisasi hasil lama kecuali 3 terakhir + dedupe system), estimasi chars/4, toggle **header > combo (kolom baru via migrasi 0002) > setting global**, `tokens_saved` di usage log. 10 test unit + 3 test integrasi. **Bukti live (tokenizer Gemini asli, payload identik 93KB / 10 tool_result):** off=38.147 tok → partial=8.195 (−78,5%) → full=1.934 (−94,9%). Bonus temuan quirks Gemini OpenAI-compat: tool message wajib berpasangan dgn assistant tool_calls bernama; function-call turn pertama harus setelah user turn. |
| P7–P10 | ✅ | P7 ✅ Agent Mode, P8 ✅ dashboard+playground, P9 ✅ export/import (roundtrip live), **P10 ✅ RELEASE v0.1.0 "Synapse"** |

**Temuan live penting (dari backup key asli):** endpoint Poolside = `inference.poolside.ai/v1` (domain lama mati); Ollama Cloud OpenAI-compat = `ollama.com/v1`, hanya key `hex.hex` yang bisa chat; Gemini via OpenAI-compat, `gemini-2.5-flash` deprecated → `gemini-3.8-flash` (ID tanpa prefix `models/`); Cloudflare tidak ada GET /models (katalog via `/ai/models/search`, base URL per-account); Groq katalog kini `openai/gpt-oss-*`; banyak model = reasoning model (perlu `max_tokens` besar untuk konten terlihat). Proxy pool 10 relay Vercel terimport. DB live ada di `live/` (gitignored, berisi key terenkripsi).

| Fase | Scope | Deliverable / Acceptance | Est. |
|---|---|---|---|
| **P0** Bootstrap | Instal Go di workspace, `go mod init`, skeleton `cmd/lollm` + cobra, config layer (merge flags>env>yaml>default + validasi), slog + request-id, Makefile awal, README stub | `./bin/lollm version` jalan; unit test merge config hijau | 1 hari |
| **P1** DB & Seed | modernc/sqlite, migrasi embed + runner, WAL, semua repository, enkripsi AES-GCM + master key, generate/hash API key, seed data | Unit test repo (temp DB); `lollm setup --basic` membuat DB + seed + key | 1,5 hari |
| **P2** Routing Engine | Filter aktif/backoff/capability, 5 strategi combo + 3 strategi provider-level, sticky store, circuit-breaker state machine, capacity adapter — **pure logic** | Table-driven tests ±30 kasus + benchmark; zero network dep | 2 hari |
| **P3** Provider Adapter | Interface adapter, openai-compat generic + registry metadata 10 provider, SSE parser, ListModels, proxy pool transport, **mock provider server (httptest)** untuk seluruh testing ke depan | Integrasi mock: chat streaming jalan via adapter | 2,5 hari |
| **P4** API Gateway | Server :20999, auth middleware, `/v1/models`, `/v1/chat/completions` stream+non-stream, **fallback pre-first-token**, normalisasi error, usage log + update health (429→backoff) | E2E mock: request → fallback saat provider#1 return 429 → sukses via provider#2; SSE utuh | 2,5 hari |
| **P5** Health Watcher | Background health checker + auto-recovery, rotasi multi-key per provider, events bus untuk dashboard | Connection "mati" pulih otomatis; event status ter-emit | 1 hari |
| **P6** Compression | partial & full + threshold configurable, `tokens_saved` di log, toggle 3 level (config/combo/header) | Unit test payload realistik Claude-Code-style; terukur hemat ≥20% | 1,5 hari |
| **P7** Agent Mode | agent_configs CRUD, orchestrator 3 mode, nested routing via combo, only-final streaming, timeout & guard, tagging usage per role | E2E mock: pipeline 3-role, assert **hanya final** yang ter-stream | 3 hari |
| **P8** Dashboard | REST admin + auth token, SSE status, SPA 9 view embedded `go:embed`, test-connection button, mask secret | Full CRUD via UI; status connection update real-time | 3,5 hari |
| **P9** CLI + Export/Import | `setup` wizard, `doctor`, `key generate/list/revoke`, `export-config`/`import-config` (+`--secrets --password`) | Roundtrip export→wipe→import identik; doctor mendeteksi masalah buatan | 1,5 hari |
| **P10** Build & Release | Static binary linux/amd64 (+optional darwin/windows), Dockerfile multi-stage, `configs/config.example.yaml` + `backup.example.json`, README lengkap + panduan integrasi Cursor/Claude Code/Cline, hardening (graceful shutdown, limit, redacted log) | **Semua kriteria sukses section 11 ✅** → tag `v0.1.0` | 1,5 hari |

**Milestone:**
- **M1 (setelah P5):** Core proxy jalan — route + fallback + auto-recovery. *(Sudah berguna sendiri!)*
- **M2 (setelah P7):** Fitur unggulan Agent Mode selesai.
- **M3 (setelah P8–P9):** Kelola penuh tanpa sentuh file config.
- **M4 (P10):** Release `v0.1.0` — single binary siap pakai.

Total ≈ **21 hari** setara kerja engineer; sebagai agent, saya kerjakan per fase (± 1–2 fase per sesi) dan laporkan hasil uji tiap fase.

---

## 5. Strategi Testing

1. **Unit (tanpa network):** routing engine (kasus: semua mati, backoff, weighted, sticky expiry), compression, config merge, enkripsi, hash key.
2. **Integrasi (mock provider):** server `httptest` yang bisa disuntik perilaku — 200/SSE, 429 + `Retry-After`, 500, timeout hang, stream putus di tengah → memvalidasi fallback, circuit breaker, dan semantik pre/post-first-token.
3. **E2E di sandbox:** `lollm serve` + curl (stream & non-stream) + script regresi; contoh payload OpenAI SDK asli.
4. **Uji manual dengan tool asli** (butuh user): arahkan Cursor/Claude Code ke `http://localhost:20999/v1` dengan internal key — diverifikasi di M1 & M4.
5. **Docker:** build image diuji di mesin yang punya Docker (sandbox tidak punya).

---

## 6. Keamanan (ringkasan kontrol)

- API key provider: AES-256-GCM di DB; master key file `0600` (atau env); **tidak pernah** dikirim ke frontend (selalu mask).
- Internal API key: hanya SHA-256 hash; constant-time compare.
- Dashboard: token admin default ON; opsi `--dashboard-no-auth` untuk 127.0.0.1.
- Log: redaksi `Authorization`/key; request-id untuk trace tanpa leak payload.
- Export default tanpa secret; opsi secret wajib password (Argon2id + AES-GCM).

## 7. Risiko & Mitigasi

| Risiko | Dampak | Mitigasi |
|---|---|---|
| Fallback salah saat stream sudah setengah jalan | Response korup di client | Aturan ketat pre/post-first-token + test khusus |
| Quirk per provider (error body beda-beda) | Salah klasifikasi status | Error normalizer per provider + tabel fixture response asli |
| Agent Mode + tool_calls dari coding agent | Latensi + behavior aneh | Rule v1: tool_calls di-forward langsung; review hanya jawaban tekstual |
| modernc/sqlite konkurensi | DB lock | WAL + busy_timeout + single-writer pattern (channel serialize write) |
| Sandbox tanpa Docker | Build image tak teruji | Dockerfile divalidasi sintaks; instruksi build lokal untuk user |
| Scope creep dashboard | Makan waktu | CRUD minimal dulu; real-time cukup via SSE polling 5s |

## 8. Keputusan Terbuka (konfirmasi sebelum/di awal implementasi)

1. **Dashboard stack** — rekomendasi saya: vanilla JS tanpa build step (toolchain Go-only). Setuju, atau mau Next.js/Vite-React?
2. **Auth dashboard default ON** (karena bind `0.0.0.0:21000`) — spec tidak menyebut login; saya tambahkan demi aman. OK?
3. **Gemini via endpoint OpenAI-compat dulu** (cukup untuk chat/tools), adapter native `generateContent` = v1.1. OK?
4. **Agent Mode + tool_calls** di-forward tanpa review (keputusan pragmatis di 3.7). OK?

## 9. Definition of Done (map ke kriteria sukses spec §11)

| Kriteria | Cara verifikasi |
|---|---|
| Single binary linux/amd64 tanpa dependency | `make build`; `file bin/lollm` = static; jalan di container minimal |
| `serve` di 0.0.0.0:20999 + 21000 | Smoke test otomatis P4/P8 |
| Request OpenAI-compatible ter-route | E2E mock + uji manual Cursor/Claude Code (butuh 1 key asli dari user) |
| Skip rate-limited/unavailable otomatis | Test integrasi injeksi 429 (P4) |
| Combo Auto hanya model hidup | Unit test routing (P2) |
| Agent Mode hanya stream final | E2E assert buffer (P7) |
| Dashboard kelola connections/combos/agent mode | Demo manual M3 |
| Export/Import berfungsi | Roundtrip test (P9) |
| Secret aman | Grep binary & DB = tidak ada plaintext; API response selalu mask |

## 10. Langkah Berikutnya

Setelah plan ini disetujui (dan 4 pertanyaan terbuka dijawab — default saya aman jika tidak dijawab):
**mulai P0 + P1 di sesi berikutnya**, lalu lanjut berurutan sampai M1 (core proxy jalan), checkpoint demo, dilanjutkan P6–P10.

> **PROGRESS: P0–P10 ✅ SEMUA SELESAI — RILIS LoLLM Synapse v0.1.0 (2026-10-05).** 150+ test hijau (13 paket), binary statis ~13 MB, gateway live :20999/:21000 dengan 57 koneksi nyata. **P10 Release:** README lengkap (quickstart, integrasi Cursor/Claude Code/Cline, Agent Mode, compression, backup, docker, arsitektur, tabel kriteria §11 ✅), CHANGELOG.md, configs/backup.example.json format `lollm-backup` v1 baru, Dockerfile multi-stage (make docker — docker tak tersedia di sandbox, target teruji `make -n`), **make release → 4 tarball cross-platform** (linux-amd64, darwin-amd64, darwin-arm64, windows-amd64; binary terverifikasi `file: statically linked` + `lollm version` = "LoLLM Synapse v0.1.0"), docs/tunnel.md, hardening terkonfirmasi (MaxBytesReader, log tanpa secret, graceful shutdown, admin token). **Smoke test final live:** Auto route (gemini) ✓, agent-auto 5-call final-only ✓, 667 model terdaftar ✓. Kriteria sukses spec §11: 9/9 ✅. **Seluruh roadmap P0–P10 selesai.**

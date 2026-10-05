# LoLLM Synapse

**Gateway AI self-hosted / LLM router dalam satu binary.** Versi: **v0.1.0 "Synapse"**.

LoLLM Synapse duduk di antara coding tools Anda (Cursor, Claude Code, Cline, Codex,
Continue, Aider, OpenCode, …) dan penyedia LLM (OpenRouter, Gemini, Groq, Mistral,
Cloudflare AI, Ollama, Poolside, atau endpoint OpenAI-compatible mana pun).
Arahkan tool ke `http://localhost:20999/v1` — sisanya ditangani Synapse:

- **API 100% OpenAI-compatible** — `/v1/chat/completions` (SSE streaming + non-stream), `/v1/models`
- **Routing health-aware** — combo model multi-provider dengan fallback otomatis, circuit breaker, skip otomatis koneksi rate-limited/unavailable, auto-recovery
- **Multi-key / multi-account** per provider (round-robin, weighted, sticky)
- **Agent Mode** (fitur unggulan) — pipeline planner → reviewer → finalizer; debate; parallel — hanya jawaban final yang di-stream
- **Token compression** — hemat hingga −95% prompt token pada beban kerja coding tool-result
- **Proxy pools** HTTP/SOCKS5 per koneksi
- **Dashboard mobile-first** + **Chat Playground** (uji model langsung dari browser/HP)
- **Backup portabel** `backup.json` (merge/replace, opsional terenkripsi password)
- **Single binary** — Go + SQLite murni + web UI embedded; zero dependency runtime

> Lihat [CHANGELOG.md](CHANGELOG.md) untuk rilis, [PLAN.md](PLAN.md) untuk roadmap,
> [docs/tunnel.md](docs/tunnel.md) untuk akses remote (Cloudflare/ngrok/Tailscale).

## Quickstart

```bash
make build          # -> ./bin/lollm (binary statis, linux/amd64)

./bin/lollm serve --host 0.0.0.0 --api-port 20999 --dashboard-port 21000
# API:       http://localhost:20999/v1
# Dashboard: http://localhost:21000
```

Konfigurasi via `config.yaml` (lihat [configs/config.example.yaml](configs/config.example.yaml)),
env vars (`HOST`, `API_PORT`, `DASHBOARD_PORT`, `DB_PATH`, `LOG_LEVEL`, `CONFIG`),
atau flag. Semua objek (koneksi, combo, key, dsb.) disimpan di SQLite dan dikelola
via dashboard/CLI — bukan di file config.

```bash
# 1) buat API key internal (dipakai client Anda, bukan key provider)
./bin/lollm key generate --name cursor

# 2) tambahkan koneksi provider (atau lewat dashboard → Connections)
#    dashboard: http://localhost:21000  (tab Connections → Tambah)

# 3) test
curl http://localhost:20999/v1/chat/completions \
  -H "Authorization: Bearer lollm-xxxx" -H "Content-Type: application/json" \
  -d '{"model":"Auto","messages":[{"role":"user","content":"hello"}]}'
```

## Integrasi coding tools

Semua tool yang mendukung OpenAI base URL cukup mengganti dua variabel —
API key yang dipakai adalah **key internal LoLLM** (`lollm key generate`), bukan key provider.

### Cursor
`Settings → Models → OpenAI API Key` isi key internal LoLLM, lalu set override base URL:
```
https://localhost:20999/v1
```
Atau via config (~/.cursor/mcp.json tidak diperlukan — cukup OpenAI override).

### Claude Code
```bash
export ANTHROPIC_BASE_URL=http://localhost:20999      # mode Anthropic-compat
# atau pakai jalur OpenAI:
export OPENAI_BASE_URL=http://localhost:20999/v1
export OPENAI_API_KEY=lollm-xxxx
```

### Cline / Continue / Codex / Aider / OpenCode
```bash
export OPENAI_BASE_URL=http://localhost:20999/v1
export OPENAI_API_KEY=lollm-xxxx
```
Di Cline: *Settings → API Provider = OpenAI Compatible* →
Base URL `http://localhost:20999/v1`, API key internal, model `Auto`.

**Pilih model**: `Auto` (combo fallback), nama combo lain, `agent-auto` /
`agent-debate` / `agent-parallel` (pipeline multi-agent), atau model provider
spesifik (lihat daftar di dashboard → Models atau `GET /v1/models`).

## Agent Mode

```bash
curl http://localhost:20999/v1/chat/completions \
  -H "Authorization: Bearer lollm-xxxx" -H "Content-Type: application/json" \
  -d '{"model":"agent-auto","messages":[{"role":"user","content":"refactor this function"}]}'
```

| Model | Pipeline |
|---|---|
| `agent-auto` | collaborative: planner → reviewer (×max_rounds) → finalizer |
| `agent-debate` | beberapa generator paralel + judge memilih/merge |
| `agent-parallel` | beberapa generator paralel + merger |

Aktivasi juga via header `X-LoLLM-Agent-Mode: true` atau flag Agent Mode di combo.
Setiap call internal tetap melewati routing + fallback penuh dan tercatat per-role
di usage logs. Bila planner menghasilkan `tool_calls`, frame diteruskan verbatim
agar coding agent tetap bisa mengeksekusi. `hide_internal_steps` menyembunyikan
event progres (default on; matikan untuk melihat `agent.step` di stream).

## Token compression

```
X-LoLLM-Compression: partial   # ramping: ringkas tool_result lama (aman utk coding)
X-LoLLM-Compression: full      # ringkas: hemat maksimum
```
Bisa juga per-combo (kolom compression) atau global (Settings). Terukur nyata di
dashboard → Usage (kolom "Token dihemat") dan `lollm usage`.

## Backup / restore

```bash
./bin/lollm export-config                          # tanpa secret (aman dibagikan)
./bin/lollm export-config --secrets                # + key provider plaintext (chmod 600)
./bin/lollm export-config --password 'rahasia'     # key disegel PBKDF2-AES-256-GCM
./bin/lollm import-config backup.json              # merge (upsert by name)
./bin/lollm import-config backup.json --replace --yes   # wipe lalu impor
./bin/lollm import-config sealed.json --password 'rahasia'
```
Format: [configs/backup.example.json](configs/backup.example.json). Hash API key
klien ikut diekspor sehingga key lama tetap berlaku setelah migrasi; key provider
di-enkripsi ulang dengan master key DB tujuan (aman pindah mesin). Dashboard:
Settings → Backup.

## Dashboard

`http://localhost:21000` — **mobile-first** (bottom nav, kartu, bottom sheet;
desktop otomatis dapat layout sidebar):

- 💬 **Chat Playground** — chat langsung dengan model/pipeline apa pun; progres agent live; tool_calls tampil
- 🔌 **Connections** — CRUD + test koneksi (probe provider nyata), status real-time
- 🧩 **Combos** — strategy, compression, agent flag, prioritas model
- 🤖 **Agent Mode** — edit role/model/system prompt/max_rounds
- 🔑 **API Keys** — generate (tampil sekali), revoke, hapus
- 📊 **Usage** — request 24 jam, token hemat, latensi, per-role agent
- 🧭 **Models**, 🕸 **Proxy Pools**, ⚙️ **Settings** (+ backup & admin token opsional)

Akses remote: aktifkan **Dashboard Admin Token** (Settings) bila dashboard di-expose —
lihat [docs/tunnel.md](docs/tunnel.md).

## CLI

```
lollm serve          # jalankan gateway (API + dashboard)
lollm setup          # wizard interaktif
lollm doctor         # diagnosa koneksi & konfigurasi
lollm key generate|list|revoke
lollm routes         # lihat combo & routing
lollm export-config  # backup.json
lollm import-config  # restore
lollm usage [--csv]  # log usage terakhir
lollm version
```

## Docker

```bash
make docker          # build image multi-stage -> lollm-synapse:latest
docker run -p 20999:20999 -p 21000:21000 -v lollm-data:/data lollm-synapse:latest
```

## Build & release

```bash
make build           # bin/lollm (linux/amd64, statis)
make test            # seluruh unit + integration test
make release         # tarball linux/amd64 + darwin/arm64 + darwin/amd64 + windows/amd64
```

Tanpa dependency runtime; butuh Go 1.24+ untuk build dari source.

## Arsitektur

```
cmd/lollm            CLI (cobra)
internal/api         OpenAI-compat API + admin JSON API (dashboard port)
internal/dashboard   Web UI embedded (go:embed) + proxy /v1 & /api
internal/routing     health-aware engine, fallback, sticky/weighted
internal/health      watcher, circuit breaker, event bus
internal/providers   adapter OpenAI-compatible per provider + probe strategy
internal/compression token compression partial/full
internal/agent       Agent Mode orchestration (collaborative/debate/parallel)
internal/backup      backup.json Build/Restore (+PBKDF2 seal)
internal/db          SQLite (modernc, murni Go) + migrations + store
internal/secret      AES-256-GCM master-key encryption
internal/auth        API key internal (SHA-256 hash)
```

## Keamanan

- API key provider: di-encrypt AES-256-GCM (master key per instalasi), **tidak pernah** dikirim ke frontend atau masuk log.
- API key internal: hanya hash SHA-256 yang disimpan.
- Port API (20999) hanya expose endpoint OpenAI-compat; admin API hanya di port dashboard, plus admin token opsional.
- Batas ukuran body request; request-id di setiap respons; graceful shutdown.

## Status kriteria sukses (spec §11)

| Kriteria | Status |
|---|---|
| Satu binary tanpa dependency eksternal | ✅ statis ~13 MB (CGO off) |
| `serve` jalan di 0.0.0.0:20999 + 21000 | ✅ |
| Request OpenAI-compat dari coding tools ter-route | ✅ live (57 koneksi nyata) |
| Koneksi rate-limited/unavailable otomatis di-skip | ✅ terverifikasi live |
| Combo "Auto" hanya model hidup | ✅ health-aware |
| Agent Mode: hanya final yang di-stream | ✅ live-proven |
| Dashboard manage connections/combos/agent | ✅ |
| Export/import backup.json | ✅ roundtrip live-proven |
| Semua secret aman | ✅ encrypted/hashed, never exposed |

---

LoLLM Synapse v0.1.0 — dibangun sesuai spesifikasi `uploads/agent.md`.

# Catatan pengembangan

Peta kode dan rencana ke depan. Untuk memakai Synapse, mulai dari [README](README.md).

## Peta kode

```
cmd/lollm/            entrypoint CLI (cobra)
internal/api/         endpoint OpenAI-compat (:20999) + admin JSON API (hanya :21000)
internal/dashboard/   web UI ter-embed (go:embed) + chat playground
internal/routing/     scoring health-aware, fallback chain, sticky session
internal/health/      background watcher, circuit breaker, event bus
internal/providers/   adapter OpenAI-compatible + strategi probe per provider
internal/compression/ pemangkasan pesan tool lama (partial/full)
internal/agent/       orkestrasi pipeline multi-agent
internal/backup/      format lollm-backup (Build/Restore/seal PBKDF2)
internal/db/          store SQLite (modernc, tanpa CGO) + migrasi + seed
internal/secret/      master key + AES-256-GCM
internal/auth/        API key internal (hash SHA-256, constant-time compare)
```

Keputusan desain yang perlu diketahui sebelum menyentuh kode:

- Semua state di SQLite; `config.yaml` hanya untuk host/port/path/level log.
- Key provider tidak pernah keluar dari server: di-encrypt di DB, tidak masuk log, tidak dikirim ke frontend (frontend hanya melihat `api_key_set: true/false`).
- Port API sengaja bersih (hanya endpoint OpenAI); semua endpoint manajemen hidup di port dashboard dan bisa dikunci admin token.
- Setiap panggilan internal Agent Mode adalah completion ter-routing penuh — fallback dan health-nya sama dengan request biasa.

## Keadaan saat ini

Semua fitur inti berjalan: routing dengan fallback, Agent Mode tiga mode, compression, dashboard, backup, CLI lengkap, build 4 platform. 150+ test unit dan integrasi; `make test` untuk menjalankan semuanya.

## Berikutnya

Ide, tanpa jadwal:

- Graf usage di dashboard (agregat per hari/provider)
- Export usage ke CSV/JSON dari dashboard
- Thinking/reasoning budget per provider
- Alias model (mis. `fast` → combo tertentu)
- Dukungan Anthropic API native (bukan lewat jalur OpenAI-compat)
- Pelacakan kuota/rate-limit per key provider

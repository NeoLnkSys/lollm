# LoLLM Synapse

**Satu binary. Satu endpoint. Semua LLM Anda.**

Anda punya key Gemini, dua akun Groq, sisa kuota Mistral, OpenRouter, dan model lokal di Ollama. Tapi Cursor cuma mau satu base URL — dan begitu provider favorit kena rate limit, kerja berhenti.

Synapse berdiri di antara coding tool Anda dan semua provider itu. Tool cukup menunjuk `http://localhost:20999/v1` dengan satu API key internal. Routing, fallback saat koneksi gagal, pemulihan otomatis, sampai pemangkasan token — semua urusan Synapse.

## Kenapa Synapse

**Tidak gampang mati.** Setiap request melewati rantai fallback: provider pertama kena 429 atau 503? Dalam hitungan milidetik Synapse pindah ke koneksi berikutnya. Koneksi bermasalah otomatis masuk backoff, di-probe berkala, dan kembali bertugas begitu pulih. Anda tidak perlu menyentuh apa pun.

**Hemat token di pekerjaan coding.** Percakapan coding penuh output tool yang panjang dan berulang. Synapse meringkas bagian lama sebelum diteruskan ke provider — pemangkasan ini terlihat langsung di kolom "token dihemat" pada dashboard.

**Dashboard di HP.** UI-nya mobile-first: pantau status koneksi secara real-time, uji koneksi ke provider, kelola combo dan API key, lalu coba model mana pun lewat chat playground bawaan — semuanya dari browser ponsel.

**Satu file, nol dependency.** Binary statis sekitar 13 MB. SQLite ter-embed, web UI ter-embed, tidak ada runtime yang harus dipasang. Unduh, jalankan, selesai.

## Install

Cara cepat (Linux/macOS):

```bash
curl -fsSL https://github.com/NeoLnkSys/lollm/raw/main/scripts/installer.sh | bash
```

Installer mendeteksi OS dan arsitektur, memilih direktori instalasi (root → `/usr/local/bin`, user → `~/.local/bin`), memverifikasi checksum, dan membetulkan PATH bila perlu. Opsi lain: `--version vX.Y.Z`, `--dest DIR`, `--uninstall`.

Manual: unduh tarball dari [Releases](https://github.com/NeoLnkSys/lollm/releases) untuk platform Anda (linux/amd64, darwin/amd64, darwin/arm64, windows/amd64), ekstrak, dan letakkan `lollm` di PATH.

Dari source:

```bash
make build    # butuh Go 1.24+
```

## 30 detik pertama

```bash
lollm serve           # API :20999 · dashboard :21000
lollm key generate    # buat API key internal
```

Buka `http://localhost:21000`, tambahkan koneksi provider (tab Connections), lalu arahkan tool Anda ke `http://localhost:20999/v1` dengan key tadi. Selesai — combo bawaan `Auto` sudah siap memilih provider tercepat yang hidup.

## Sambungkan coding tool

Semua tool yang paham OpenAI base URL. API key yang dipakai adalah key internal LoLLM, bukan key provider.

**Cursor** — Settings → Models → OpenAI API Key: isi key internal, base URL `http://localhost:20999/v1`.

**Claude Code**

```bash
export ANTHROPIC_BASE_URL=http://localhost:20999
# atau jalur OpenAI:
export OPENAI_BASE_URL=http://localhost:20999/v1
export OPENAI_API_KEY=lollm-xxxx
```

**Cline / Continue / Codex / Aider / OpenCode** — pilih provider "OpenAI Compatible", base URL `http://localhost:20999/v1`, key internal, model `Auto`.

Model bisa nama combo atau model provider tertentu — daftarnya di dashboard atau `GET /v1/models`.

## Pemangkasan token

```
X-LoLLM-Compression: partial   # ringkas tool output lama
X-LoLLM-Compression: full      # agresif
```

Bisa per-request (header), per-combo, atau global. Matikan saja bila tidak mau.

## Backup & pindah mesin

```bash
lollm export-config                       # tanpa secret — aman dibagikan
lollm export-config --password 'rahasia'  # key provider disegel AES-256-GCM
lollm import-config backup.json            # merge
lollm import-config backup.json --replace  # ganti total
```

Hash API key klien ikut serta, jadi key lama tetap berlaku setelah pindah mesin. Bisa juga lewat dashboard → Settings → Backup.

## Docker

```bash
make docker
docker run -p 20999:20999 -p 21000:21000 -v lollm-data:/data lollm-synapse:0.1.0
```

## Di dalam binary

```
cmd/lollm            CLI
internal/api         endpoint OpenAI-compat + admin API (port dashboard saja)
internal/dashboard   web UI ter-embed + chat playground
internal/routing     mesin health-aware, fallback, weighted/sticky
internal/health      watcher, circuit breaker, auto-recovery
internal/providers   adapter per provider + strategi probe
internal/compression pemangkasan token
internal/backup      format backup.json
internal/db          SQLite murni (tanpa CGO) + migrasi
internal/secret      enkripsi master-key AES-256-GCM
internal/auth        API key internal (hash SHA-256)
```

Konfigurasi: `config.yaml` → env (`HOST`, `API_PORT`, `DASHBOARD_PORT`, `DB_PATH`, `LOG_LEVEL`) → flag CLI. Contoh lengkap di [configs/config.example.yaml](configs/config.example.yaml).

---

Ditulis dalam Go murni (CGO off), diuji 150+ test unit dan integrasi. Butuh akses dari luar jaringan? Lihat [docs/tunnel.md](docs/tunnel.md).

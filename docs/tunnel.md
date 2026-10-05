# Akses remote (tunnel)

Gateway biasanya berjalan di jaringan lokal (`http://0.0.0.0:20999` API, `http://0.0.0.0:21000` dashboard). Kalau butuh akses dari luar — coding agent di cloud, atau dashboard dari HP — berikut caranya.

## 1. Cloudflare Tunnel (rekomendasi — gratis & stabil)

```bash
# install
curl -L https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64 -o /usr/local/bin/cloudflared
chmod +x /usr/local/bin/cloudflared

# quick tunnel (tanpa akun, domain acak trycloudflare.com)
cloudflared tunnel --url http://localhost:20999
# → https://<random>.trycloudflare.com   (arahkan base_url ke sini)
```

Dengan akun Cloudflare (domain sendiri, hostname tetap):

```bash
cloudflared tunnel login
cloudflared tunnel create lollm-api
cloudflared tunnel route dns lollm-api api.contohdomain.com
cloudflared tunnel run --url http://localhost:20999 lollm-api
```

Ulangi `tunnel run --url http://localhost:21000` dengan nama lain untuk dashboard (atau biarkan dashboard hanya di LAN — lebih aman).

## 2. ngrok

```bash
ngrok config add-authtoken <TOKEN>
ngrok http 20999    # API
ngrok http 21000    # dashboard (terpisah)
```

## 3. Tailscale / WireGuard (mesh VPN — tidak expose ke internet publik)

```bash
tailscale up
# gateway otomatis reachable di http://<tailscale-ip>:20999
./lollm serve --host 0.0.0.0 --api-port 20999 --dashboard-port 21000
```

## Keamanan saat expose

1. **API key internal wajib** — semua endpoint `/v1/*` menolak request tanpa `Authorization: Bearer lollm-…`.
2. **Aktifkan dashboard admin token** (Settings → Dashboard Admin Token, atau via API) kalau dashboard ikut di-expose. Tanpa token, siapa pun yang bisa reach port 21000 bisa mengelola gateway.
3. **Export backup tanpa `--secrets`** kalau file akan berpindah tangan; gunakan `--password` untuk backup yang menyimpan API key provider.
4. Cloudflare Access (Zero Trust) bisa menambah login SSO di depan tunnel bila perlu.

## Contoh: coding agent via tunnel

```bash
# di mesin cloud
export OPENAI_BASE_URL=https://<random>.trycloudflare.com/v1
export OPENAI_API_KEY=lollm-xxxx   # key internal LoLLM, bukan key provider
```

Semua request kini melewati routing, fallback multi-provider, dan Agent Mode Synapse.

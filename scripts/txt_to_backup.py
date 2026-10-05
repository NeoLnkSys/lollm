#!/usr/bin/env python3
"""Konversi uploads/backup_clean-1.txt (export teks proxy+keys) menjadi
backup.json format `lollm-backup` v1 (plain secrets) — restorable via
`lollm import-config` atau dashboard.

Sumber data:
  - kunci API, status, Account ID Cloudflare, URL proxy  → dari file txt
  - katalog model per koneksi, combo Auto, agent config, settings
    → dari DB live (hasil validasi nyata 57/61 key)
"""
import json, re, sqlite3, sys
from datetime import datetime, timezone

TXT = "/home/user/uploads/backup_clean-1.txt"
DB = "/home/user/lollm/live/data/lollm.db"
OUT = "/home/user/backup_clean-1.json"

# ---------- 1. parse txt ----------
text = open(TXT).read()
lines = text.splitlines()

pools_txt = []          # [(name, url)]
prov = None
pools = []              # proxy pool entries
conns_txt = []          # {"id","provider","status","key","account"}
cur = {}

i = 0
while i < len(lines):
    l = lines[i]
    m = re.match(r"^(\d+)\.\s+(\S+)$", l.strip()) if i > 4 and l.startswith(("1","2","3","4","5","6","7","8","9")) and "." in l[:4] else None
    if m and i < 78:  # seksi proxy pools
        # cari URL di blok berikutnya
        for j in range(i+1, min(i+8, len(lines))):
            mu = re.match(r"^\s*URL\s*:\s*(\S+)", lines[j])
            if mu:
                pools.append({"name": m.group(2), "proxies": []})
                pools[-1]["proxies"].append(mu.group(1))
                break
    mp = re.match(r"^#### PROVIDER:\s+(\S+)", l)
    if mp:
        prov = mp.group(1).lower()
    mk = re.match(r"^\s*\[([A-Z0-9]+)\]\s+Status:\s+(\S+)", l)
    if mk:
        cur = {"id": mk.group(1), "provider": prov, "status": mk.group(2)}
    ma = re.match(r"^\s*API Key\s*:\s*(\S+)", l)
    if ma and cur:
        cur["key"] = ma.group(1)
    mr = re.match(r"^\s*Account ID\s*:\s*(\S+)", l)
    if mr and cur:
        cur["account"] = mr.group(1)
        conns_txt.append(cur); cur = {}
    elif ma and cur and prov != "cloudflare-ai":
        conns_txt.append(cur); cur = {}
    i += 1

# ---------- 2. metadata dari DB live ----------
dbc = sqlite3.connect(DB)
meta = {}
for name, provider, is_active, base_url, models_json, prio, weight in dbc.execute(
        "SELECT name, provider, is_active, base_url, models_json, priority, weight FROM connections"):
    meta[name] = {"provider": provider, "is_active": bool(is_active),
                  "base_url": base_url or "", "models": json.loads(models_json or "[]"),
                  "priority": prio, "weight": weight}

PREFIX = {"cloudflare-ai": "cloudflare-ai-", "gemini": "gemini-", "groq": "groq-",
          "mistral": "mistral-", "ollama": "ollama-", "openrouter": "openrouter-",
          "poolside": "poolside-"}
NAME_OF = {"cloudflare-ai": lambda i: f"cloudflare-ai-{i.lower()}",
           "gemini": lambda i: f"gemini-g{i[1:].lower()}",
           "groq": lambda i: f"groq-g{i[1:].lower()}",
           "mistral": lambda i: f"mistral-m{i[1:].lower()}",
           "ollama": lambda i: f"ollama-{i.lower()}",
           "openrouter": lambda i: f"openrouter-{i.lower()}",
           "poolside": lambda i: f"poolside-{i.lower()}"}

# ---------- 3. bangun backup ----------
connections = []
for c in conns_txt:
    name = NAME_OF[c["provider"]](c["id"])
    m = meta.get(name)
    if not m:
        print(f"  ! {name} tidak ada di DB live — pakai default", file=sys.stderr)
        m = {"is_active": True, "base_url": "", "models": [], "priority": 100, "weight": 1}
    base = m["base_url"]
    if c["provider"] == "cloudflare-ai" and c.get("account"):
        base = f"https://api.cloudflare.com/client/v4/accounts/{c['account']}/ai/v1"
    connections.append({
        "name": name, "provider": c["provider"], "api_key": c["key"],
        "base_url": base, "priority": m["priority"] or 100, "weight": m["weight"] or 1,
        "is_active": m["is_active"], "models": m["models"],
    })

# combo Auto (resolve by name)
combos = []
for name, strategy, compression, agent_on, models_json in dbc.execute(
        "SELECT name, strategy, compression, agent_mode_enabled, models_json FROM combos"):
    id2name = {r[0]: r[0] for r in dbc.execute("SELECT name FROM connections")}
    entries = []
    for e in json.loads(models_json):
        row = dbc.execute("SELECT name FROM connections WHERE id=?", (e["connection_id"],)).fetchone()
        entries.append({"connection": row[0] if row else "", "model": e["model"], "priority": e["priority"]})
    combos.append({"name": name, "strategy": strategy, "compression": compression or "",
                   "agent_mode_enabled": bool(agent_on), "models": entries})

agent_configs = []
for name, mode, rounds, hide, roles_json in dbc.execute(
        "SELECT name, mode, max_rounds, hide_internal_steps, roles_json FROM agent_configs"):
    agent_configs.append({"name": name, "mode": mode, "max_rounds": rounds,
                          "hide_internal_steps": bool(hide), "roles": json.loads(roles_json)})

settings = {k: v for k, v in dbc.execute("SELECT key, value FROM settings")
            if k != "dashboard.admin_token_hash"}  # token milik instalasi, jangan ikut

# proxy pool: gabung semua entri vercel-relay menjadi satu pool
pool_proxies = [p["proxies"][0] for p in pools if p["proxies"]]
proxy_pools = [{"name": "vercel-relay", "proxies": pool_proxies}] if pool_proxies else []

backup = {
    "format": "lollm-backup", "version": 1, "product": "LoLLM Synapse",
    "exported_at": datetime.now(timezone.utc).isoformat(),
    "secrets": "plain",
    "connections": connections,
    "proxy_pools": proxy_pools,
    "combos": combos,
    "agent_configs": agent_configs,
    "api_keys": [],   # key internal dibuat baru setelah restore (lollm key generate)
    "settings": settings,
}

json.dump(backup, open(OUT, "w"), indent=2)

# ---------- 4. laporan ----------
from collections import Counter
cnt = Counter(c["provider"] for c in connections)
print(f"✓ {OUT}")
print(f"  connections : {len(connections)} → " + ", ".join(f"{k}:{v}" for k, v in sorted(cnt.items())))
print(f"  aktif       : {sum(1 for c in connections if c['is_active'])} / {len(connections)}")
print(f"  combo       : {[c['name'] for c in combos]} ({len(combos[0]['models']) if combos else 0} model)")
print(f"  agent config: {[a['name'] for a in agent_configs]}")
print(f"  proxy pool  : {len(proxy_pools)} pool, {len(pool_proxies)} proxy")
print(f"  settings    : {settings}")
missing = [c["name"] for c in connections if not c.get("api_key")]
nokey = [c["name"] for c in connections if not c.get("api_key")]
print(f"  key kosong  : {nokey or 'tidak ada'}")
cf_ok = all(("accounts/" in c["base_url"]) for c in connections if c["provider"] == "cloudflare-ai")
print(f"  CF base_url : {'semua ber-account-id ✓' if cf_ok else 'ADA YANG TANPA ACCOUNT ID!'}")

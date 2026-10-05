#!/usr/bin/env bash
# ============================================================================
#  LoLLM Synapse — installer otomatis (curl+bash / wget+bash)
#
#  Instalasi satu baris (repo publik):
#    curl -fsSL https://github.com/NeoLnkSys/lollm/raw/main/scripts/installer.sh | bash
#    wget -qO- https://github.com/NeoLnkSys/lollm/raw/main/scripts/installer.sh | bash
#
#  Repo privat: tambahkan env token (fine-grained PAT, scope contents:read):
#    LOLM_TOKEN=ghp_xxx curl -fsSL .../installer.sh | bash
#
#  Opsi:
#    --version v0.1.0   versi spesifik (default: latest release)
#    --dest DIR         direktori instalasi (default: smart — lihat bawah)
#    --uninstall        hapus binary lollm
#    --no-verify        lewati verifikasi checksum
#  Env: LOLM_REPO, LOLM_VERSION, LOLM_DEST, LOLM_TOKEN, LOLM_NO_VERIFY=1
#
#  Pintar: deteksi OS/arch, curl ATAU wget, pilih dir otomatis
#  (root → /usr/local/bin; user → ~/.local/bin + auto-PATH),
#  verifikasi sha256, uji binary setelah instalasi.
# ============================================================================
set -euo pipefail

REPO="${LOLM_REPO:-NeoLnkSys/lollm}"
VERSION="${LOLM_VERSION:-latest}"
DEST=""
UNINSTALL=0
VERIFY=1
[ "${LOLM_NO_VERIFY:-0}" = "1" ] && VERIFY=0

# ---------- warna ----------
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  C_G="\033[32m"; C_Y="\033[33m"; C_R="\033[31m"; C_B="\033[1m"; C_D="\033[2m"; C_0="\033[0m"
else
  C_G=""; C_Y=""; C_R=""; C_B=""; C_D=""; C_0=""
fi
info()  { printf "${C_B}::${C_0} %s\n" "$*"; }
ok()    { printf "${C_G}✓${C_0} %s\n" "$*"; }
warn()  { printf "${C_Y}!${C_0} %s\n" "$*"; }
die()   { printf "${C_R}✗${C_0} %s\n" "$*" >&2; exit 1; }

# ---------- argumen ----------
while [ $# -gt 0 ]; do
  case "$1" in
    --version)   VERSION="${2:?}"; shift 2 ;;
    --dest)      DEST="${2:?}"; shift 2 ;;
    --uninstall) UNINSTALL=1; shift ;;
    --no-verify) VERIFY=0; shift ;;
    -h|--help)   sed -n '2,30p' "$0" 2>/dev/null || grep '^#' "$0" | head -25; exit 0 ;;
    *) die "argumen tidak dikenal: $1 (lihat --help)" ;;
  esac
done

# ---------- deteksi OS & arch ----------
OS="$(uname -s)"
ARCH="$(uname -m)"
case "$OS" in
  Linux)          OS="linux" ;;
  Darwin)         OS="darwin" ;;
  MINGW*|MSYS*|CYGWIN*)
    warn "Windows terdeteksi — unduh manual dari https://github.com/$REPO/releases (asset *-windows-amd64.tar.gz)"
    exit 3 ;;
  *) die "OS tidak didukung: $OS" ;;
esac
case "$ARCH" in
  x86_64|amd64)   ARCH="amd64" ;;
  aarch64|arm64)  ARCH="arm64" ;;
  *) die "arsitektur tidak didukung: $ARCH (asset tersedia: amd64, arm64)" ;;
esac
ok "platform: $OS/$ARCH"

# ---------- uninstall ----------
if [ "$UNINSTALL" = "1" ]; then
  BIN="$(command -v lollm || true)"
  [ -z "$BIN" ] && [ ! -x "$HOME/.local/bin/lollm" ] && die "lollm tidak ditemukan"
  BIN="${BIN:-$HOME/.local/bin/lollm}"
  rm -f "$BIN" && ok "dihapus: $BIN" || die "gagal menghapus (butuh sudo?)"
  exit 0
fi

# ---------- tooling: curl ATAU wget ----------
FETCH=""
if command -v curl >/dev/null 2>&1; then FETCH="curl"
elif command -v wget >/dev/null 2>&1; then FETCH="wget"
else die "butuh curl atau wget — install salah satu dulu"; fi
ok "downloader: $FETCH"
command -v tar >/dev/null 2>&1 || die "tar tidak ditemukan"

# fetch <url> [outfile] [accept] — curl atau wget; token dipakai bila ada
fetch() {
  local url="$1" out="${2:-}" accept="${3:-}"
  if [ "$FETCH" = "curl" ]; then
    local args=(-fsSL)
    [ -n "$accept" ] && args+=(-H "Accept: $accept")
    [ -n "${LOLM_TOKEN:-}" ] && args+=(-H "Authorization: Bearer $LOLM_TOKEN")
    if [ -n "$out" ]; then curl "${args[@]}" -o "$out" "$url"
    else curl "${args[@]}" "$url"; fi
  else
    local args=(-q)
    [ -n "$accept" ] && args+=(--header="Accept: $accept")
    [ -n "${LOLM_TOKEN:-}" ] && args+=(--header="Authorization: Bearer $LOLM_TOKEN")
    if [ -n "$out" ]; then wget "${args[@]}" -O "$out" "$url"
    else wget "${args[@]}" -O- "$url"; fi
  fi
}

# ---------- versi ----------
REL_JSON=""
if [ "$VERSION" = "latest" ]; then
  info "mencari versi terbaru…"
  REL_JSON="$(fetch "https://api.github.com/repos/$REPO/releases/latest" || true)"
else
  REL_JSON="$(fetch "https://api.github.com/repos/$REPO/releases/tags/$VERSION" || true)"
fi
if [ -z "$REL_JSON" ] || [ "$(printf '%s' "$REL_JSON" | head -c1)" != "{" ]; then
  die "gagal mengambil info release (repo privat? set LOLM_TOKEN)"
fi
if [ "$VERSION" = "latest" ]; then
  VERSION="$(printf '%s' "$REL_JSON" | grep -o '"tag_name": *"[^"]*"' | head -1 | sed 's/^.*"tag_name": *"//;s/"$//')"
  [ -n "$VERSION" ] || die "tag_name tidak ditemukan dari API"
fi
VER_NUM="${VERSION#v}"
ok "versi  : $VERSION"

# asset_id <json> <nama-asset> → id asset di API (untuk repo privat)
asset_id() {
  printf '%s' "$1" | tr '{},' '\n' | grep -E '"(url|name)":' | awk -v want="\"$2\"" '
    /releases\/assets\// { line=$0 }
    $0 ~ "\"name\": "want { print line; exit }
  ' | grep -o 'assets/[0-9][0-9]*' | tail -1 | cut -d/ -f2
}

# download_asset <nama-asset> <outfile> — URL publik dulu, fallback API asset
download_asset() {
  local name="$1" out="$2"
  local url="https://github.com/$REPO/releases/download/$VERSION/$name"
  fetch "$url" "$out" 2>/dev/null && return 0
  [ -n "${LOLM_TOKEN:-}" ] || return 1
  local aid
  aid="$(asset_id "$REL_JSON" "$name")"
  [ -n "$aid" ] || return 1
  fetch "https://api.github.com/repos/$REPO/releases/assets/$aid" "$out" "application/octet-stream"
}

# ---------- pilih direktori instalasi ----------
if [ -z "$DEST" ]; then
  if [ "$(id -u)" = "0" ]; then
    DEST="/usr/local/bin"
  else
    DEST="$HOME/.local/bin"
  fi
fi
mkdir -p "$DEST" 2>/dev/null || die "tidak bisa membuat $DEST (pakai --dest DIR lain)"
[ -w "$DEST" ] || die "$DEST tidak writable (coba --dest \$HOME/.local/bin)"

# ---------- unduh ----------
ASSET="lollm-$VER_NUM-$OS-$ARCH.tar.gz"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
info "mengunduh $ASSET…"
download_asset "$ASSET" "$TMP/$ASSET" || die "download gagal (cek koneksi / token bila repo privat)"
ok "terunduh: $(du -h "$TMP/$ASSET" | cut -f1)"

# ---------- verifikasi checksum ----------
if [ "$VERIFY" = "1" ]; then
  if download_asset "checksums.txt" "$TMP/checksums.txt" 2>/dev/null \
     && [ -s "$TMP/checksums.txt" ]; then
    SHACMD=""
    if command -v sha256sum >/dev/null 2>&1; then SHACMD="sha256sum"
    elif command -v shasum >/dev/null 2>&1; then SHACMD="shasum -a 256"; fi
    if [ -n "$SHACMD" ]; then
      WANT="$(awk -v a="$ASSET" '$2==a {print $1}' "$TMP/checksums.txt")"
      if [ -n "$WANT" ]; then
        GOT="$(cd "$TMP" && $SHACMD "$ASSET" | awk '{print $1}')"
        [ "$GOT" = "$WANT" ] && ok "checksum sha256 cocok" \
          || die "CHECKSUM TIDAK COCOK — file korup/salah, instalasi dibatalkan"
      else
        warn "$ASSET tidak tercantum di checksums.txt — skip verifikasi"
      fi
    else
      warn "tool sha256 tidak ditemukan — skip verifikasi"
    fi
  else
    warn "checksums.txt tidak tersedia di release — skip verifikasi"
  fi
fi

# ---------- ekstrak & pasang ----------
tar -xzf "$TMP/$ASSET" -C "$TMP"
[ -x "$TMP/lollm" ] || die "binary lollm tidak ditemukan di dalam tarball"
install -m 0755 "$TMP/lollm" "$DEST/lollm" 2>/dev/null || cp "$TMP/lollm" "$DEST/lollm"
ok "terpasang : $DEST/lollm"

# ---------- uji binary ----------
INSTALLED_VERSION="$("$DEST/lollm" version 2>/dev/null | head -1 || true)"
[ -n "$INSTALLED_VERSION" ] || die "binary tidak bisa dijalankan di sistem ini"
ok "berjalan  : $INSTALLED_VERSION"

# ---------- PATH otomatis ----------
case ":$PATH:" in
  *":$DEST:"*) ;;
  *)
    if [ "${DEST#"$HOME"}" != "$DEST" ]; then  # hanya untuk path di bawah $HOME
      RC=""
      case "${SHELL:-}" in
        *zsh*) RC="$HOME/.zshrc" ;;
        *)     RC="$HOME/.bashrc" ;;
      esac
      [ -f "$RC" ] || RC="$HOME/.profile"
      if [ -f "$RC" ] || touch "$RC" 2>/dev/null; then
        grep -q '>>> lollm >>>' "$RC" 2>/dev/null || printf '\n# >>> lollm >>>\nexport PATH="%s:$PATH"\n# <<< lollm <<<\n' "$DEST" >> "$RC"
        warn "$DEST belum di PATH — baris sudah ditambahkan ke $RC"
        warn "jalankan: export PATH=\"$DEST:\$PATH\"  (atau buka shell baru)"
      else
        warn "$DEST belum di PATH — tambahkan manual: export PATH=\"$DEST:\$PATH\""
      fi
    else
      warn "$DEST belum di PATH — tambahkan manual bila perlu"
    fi
    ;;
esac

printf '\n'
ok "LoLLM Synapse siap! Mulai:  ${C_B}lollm serve${C_0}  (API :20999 · dashboard :21000)"
printf "   cek: lollm version · lollm setup · lollm doctor\n"
printf "   docs: https://github.com/%s\n" "$REPO"

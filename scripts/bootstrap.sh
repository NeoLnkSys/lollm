#!/usr/bin/env bash
# Install the Go toolchain if missing (sandbox/CI convenience).
# Usage: bash scripts/bootstrap.sh
set -uo pipefail

if command -v go >/dev/null 2>&1; then
  go version
  exit 0
fi

echo "Go not found — installing (user-local, no root required)..." >&2
if command -v curl >/dev/null 2>&1; then
  DEST="${LOLLM_GO_DEST:-/tmp/go-toolchain}"
  VER="${LOLLM_GO_VERSION:-$(curl -fsSL 'https://go.dev/dl/?mode=json' \
    | grep -o '"version": *"go[0-9.]*"' | head -1 | cut -d'"' -f4)}"
  echo "Downloading ${VER} -> ${DEST}" >&2
  curl -fsSL "https://go.dev/dl/${VER}.linux-amd64.tar.gz" -o /tmp/go.tgz
  rm -rf "${DEST}"
  mkdir -p "${DEST}"
  tar -C "${DEST}" -xzf /tmp/go.tgz
  rm -f /tmp/go.tgz
  export PATH="${DEST}/go/bin:${PATH}"
  echo "Go installed at ${DEST}/go — add it to PATH or re-run:" \
       "export PATH=\"${DEST}/go/bin:\$PATH\"" >&2
elif command -v apt-get >/dev/null 2>&1 && [ "$(id -u)" = "0" ]; then
  export DEBIAN_FRONTEND=noninteractive
  apt-get install -y -qq golang-go \
    || { apt-get update -qq && apt-get install -y -qq golang-go; }
else
  echo "No curl (and no root apt) — install Go >= 1.24 manually: https://go.dev/dl/" >&2
  exit 1
fi
go version

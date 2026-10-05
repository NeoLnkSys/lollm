#!/usr/bin/env bash
# LoLLM Synapse — push ke GitHub + buat Release v0.1.0 dengan 4 artefak.
# Jalankan di MESIN ANDA (butuh: git + gh CLI ter-login: `gh auth login`).
#   bash scripts/github_release.sh <username> [repo-name]
set -euo pipefail

USERNAME="${1:?usage: github_release.sh <username> [repo-name]}"
REPO="${2:-lollm}"
cd "$(dirname "$0")/.."

echo "==> Repo lokal: $(git log --oneline -1)"
echo "==> Tag:        $(git tag)"

# 1) Buat repo private baru (ganti --private → --public bila mau publik)
gh repo create "$USERNAME/$REPO" --private --source=. --remote=origin --push 2>/dev/null \
  || { echo "repo mungkin sudah ada — push ke origin"; git remote add origin "git@github.com:$USERNAME/$REPO.git" 2>/dev/null || true; git push -u origin main; }

# 2) Push tag
git push origin v0.1.0

# 3) Release dengan artefak
gh release create v0.1.0 \
  --title "LoLLM Synapse v0.1.0" \
  --notes-file CHANGELOG.md \
  release/lollm-0.1.0-*.tar.gz

echo "✓ Selesai: https://github.com/$USERNAME/$REPO/releases/tag/v0.1.0"

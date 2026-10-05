#!/usr/bin/env bash
# Sandbox/CI-friendly Go environment.
# Keeps module & build caches OUT of the workspace (~128MB snapshot cap) and
# OUT of the small tmpfs /tmp (which fills up) — they live on the root
# filesystem at /var/tmp instead. Override with LOLLM_GOPATH / LOLLM_GOCACHE.
# Usage: source scripts/env.sh

export GOPATH="${LOLLM_GOPATH:-/var/tmp/gopath}"
export GOCACHE="${LOLLM_GOCACHE:-/var/tmp/gocache}"
export GOMODCACHE="${LOLLM_GOMODCACHE:-$GOPATH/pkg/mod}"
export CGO_ENABLED="${CGO_ENABLED:-0}"
export GOFLAGS="${GOFLAGS:--mod=mod}"
# Pin to the local toolchain; avoids surprise toolchain downloads.
export GOTOOLCHAIN="${GOTOOLCHAIN:-local}"

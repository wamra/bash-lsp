#!/usr/bin/env bash
# Stage the installable plugin tree, with the six binaries inside, into OUT.
# Usage: scripts/build-plugin.sh OUT [VERSION]
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${1:?usage: build-plugin.sh OUT [VERSION]}"
VERSION="${2:-$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)}"
P="$ROOT/plugin"
rm -rf "$OUT"; mkdir -p "$OUT/.claude-plugin" "$OUT/bin"
for t in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
  os="${t%/*}"; arch="${t#*/}"; ext=""; [ "$os" = windows ] && ext=".exe"
  ( cd "$ROOT" && CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$OUT/bin/bash-lsp-$os-$arch$ext" ./cmd/bash-lsp )
done
cp "$ROOT/scripts/plugin-launcher.sh" "$OUT/bin/bash-lsp"
cp "$P/.claude-plugin/plugin.json" "$OUT/.claude-plugin/"
cp "$P/.lsp.json" "$OUT/"
cp "$ROOT/LICENSE" "$ROOT/NOTICE" "$P/README.md" "$OUT/"
chmod +x "$OUT"/bin/*

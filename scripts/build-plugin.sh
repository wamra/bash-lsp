#!/usr/bin/env bash
# Stage the installable plugin tree, with the six binaries inside, into OUT.
# Usage: scripts/build-plugin.sh OUT [VERSION]
# VERSION defaults to the content of the VERSION file.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${1:?usage: build-plugin.sh OUT [VERSION]}"
VERSION="${2:-}"
if [ -z "$VERSION" ]; then
  [ -f "$ROOT/VERSION" ] || { echo "build-plugin.sh: no VERSION argument and no VERSION file" >&2; exit 2; }
  VERSION="$(tr -d '[:space:]' < "$ROOT/VERSION")"
fi
case "$VERSION" in
  '' | *[!0-9A-Za-z.+-]* | -* ) echo "build-plugin.sh: bad version: $VERSION" >&2; exit 2 ;;
esac
P="$ROOT/plugin"

# OUT is deleted and rebuilt, so refuse anything that is not a previous
# staging: /, ., .., $HOME, the repo root, the plugin source tree (which holds
# .claude-plugin/ itself), or an existing non-empty directory without
# .claude-plugin/ (and any existing non-directory).
refuse() { echo "build-plugin.sh: refusing to replace $OUT: $1" >&2; exit 2; }
physical() { (cd "$1" 2>/dev/null && pwd -P); }
case "$OUT" in
  / | . | .. | */. | */..) refuse "not a staging directory" ;;
esac
if [ -e "$OUT" ] || [ -L "$OUT" ]; then
  [ -d "$OUT" ] && [ ! -L "$OUT" ] || refuse "not a directory"
  real="$(physical "$OUT")"
  for p in / "$(physical "$HOME")" "$(physical "$ROOT")" "$(pwd -P)" "$(physical ..)"; do
    [ "$real" != "$p" ] || refuse "it is $p"
  done
  src="$(physical "$P")"
  case "$real/" in "$src"/*) refuse "it is the plugin source tree $P" ;; esac
  if [ -n "$(ls -A "$OUT")" ] && [ ! -d "$OUT/.claude-plugin" ]; then
    refuse "a non-empty directory that is not a previous staging (no .claude-plugin/)"
  fi
fi
rm -rf "$OUT"; mkdir -p "$OUT/.claude-plugin" "$OUT/bin"
for t in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
  os="${t%/*}"; arch="${t#*/}"; ext=""; [ "$os" = windows ] && ext=".exe"
  ( cd "$ROOT" && CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -buildvcs=false -ldflags "-s -w -X main.version=$VERSION" -o "$OUT/bin/bash-lsp-$os-$arch$ext" ./cmd/bash-lsp )
done
cp "$ROOT/scripts/plugin-launcher.sh" "$OUT/bin/bash-lsp"
cp "$P/.claude-plugin/plugin.json" "$OUT/.claude-plugin/"
cp "$P/.lsp.json" "$OUT/"
cp -R "$P/hooks" "$OUT/hooks"
cp "$ROOT/LICENSE" "$ROOT/THIRD_PARTY_NOTICES.md" "$P/README.md" "$OUT/"
chmod +x "$OUT"/bin/*

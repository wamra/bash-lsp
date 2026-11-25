#!/usr/bin/env bash
# Cross-build the six release archives with plain Go tooling.
# Each archive holds the binary, LICENSE, NOTICE and README.md.
#
# Usage: scripts/build-release.sh [VERSION] [OUT]
#
# VERSION defaults to the content of the VERSION file ("" counts as absent).
# OUT defaults to dist/. The script refuses a tree with uncommitted or
# untracked files, builds into OUT only, and writes two lists there:
#   checksums.txt  sha256 of each archive
#   binaries.txt   sha256 of each binary, named as inside its archive
# Archive bytes carry modification times, so two builds differ in
# checksums.txt. The binaries are built with -trimpath, -buildvcs=false (no
# commit is stamped, so a later commit with the same Go sources gives the same
# binary) and the version stamped, and come out identical: compare
# binaries.txt.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"

VERSION="${1:-}"
if [ -z "$VERSION" ]; then
  [ -f "$ROOT/VERSION" ] || { echo "build-release.sh: no VERSION argument and no VERSION file" >&2; exit 2; }
  VERSION="$(tr -d '[:space:]' < "$ROOT/VERSION")"
fi
case "$VERSION" in
  '' | *[!0-9A-Za-z.+-]* | -* ) echo "build-release.sh: bad version: $VERSION" >&2; exit 2 ;;
esac

OUT="${2:-$ROOT/dist}"

refuse() { echo "build-release.sh: $1" >&2; exit 2; }
physical() { (cd "$1" 2>/dev/null && pwd -P); }

git -C "$ROOT" rev-parse --git-dir >/dev/null 2>&1 || refuse "not a git checkout: cannot tell whether the tree is clean"
if [ -n "$(git -C "$ROOT" status --porcelain)" ]; then
  refuse "the tree has uncommitted or untracked files; commit or remove them first"
fi

# OUT is deleted and rebuilt, so refuse anything that is not a previous output.
case "$OUT" in
  / | . | .. | */. | */..) refuse "refusing to replace $OUT: not an output directory" ;;
esac
if [ -e "$OUT" ] || [ -L "$OUT" ]; then
  [ -d "$OUT" ] && [ ! -L "$OUT" ] || refuse "refusing to replace $OUT: not a directory"
  real="$(physical "$OUT")"
  for p in / "$(physical "$HOME")" "$ROOT" "$(pwd -P)" "$(physical "$(dirname "$ROOT")")"; do
    [ "$real" != "$p" ] || refuse "refusing to replace $OUT: it is $p"
  done
  case "$ROOT/" in "$real"/*) refuse "refusing to replace $OUT: it holds the repository" ;; esac
  if [ -n "$(ls -A "$OUT")" ] && [ ! -f "$OUT/checksums.txt" ]; then
    refuse "refusing to replace $OUT: a non-empty directory that is not a previous output (no checksums.txt)"
  fi
fi
rm -rf "$OUT"; mkdir -p "$OUT"
OUT="$(physical "$OUT")"

if command -v sha256sum >/dev/null 2>&1; then sum() { sha256sum "$@"; }; else sum() { shasum -a 256 "$@"; }; fi
export COPYFILE_DISABLE=1

: > "$OUT/binaries.txt"
for t in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
  os="${t%/*}"; arch="${t#*/}"
  name="bash-lsp_${VERSION}_${os}_${arch}"
  stage="$OUT/$name"; mkdir -p "$stage"
  exe=bash-lsp; [ "$os" = windows ] && exe=bash-lsp.exe
  ( cd "$ROOT" && CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -buildvcs=false -ldflags "-s -w -X main.version=$VERSION" -o "$stage/$exe" ./cmd/bash-lsp )
  ( cd "$OUT" && sum "$name/$exe" ) >> "$OUT/binaries.txt"
  cp "$ROOT/LICENSE" "$ROOT/NOTICE" "$ROOT/README.md" "$stage/"
  if [ "$os" = windows ]; then ( cd "$OUT" && zip -qr "$name.zip" "$name" ); else tar -C "$OUT" -czf "$OUT/$name.tar.gz" "$name"; fi
  rm -rf "$stage"
done
( cd "$OUT" && sum *.tar.gz *.zip > checksums.txt )
ls -l "$OUT"

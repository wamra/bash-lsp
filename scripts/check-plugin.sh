#!/usr/bin/env bash
# Check a plugin tree staged by scripts/build-plugin.sh. Usage: scripts/check-plugin.sh OUT
set -euo pipefail
OUT="${1:?usage: check-plugin.sh OUT}"
fail=0
need() { [ -s "$OUT/$1" ] || { echo "missing or empty: $1" >&2; fail=1; }; }
for f in LICENSE THIRD_PARTY_NOTICES.md README.md .claude-plugin/plugin.json .lsp.json hooks/hooks.json bin/bash-lsp \
  bin/bash-lsp-darwin-amd64 bin/bash-lsp-darwin-arm64 bin/bash-lsp-linux-amd64 bin/bash-lsp-linux-arm64 \
  bin/bash-lsp-windows-amd64.exe bin/bash-lsp-windows-arm64.exe; do
  need "$f"
done
# The notices must say what the binary contains.
if [ -s "$OUT/THIRD_PARTY_NOTICES.md" ] && ! grep -q 'mvdan.cc/sh/v3/syntax' "$OUT/THIRD_PARTY_NOTICES.md"; then
  echo "THIRD_PARTY_NOTICES.md does not name the vendored parser" >&2; fail=1
fi
[ -x "$OUT/bin/bash-lsp" ] || { echo "bin/bash-lsp is not executable" >&2; fail=1; }
[ "$fail" = 0 ] && echo "plugin tree ok: $OUT"
exit "$fail"

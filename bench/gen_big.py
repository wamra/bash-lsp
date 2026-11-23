"""Generate the large benchmark script: 2,000 functions, about 561 KB. Usage: gen_big.py OUT"""
import sys
out = ["#!/usr/bin/env bash", "echo start"]
for i in range(2000):
    out += [f"# Function number {i}.", f"fn_{i}() {{", '  local a="$1" b="$2"',
            f'  if [[ -n "$a" && "$b" != "x{i}" ]]; then', f'    printf \'%s %s\\n\' "$a" "$b" | tr a-z A-Z', '  else',
            f'    for f in "$@"; do echo "${{f:-none}}" >&2; done', '  fi',
            f'  case "$a" in start|stop) echo "$a";; *) return {i%250};; esac', "}", ""]
    if i:
        out.append(f"fn_{i-1} a b")
out.append("fn_1999 a b")
open(sys.argv[1], "w").write("\n".join(out) + "\n")

"""Generate a large benchmark script with arithmetic: the same shape as gen_big.py (functions of ten lines) plus
one arithmetic construct per function and a second in every even one, i.e. about one per 9 lines (about 2,400 constructs in 560 KB). That density is a stress choice, not a measured property of real scripts: it is meant to be at least as arithmetic-heavy as ordinary shell code, so the tokenizer's share of open and edit time is not understated.
The five forms rotate: (( ++ )), $(( )), C-style for, let, subscript and ${v:off:len}.
Usage: gen_arith.py OUT"""
import sys
forms = [
    lambda i: f"  (( cnt += {i} * n + 1, total++ ))",
    lambda i: f'  echo "$(( cnt + n * {i} ))" >&2',
    lambda i: "  for ((k=0; k<n; k++)); do :; done",
    lambda i: "  let cnt=cnt+n total--",
    lambda i: '  echo "${arr[n+1]} ${v:off:len-1}" >&2',
]
out = ["#!/usr/bin/env bash", "echo start"]
for i in range(1620):
    out += [f"# Function number {i}.", f"fn_{i}() {{", '  local a="$1" b="$2" n=3 cnt=0',
            f'  if [[ -n "$a" && "$b" != "x{i}" ]]; then', f'    printf \'%s %s\\n\' "$a" "$b" | tr a-z A-Z', '  else',
            f'    for f in "$@"; do echo "${{f:-none}}" >&2; done', '  fi',
            f'  case "$a" in start|stop) echo "$a";; *) return {i%250};; esac',
            forms[i % 5](i)] + ([forms[(i + 2) % 5](i)] if i % 2 == 0 else []) + ["}", ""]
    if i:
        out.append(f"fn_{i-1} a b")
out.append("fn_1619 a b")
open(sys.argv[1], "w").write("\n".join(out) + "\n")

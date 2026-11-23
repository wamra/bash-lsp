"""Generate the workspace scale test: N files of F functions each, chained by `source`.
File i sources file i-1; function i_k calls function (i-1)_k, so every call
crosses a file and the visibility closure of the last file is the whole chain.
Usage: gen_workspace.py OUTDIR [FILES=1000] [FUNCS=20]"""
import os, sys

out = sys.argv[1]
files = int(sys.argv[2]) if len(sys.argv) > 2 else 1000
funcs = int(sys.argv[3]) if len(sys.argv) > 3 else 20
os.makedirs(out, exist_ok=True)
for i in range(files):
    lines = ["#!/usr/bin/env bash"]
    if i:
        lines.append(f"source ./s{i-1:04d}.sh")
    for k in range(funcs):
        lines += [f"# Function {i}_{k}.", f"f{i}_{k}() {{", '  local a="$1"']
        if i:
            lines.append(f'  f{i-1}_{k} "$a"')
        lines += ['  echo "$a"', "}", ""]
    open(os.path.join(out, f"s{i:04d}.sh"), "w").write("\n".join(lines) + "\n")

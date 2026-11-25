"""Run one fixed, harmless snippet per feature key of the bash version table
under a given bash, and record what happened.

Usage: uv run --no-project python bench/version_probe.py BASH [OUT.json]

Every snippet is a constant in this file (no repository text, nothing read
from the analysed corpus). Each runs as `env -i PATH=/usr/bin:/bin BASH --noprofile --norc ...`. A syntactic feature runs with `-n -c SNIPPET`
(parse only, nothing executes); the others under `BASH -c SNIPPET` with
a command that needs nothing but the shell (`sleep 0`, `/dev/null`). A snippet
"fails" when bash exits non-zero. Expected on 3.2.57: every 4.0+ feature
fails. The `control` rows are forms bash 3.2 has: they must pass, and a
failing control means the probe itself is broken.

OUT defaults to bench/results/version-probe-m3.json. The file holds, per
key: the snippet, how it ran, the exit status, the first stderr line and
the verdict, with the bash path, its `$BASH_VERSION` and the date.
"""
import datetime
import json
import os
import re
import subprocess
import sys

# key, first version FDS-016 names ("" for a control), parse-only?, snippet
PROBES = [
    ("assoc-array", "4.0", False, "declare -A m"),
    ("mapfile", "4.0 (FDS-016: one conflicting source; not in the table)", False, "mapfile -t a </dev/null"),
    ("case-modification", "4.0", False, 'x=ab; echo "${x^^}" >/dev/null'),
    ("coproc", "4.0", True, "coproc { :; }"),
    ("globstar", "4.0", False, "shopt -s globstar"),
    ("case-fallthrough", "4.0", True, "case x in x) :;& y) :;; esac"),
    ("case-resume", "4.0", True, "case x in x) :;;& y) :;; esac"),
    ("pipe-all", "4.0", True, "true |& cat"),
    ("append-all", "4.0", True, "true &>> /dev/null"),
    ("declare-g", "4.2", False, "declare -g x=1"),
    ("lastpipe", "4.2", False, "shopt -s lastpipe"),
    ("nameref", "4.3", False, "declare -n r=x"),
    ("wait-n", "4.3", False, ": & wait -n"),
    ("negative-subscript", "4.3", False, 'a=(1 2); [ "${a[-1]}" = 2 ]'),
    ("transform-op", "4.4", False, 'x=a; echo "${x@Q}" >/dev/null'),
    ("epochseconds", "5.0", False, '[ -n "${EPOCHSECONDS-}" ]'),
    ("wait-p", "5.1", False, ": & wait -p v"),
    # Forms bash 3.2 has (WB1302 evidence, no version).
    ("control:double-bracket", "", True, "[[ -n x ]]"),
    ("control:function-keyword", "", True, "function f { :; }"),
    ("control:array-assign", "", True, "a=(1 2)"),
    ("control:ansi-c-quote", "", True, "echo $'x' >/dev/null"),
    ("control:herestring", "", True, "cat <<< hi >/dev/null"),
    ("control:redirect-all", "", True, "true &> /dev/null"),
    ("control:process-subst", "", True, "cat <(true) >/dev/null"),
    ("control:source-builtin", "", True, "source /dev/null"),
    ("control:plain", "", False, "true"),
]


def series(version):
    m = re.match(r"(\d+)\.(\d+)", version)
    return (int(m.group(1)), int(m.group(2))) if m else (0, 0)


def minimum(first):
    m = re.match(r"(\d+)\.(\d+)$", first)
    return (int(m.group(1)), int(m.group(2))) if m else None


def run(bash, parse_only, snippet):
    argv = ["env", "-i", "PATH=/usr/bin:/bin", bash, "--noprofile", "--norc"] + \
        (["-n"] if parse_only else []) + ["-c", snippet]
    p = subprocess.run(argv, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                       stderr=subprocess.PIPE, text=True, timeout=10)
    err = p.stderr.strip().splitlines()
    return argv, p.returncode, err[0] if err else ""


def main():
    if len(sys.argv) < 2:
        sys.exit(__doc__)
    bash = sys.argv[1]
    out = sys.argv[2] if len(sys.argv) > 2 else os.path.join(
        os.path.dirname(os.path.abspath(__file__)), "results", "version-probe-m3.json")
    version = subprocess.run([bash, "-c", "echo $BASH_VERSION"], stdin=subprocess.DEVNULL,
                             stdout=subprocess.PIPE, text=True, timeout=10).stdout.strip()
    rows, bad = [], []
    for key, first, parse_only, snippet in PROBES:
        argv, code, err = run(bash, parse_only, snippet)
        control = key.startswith("control:")
        fails = code != 0
        mn = minimum(first)
        if control:
            expect = "passes"
        elif mn is not None:
            expect = "fails" if series(version) < mn else "passes"
        else:
            expect = None  # not a table row: recorded, not judged
        verdict = ("control ok" if not fails else "CONTROL FAILED") if control else \
            ("fails" if fails else "passes")
        if expect and verdict.split()[0] != expect and not (control and not fails):
            bad.append(key)
        rows.append({"key": key, "first_version_fds016": first, "argv": argv[1:], "exit": code,
                     "stderr_first_line": err, "verdict": verdict, "expected": expect})
    rec = {"label": "bash version probe", "bash": bash, "BASH_VERSION": version,
           "date": datetime.datetime.now().astimezone().isoformat(timespec="seconds"),
           "expect": "a table row fails below its first version and passes from it; every control row passes",
           "unexpected": bad, "rows": rows}
    with open(out, "w") as f:
        json.dump(rec, f, indent=1)
        f.write("\n")
    for r in rows:
        print(f"{r['key']:28} {r['verdict']:12} exit={r['exit']}  {r['stderr_first_line'][:70]}")
    print(f"{version}: {len(rows)} probes, unexpected: {bad or 'none'} -> {out}")


main()

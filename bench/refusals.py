"""Corpus check for the nesting guard: no real shell file may be refused.
Collects shell files (by extension or shebang) under each ROOT, read-only, at
most --cap in total shared evenly between the roots (a root given with --all
is scanned whole and does not count toward the cap), skipping anything over
2 MiB; runs `BINARY ast FILE` on each and reports every file refused with
"nesting too deep to parse safely". Prints one JSON object.
Usage: uv run --no-project python refusals.py BINARY [--cap N] [--all ROOT]... ROOT..."""
import concurrent.futures, datetime, json, os, re, subprocess, sys

args = sys.argv[1:]
cap = 5000
if "--cap" in args:
    i = args.index("--cap"); cap = int(args[i + 1]); del args[i:i + 2]
whole = []
while "--all" in args:
    i = args.index("--all"); whole.append(args[i + 1]); del args[i:i + 2]
binary, roots = args[0], args[1:]
MAX = 2 << 20
EXT = (".sh", ".bash", ".bats", ".ksh")
SHEBANG = re.compile(rb"^#![^\n]*\b(sh|bash|dash|ksh|mksh|bats)\b")
REFUSAL = "nesting too deep to parse safely"

def shell_files(root, limit):
    found = []
    for d, dirs, files in os.walk(root):
        dirs[:] = sorted(x for x in dirs if x != ".git")
        for name in sorted(files):
            p = os.path.join(d, name)
            try:
                if os.path.islink(p) or not os.path.isfile(p) or os.path.getsize(p) > MAX:
                    continue
                if not name.endswith(EXT):
                    with open(p, "rb") as f:
                        if not SHEBANG.match(f.read(128)):
                            continue
            except OSError:
                continue
            found.append(p)
            if len(found) >= limit:
                return found
    return found

def check(p):
    r = subprocess.run([binary, "ast", p], stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True, errors="replace")
    if r.returncode == 0:
        return "parsed"
    if REFUSAL in r.stderr:
        return "refused"
    if r.returncode < 0 or "goroutine" in r.stderr:
        return "crashed"
    return "skipped-other"  # zsh, unreadable

per_root, refused, crashed, totals = {}, [], [], {"parsed": 0, "refused": 0, "crashed": 0, "skipped-other": 0}
for root in whole + roots:
    files = shell_files(root, 1 << 30 if root in whole else cap // len(roots))
    with concurrent.futures.ThreadPoolExecutor(8) as ex:
        results = list(ex.map(check, files))
    counts = {k: results.count(k) for k in totals}
    per_root[root] = {"scanned": len(files), **counts}
    for k in totals:
        totals[k] += counts[k]
    refused += [p for p, r in zip(files, results) if r == "refused"]
    crashed += [p for p, r in zip(files, results) if r == "crashed"]
repo = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
commit = subprocess.check_output(["git", "rev-parse", "--short", "HEAD"], cwd=repo, text=True).strip()
print(json.dumps({"date": datetime.datetime.now().astimezone().isoformat(timespec="seconds"), "commit": commit,
                  "cap": cap, "max_bytes": MAX, "scanned": sum(v["scanned"] for v in per_root.values()),
                  "refused": totals["refused"], "crashed": totals["crashed"], "parsed": totals["parsed"],
                  "skipped_other": totals["skipped-other"], "roots": per_root,
                  "refused_paths": refused, "crashed_paths": crashed}, indent=1))

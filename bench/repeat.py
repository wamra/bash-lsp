"""Run a measurement command N times and keep every raw sample.
Usage: uv run --no-project python repeat.py OUT.json LABEL N [--wall] [--env K=V ...] -- CMD...
Each run of CMD prints one JSON object on stdout (stored as is); with --wall the
wall time of the command is stored instead and stdout is discarded. OUT.json
holds {label, date, commit, machine, binary, cmd, n, runs:[...]} so every number
quoted in a document can be recomputed from the file (summarize.py)."""
import datetime, json, os, platform, subprocess, sys, time

out, label, n = sys.argv[1], sys.argv[2], int(sys.argv[3])
rest = sys.argv[4:]
cmd = rest[rest.index("--") + 1:]
opts = rest[:rest.index("--")]
wall = "--wall" in opts
env = dict(os.environ)
for i, o in enumerate(opts):
    if o == "--env":
        k, v = opts[i + 1].split("=", 1)
        env[k] = v
repo = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
def sh(*a):
    return subprocess.check_output(a, cwd=repo, text=True).strip()
runs = []
for _ in range(n):
    t = time.time()
    p = subprocess.run(cmd, env=env, stdout=subprocess.PIPE, text=True, check=True)
    ms = round((time.time() - t) * 1000, 1)
    runs.append({"wall_ms": ms} if wall else json.loads(p.stdout))
rec = {"label": label, "date": datetime.datetime.now().astimezone().isoformat(timespec="seconds"),
       "commit": sh("git", "rev-parse", "--short", "HEAD"), "dirty": bool(sh("git", "status", "--porcelain", "--", "cmd", "internal", "analysis", "tree")),
       "machine": f"workstation {sh('sysctl', '-n', 'machdep.cpu.brand_string')}, {platform.platform()}",
       "cmd": [os.path.basename(c) if c.startswith("/") and i == 0 else c for i, c in enumerate(cmd)], "n": n, "runs": runs}
json.dump(rec, open(out, "w"), indent=1)
print(f"{label}: {n} runs -> {out}")

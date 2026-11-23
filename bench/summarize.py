"""Print min / median / p95 / max for the samples in a bench/results/repeat-*.json file.
Usage: uv run --no-project python summarize.py FILE [KEY ...]   (KEY: a field of each run, scalar or list;
wall-time files use wall_ms)"""
import json, statistics, sys
d = json.load(open(sys.argv[1]))
keys = sys.argv[2:] or (["wall_ms"] if "wall_ms" in d["runs"][0] else [k for k, v in d["runs"][0].items() if isinstance(v, (int, float)) and not isinstance(v, bool)])
print(d["label"], d["commit"], d["date"], f"n={d['n']}")
for k in keys:
    xs = []
    for r in d["runs"]:
        v = r[k]
        xs += v if isinstance(v, list) else [v]
    xs.sort()
    p95 = xs[min(len(xs) - 1, int(len(xs) * 0.95))]
    print(f"  {k}: samples={len(xs)} min={xs[0]} median={statistics.median(xs)} p95={p95} max={xs[-1]} over50={sum(x > 50 for x in xs) / len(xs):.0%}")

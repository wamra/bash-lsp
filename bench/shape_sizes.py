"""Time chosen bind shapes at four sizes with the shape probe, best of N runs per size.
Usage: shape_sizes.py PROBE WORKDIR N SHAPE[:COL,COL...]...
PROBE is the binary built from bench/testdata/shapeprobe; WORKDIR receives the inputs, written by
gen_bind_shapes.py at 1/4, 1/2, 1 and 2 times its default size (512 KiB, 1 MiB, 2 MiB, 4 MiB, each less 64 bytes
scaled). For each SHAPE and each named column (default: every column the probe prints) one line:
  shape column t1 / t2 / t3 / t4 ms  ratios r12 r23 r34
with ri = t(i+1)/t(i) per doubling ("-" when the smaller time is under 1 ms or the larger under 20 ms).
Linear is x2.0 per doubling; a ratio that stays over about x2.5 at the larger sizes is super-linear."""
import os, subprocess, sys
probe, work, n = sys.argv[1], sys.argv[2], int(sys.argv[3])
specs = sys.argv[4:]
here = os.path.dirname(os.path.abspath(__file__))
M0 = 2 * 1024 * 1024 - 64
sizes = [M0 // 4, M0 // 2, M0, M0 * 2]
dirs = []
for s in sizes:
    d = os.path.join(work, str(s))
    if not os.path.isdir(d):
        os.makedirs(d)
        subprocess.run(["uv", "run", "--no-project", "python", os.path.join(here, "gen_bind_shapes.py"), d, str(s)], check=True,
                       stdout=subprocess.DEVNULL)
    dirs.append(d)
def run(path):
    best = {}
    for _ in range(n):
        out = subprocess.run([probe, path], check=True, capture_output=True, text=True).stdout.split()
        for kv in out:
            k, v = kv.split("=", 1)
            if k in ("bytes", "decls", "uses") or not v.isdigit():
                continue
            best[k] = min(best.get(k, 1 << 60), int(v))
    return best
print("# sizes (bytes): " + " / ".join(str(s) for s in sizes) + "; best of %d runs per size" % n)
for spec in specs:
    shape, _, want = spec.partition(":")
    res = [run(os.path.join(d, shape + ".sh")) for d in dirs]
    cols = want.split(",") if want else [k for k in res[0]]
    for c in cols:
        ts = [r.get(c) for r in res]
        rs = []
        for a, b in zip(ts, ts[1:]):
            rs.append("x%.1f" % (b / a) if a is not None and b is not None and a >= 1 and b >= 20 else "-")
        print("%-30s %-8s %s ms   %s" % (shape, c, " / ".join(str(t) for t in ts), " ".join(rs)), flush=True)

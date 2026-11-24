"""List the cells of the "after" table of a bind-shapes results file (bench/results/bind-shapes-*.txt)
that are at least MS milliseconds at the full size, or whose ratio full/half is at least RATIO.
Usage: shape_cells.py FILE [MS=60] [RATIO=2.5]
Prints one line per cell: shape, column, full/half ms, ratio (recomputed from full/half when half >= 1 ms
and full >= 20 ms, the file's own rule), and the flags that selected it; then the shapes with any cell at or
over RATIO."""
import re, sys
path = sys.argv[1]
ms = float(sys.argv[2]) if len(sys.argv) > 2 else 60
ratio = float(sys.argv[3]) if len(sys.argv) > 3 else 2.5
lines = open(path).read().split("\n")
start = next(i for i, l in enumerate(lines) if l.startswith("shape "))
cols = lines[start].split()[3:]
cell = re.compile(r"(\d+)/(\d+)(?: x(\d+\.\d+))?|-")
flagged = {}
for l in lines[start + 1:]:
    if not l.strip() or l.startswith("#"):
        break
    name, rest = l[:33].strip(), l[33:]
    if "skipped" in rest:
        continue
    parts = rest.split()
    vals = parts[2:]
    i, c = 0, 0
    while i < len(vals) and c < len(cols):
        v = vals[i]
        if v == "-":
            i, c = i + 1, c + 1
            continue
        full, half = (int(x) for x in v.split("/"))
        r = None
        if i + 1 < len(vals) and vals[i + 1].startswith("x"):
            i += 1
        if half >= 1 and full >= 20:
            r = full / half
        flags = []
        if full >= ms:
            flags.append(">=%gms" % ms)
        if r is not None and round(r, 1) >= ratio:
            flags.append(">=x%g" % ratio)
            flagged.setdefault(name, []).append(cols[c])
        if flags:
            rs = "x%.1f" % r if r is not None else "-"
            print("%-34s %-9s %6d/%-6d %-6s %s" % (name, cols[c], full, half, rs, " ".join(flags)))
        i, c = i + 1, c + 1
print()
print("shapes with a cell at x%g or more:" % ratio)
for k, v in flagged.items():
    print("  %s: %s" % (k, ", ".join(v)))

"""Run every generator in this directory against a binary and record the outcome.
Usage: uv run --no-project python run_all.py BINARY OUTDIR > repro-c1-m2a.json
The generators hard-code /private/tmp/wbrr; their source is read and that prefix replaced
by OUTDIR, so nothing is written elsewhere. Each case is a directory holding x.sh (fp.py
writes single files, one directory per file). Per case: `BINARY symbols DIR --json` (exit
code, first output line, symbol count), `BINARY ast FILE --json` (exit code, the errors it
reports and the kinds of the top-level nodes; `ast` exits 1 for a file skipped as too deep and
for a tree deeper than 1,000 levels, which it will not print) and `BINARY doctor DIR`.
Outcome of a case: `nesting error, skipped` (ast refuses the file with the nesting error),
`nesting error, rest analysed`, `analysed` (statements, no error), or `error gap` (the only
top-level node is a gap: resync found nothing it could parse). A crash case passes when the
binary exits 0 on symbols and doctor without timing out; an fp case passes when it is analysed
with no error: no syntax error in doctor, none in ast where ast prints it, and a symbols exit of 0."""
import json, os, subprocess, sys, shutil, datetime

binary, out = sys.argv[1], os.path.abspath(sys.argv[2])
here = os.path.dirname(os.path.abspath(__file__))
shutil.rmtree(out, ignore_errors=True)
os.makedirs(os.path.join(out, "cases"))
os.makedirs(os.path.join(out, "fp"))
kind = {}
for g in ["gen.py", "gen2.py", "gen3.py", "fp.py"]:
    src = open(os.path.join(here, g)).read().replace("/private/tmp/wbrr", out)
    before = set(os.listdir(os.path.join(out, "cases"))) | set(os.listdir(os.path.join(out, "fp")))
    subprocess.run([sys.executable, "-c", src], check=True, stdout=subprocess.DEVNULL)
    for name in (set(os.listdir(os.path.join(out, "cases"))) | set(os.listdir(os.path.join(out, "fp")))) - before:
        kind[name] = (g, "fp" if g == "fp.py" else "crash")
# fp.py writes flat files: give each its own directory with x.sh
fpdir = os.path.join(out, "fp")
for f in sorted(os.listdir(fpdir)):
    p = os.path.join(fpdir, f)
    if os.path.isfile(p):
        d = os.path.join(out, "cases", f.removesuffix(".sh"))
        os.makedirs(d)
        shutil.move(p, os.path.join(d, "x.sh"))
        kind[f.removesuffix(".sh")] = ("fp.py", "fp")
        kind.pop(f, None)
def run(cmd):
    p = subprocess.run(cmd, capture_output=True, text=True, timeout=120)
    return p
results = {}
for name in sorted(os.listdir(os.path.join(out, "cases"))):
    d = os.path.join(out, "cases", name)
    g, k = kind[name]
    r = {"generator": g, "kind": k, "bytes": os.path.getsize(os.path.join(d, "x.sh"))}
    try:
        p = run([binary, "symbols", d, "--json"])
        r["symbols_exit"] = p.returncode
        r["symbols_first_line"] = (p.stdout.splitlines() or [""])[0]
        try:
            r["symbols"] = len(json.loads(p.stdout)["symbols"])
        except Exception:
            r["symbols"] = None
        r["symbols_stderr"] = (p.stderr.splitlines() or [""])[0][:200]
        p = run([binary, "ast", os.path.join(d, "x.sh"), "--json"])
        r["ast_exit"] = p.returncode
        r["ast_stderr"] = (p.stderr.splitlines() or [""])[0][:200]
        if p.returncode == 0:
            a = json.loads(p.stdout)
            r["errors"] = [e["msg"] for e in a.get("errors", [])][:3]
            r["n_errors"] = len(a.get("errors", []))
            r["skipped"] = a.get("skipped", "")
            kids = [c["kind"] for c in a["root"].get("children") or []]
            r["statements"] = sum(k != "ErrorGap" for k in kids)
            r["gaps"] = sum(k == "ErrorGap" for k in kids)
    except subprocess.TimeoutExpired:
        r["timeout"] = True
    nest = "nesting is deeper than 10000 levels"
    p = run([binary, "doctor", d])
    r["doctor_exit"] = p.returncode
    r["doctor_syntax_errors"] = next((int(l.split(":")[1].split()[0]) for l in p.stdout.splitlines() if l.startswith("syntax errors:")), None)
    r["doctor_skipped"] = next((int(l.split(":")[1]) for l in p.stdout.splitlines() if l.startswith("skipped:")), None)
    if nest in r.get("ast_stderr", ""):
        r["outcome"] = "nesting error, skipped"
    elif nest in r.get("errors", []):
        r["outcome"] = "nesting error, rest analysed"
    elif r.get("statements", 0) > 0 and r.get("n_errors", 1) == 0:
        r["outcome"] = "analysed"
    elif r.get("statements", 0) > 0:
        r["outcome"] = "analysed, with errors"
    elif r.get("gaps"):
        r["outcome"] = "error gap"
    else:  # ast would not print the tree (deeper than 1,000 levels): doctor's count decides
        r["outcome"] = "analysed" if r.get("symbols_exit") == 0 and r["doctor_syntax_errors"] == 0 else "unknown"
    ran = r.get("symbols_exit") == 0 and r.get("doctor_exit") == 0 and "timeout" not in r
    if k == "fp":
        ok = ran and r["doctor_syntax_errors"] == 0 and r["doctor_skipped"] == 0 and r.get("n_errors", 0) == 0
    else:
        ok = ran
    r["pass"] = ok
    results[name] = r
rec = {"date": datetime.datetime.now().astimezone().isoformat(timespec="seconds"),
       "binary": binary, "commit": subprocess.check_output(["git", "rev-parse", "--short", "HEAD"], cwd=here, text=True).strip(),
       "all_pass": all(r["pass"] for r in results.values()), "cases": results}
print(json.dumps(rec, indent=1))

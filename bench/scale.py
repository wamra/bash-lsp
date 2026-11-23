"""Scale timing: one large script (N generated functions). Time from didOpen to a
documentSymbol answer, then a warm re-query after a one-line edit, and RSS.
Usage: uv run --no-project python scale.py NAME BIGFILE -- SERVER_CMD..."""
import json, os, shutil, subprocess, sys, tempfile, time
import bench

name, big = sys.argv[1], sys.argv[2]
cmd = sys.argv[sys.argv.index("--") + 1:]
root = os.path.realpath(tempfile.mkdtemp()); shutil.copy(big, os.path.join(root, "big.sh"))
s, r, init_ms = bench.start(cmd, root)
text = open(os.path.join(root, "big.sh")).read()
uri = s.uri("big.sh")
t = time.time()
s.notify("textDocument/didOpen", {"textDocument": {"uri": uri, "languageId": "shellscript", "version": 1, "text": text}})
n = 0
while time.time() - t < 120:
    r = s.call("textDocument/documentSymbol", {"textDocument": {"uri": uri}}, timeout=120)
    n = len(r) if isinstance(r, list) else 0
    if n:
        break
    time.sleep(0.05)
open_ms = round((time.time() - t) * 1000)
# edit: full-document change (works for full and incremental sync servers), then re-query
text2 = text.replace("echo start", "echo started", 1)
t = time.time()
s.notify("textDocument/didChange", {"textDocument": {"uri": uri, "version": 2}, "contentChanges": [{"text": text2}]})
r = s.call("textDocument/documentSymbol", {"textDocument": {"uri": uri}}, timeout=120)
edit_ms = round((time.time() - t) * 1000)
t = time.time()
d = s.call("textDocument/definition", {"textDocument": {"uri": uri}, "position": {"line": text2.split("\n").index("fn_1999 a b") if "fn_1999 a b" in text2.split("\n") else 5, "character": 2}}, timeout=60)
def_ms = round((time.time() - t) * 1000, 1)
s.pump(1)
rss = int(subprocess.check_output(["ps", "-o", "rss=", "-p", str(s.p.pid)]).split()[0]) // 1024
s.p.kill(); shutil.rmtree(root, ignore_errors=True)
print(json.dumps({"name": name, "bytes": len(text), "symbols": n, "open_to_symbols_ms": open_ms,
                  "edit_to_symbols_ms": edit_ms, "definition_ms": def_ms, "definition_found": bool(d) and not bench.err(d), "rss_mb": rss}))

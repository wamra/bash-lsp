"""Workspace scale timing: one overlay edit to a refreshed answer in a many-file
workspace made by gen_workspace.py. Opens one file in the middle of the chain,
edits it (full-document change), then asks for a definition that crosses a
`source` edge, and times didChange to that answer for two kinds of edit, kept
apart: (a) a body edit that leaves the source graph alone, (b) adding or removing
a `source` line (visibility is recomputed). Also times initialize to a
first workspace answer (the walk of every file).
Usage: uv run --no-project python scale_ws.py NAME WSDIR -- SERVER_CMD..."""
import json, os, shutil, statistics, subprocess, sys, tempfile, time
import bench

name, src = sys.argv[1], sys.argv[2]
cmd = sys.argv[sys.argv.index("--") + 1:]
root = os.path.join(os.path.realpath(tempfile.mkdtemp()), "ws")
shutil.copytree(src, root)
mid = len([f for f in os.listdir(root) if f.endswith(".sh")]) // 2
rel = f"s{mid:04d}.sh"
t0 = time.time()
s, r, init_ms = bench.start(cmd, root)
q = s.call("workspace/symbol", {"query": f"f{mid}_0"}, timeout=120)
walk_ms = round((time.time() - t0) * 1000)
text = open(os.path.join(root, rel)).read()
uri = s.uri(rel)
s.notify("textDocument/didOpen", {"textDocument": {"uri": uri, "languageId": "shellscript", "version": 1, "text": text}})
call_line = f'  f{mid-1}_0 "$a"'
ln = text.split("\n").index(call_line)
def definition():
    return s.call("textDocument/definition", {"textDocument": {"uri": uri},
                  "position": {"line": ln, "character": 4}}, timeout=60)
d0 = definition()
ok = bool(d0) and not bench.err(d0)
version = 1
def timed_edit(new_text):
    """didChange (full text) to a refreshed cross-file definition, ms."""
    global version, ok
    version += 1
    t = time.time()
    s.notify("textDocument/didChange", {"textDocument": {"uri": uri, "version": version}, "contentChanges": [{"text": new_text}]})
    d = definition()
    ms = round((time.time() - t) * 1000, 1)
    ok = ok and bool(d) and not bench.err(d)
    return ms
# (a) body edits: a trailing comment line, the source graph is untouched
body_ms = []
for v in range(6):
    text = text + f"# edit {v}\n"
    body_ms.append(timed_edit(text))
# (b) graph edits: append a `source` line to a file lower in the chain (line numbers above it stay put), then remove it, alternately
# (the visibility sets must be recomputed each time)
edge = f"source ./s{mid // 2:04d}.sh\n"
base = text
graph_ms = []
for v in range(6):
    graph_ms.append(timed_edit(base if v % 2 else base + edge))
s.pump(0.2)
rss = int(subprocess.check_output(["ps", "-o", "rss=", "-p", str(s.p.pid)]).split()[0]) // 1024
s.p.kill(); shutil.rmtree(os.path.dirname(root), ignore_errors=True)
print(json.dumps({"name": name, "files": mid * 2, "walk_to_first_answer_ms": walk_ms, "initialize_ms": init_ms,
                  "workspace_symbol_hits": len(q) if isinstance(q, list) else 0, "definition_found": ok,
                  "body_edit_to_definition_ms": body_ms, "body_edit_median_ms": statistics.median(body_ms),
                  "graph_edit_to_definition_ms": graph_ms, "graph_edit_median_ms": statistics.median(graph_ms), "rss_mb": rss}))

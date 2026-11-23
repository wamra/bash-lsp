"""Workspace scale timing: one overlay edit to a refreshed answer in a many-file
workspace made by gen_workspace.py. Opens one file in the middle of the chain,
edits it (full-document change), then asks for a definition that crosses a
`source` edge, and times didChange to that answer. Also times initialize to a
first workspace answer (the walk of every file).
Usage: uv run --no-project python scale_ws.py NAME WSDIR -- SERVER_CMD..."""
import json, os, shutil, subprocess, sys, tempfile, time
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
lines = text.split("\n")
ln = lines.index(call_line)
def definition():
    return s.call("textDocument/definition", {"textDocument": {"uri": uri},
                  "position": {"line": ln, "character": 4}}, timeout=60)
d0 = definition()
edit_ms = []
for v in range(2, 8):
    text = text.replace("# Function", "# Function", 1) + f"# edit {v}\n"
    t = time.time()
    s.notify("textDocument/didChange", {"textDocument": {"uri": uri, "version": v}, "contentChanges": [{"text": text}]})
    d = definition()
    edit_ms.append(round((time.time() - t) * 1000, 1))
    s.pump(0)
s.pump(0.2)
rss = int(subprocess.check_output(["ps", "-o", "rss=", "-p", str(s.p.pid)]).split()[0]) // 1024
s.p.kill(); shutil.rmtree(os.path.dirname(root), ignore_errors=True)
print(json.dumps({"name": name, "files": mid * 2, "walk_to_first_answer_ms": walk_ms, "initialize_ms": init_ms,
                  "workspace_symbol_hits": len(q) if isinstance(q, list) else 0,
                  "definition_found": bool(d0) and not bench.err(d0) and bool(d),
                  "edit_to_definition_ms": edit_ms, "edit_to_definition_ms_median": sorted(edit_ms)[len(edit_ms) // 2], "rss_mb": rss}))

"""Diagnostics push timing: didChange to the native publishDiagnostics for that
document, on a many-file workspace (gen_workspace.py) and on one large file
(gen_big.py). Run it with ShellCheck off (BASH_LSP_CONFIG pointing at
config-shellcheck-off.toml) so that only the native findings are timed.

The push of a large file can be big, so this script reads the server's pipe with
a buffer of its own (bench.py's reader appends byte by byte; see the note in
README.md) after the initialize call.

Usage: uv run --no-project python scale_diag.py NAME WSDIR BIGFILE -- SERVER_CMD...
Prints one JSON object (the unit repeat.py repeats)."""
import json, os, select, shutil, statistics, subprocess, sys, tempfile, time
import bench

name, wsdir, big = sys.argv[1], sys.argv[2], sys.argv[3]
cmd = sys.argv[sys.argv.index("--") + 1:]


class Reader:
    """Buffered reader of Content-Length frames from a bench.Server's pipe."""

    def __init__(self, s):
        self.s, self.fd, self.buf = s, s.p.stdout.fileno(), bytearray()

    def frame(self, timeout):
        end = time.time() + timeout
        while True:
            i = self.buf.find(b"\r\n\r\n")
            if i >= 0:
                head = bytes(self.buf[:i]).split(b"\r\n")
                n = int([h for h in head if h.lower().startswith(b"content-length")][0].split(b":")[1])
                if len(self.buf) >= i + 4 + n:
                    body = bytes(self.buf[i + 4:i + 4 + n])
                    del self.buf[:i + 4 + n]
                    return json.loads(body), n
            rem = end - time.time()
            if rem <= 0 or not select.select([self.fd], [], [], rem)[0]:
                return None, 0
            chunk = os.read(self.fd, 1 << 20)
            if not chunk:
                raise EOFError("server closed its output")
            self.buf += chunk

    def push(self, uri, version, timeout=120):
        """Wait for the publishDiagnostics of uri at version: (findings, bytes)."""
        end = time.time() + timeout
        while time.time() < end:
            m, n = self.frame(end - time.time())
            if m is None:
                break
            if m.get("method") == "textDocument/publishDiagnostics" and m["params"]["uri"] == uri \
                    and m["params"].get("version") == version:
                return len(m["params"]["diagnostics"]), n
            if "id" in m and "method" in m:  # a request from the server: answer null
                self.s._write({"jsonrpc": "2.0", "id": m["id"], "result": None})
        raise TimeoutError(f"no publishDiagnostics for {uri} version {version}")


def session(root):
    s, _, init_ms = bench.start(cmd, root)
    return s, Reader(s), init_ms


def edit_ms(s, rd, uri, version, text):
    t = time.time()
    s.notify("textDocument/didChange", {"textDocument": {"uri": uri, "version": version}, "contentChanges": [{"text": text}]})
    count, size = rd.push(uri, version)
    return round((time.time() - t) * 1000, 1), count, size


def open_ms(s, rd, uri, text):
    t = time.time()
    s.notify("textDocument/didOpen", {"textDocument": {"uri": uri, "languageId": "shellscript", "version": 1, "text": text}})
    count, size = rd.push(uri, 1)
    return round((time.time() - t) * 1000, 1), count, size


out = {"name": name}

# the workspace: open a file in the middle of the chain, then edit its body
root = os.path.join(os.path.realpath(tempfile.mkdtemp()), "ws")
shutil.copytree(wsdir, root)
mid = len([f for f in os.listdir(root) if f.endswith(".sh")]) // 2
rel = f"s{mid:04d}.sh"
s, rd, _ = session(root)
text = open(os.path.join(root, rel)).read()
uri = s.uri(rel)
o_ms, o_count, _ = open_ms(s, rd, uri, text)
ws_ms = []
for v in range(2, 12):
    text += f"# edit {v}\n"
    ws_ms.append(edit_ms(s, rd, uri, v, text)[0])
s.p.kill()
shutil.rmtree(os.path.dirname(root), ignore_errors=True)
out.update({"ws_files": mid * 2, "ws_open_to_push_ms": o_ms, "ws_findings": o_count,
            "ws_edit_to_push_ms": ws_ms, "ws_edit_median_ms": statistics.median(ws_ms)})

# the large file
root = os.path.realpath(tempfile.mkdtemp())
shutil.copy(big, os.path.join(root, "big.sh"))
s, rd, _ = session(root)
text = open(os.path.join(root, "big.sh")).read()
uri = s.uri("big.sh")
b_open, b_count, b_size = open_ms(s, rd, uri, text)
b_edits = []
for v in range(2, 6):
    text = text.replace("echo start", "echo started", 1) if v == 2 else text + f"# edit {v}\n"
    b_edits.append(edit_ms(s, rd, uri, v, text)[0])
s.p.kill()
shutil.rmtree(root, ignore_errors=True)
out.update({"big_bytes": len(text), "big_findings": b_count, "big_push_bytes": b_size, "big_open_to_push_ms": b_open,
            "big_edit_to_push_ms": b_edits, "big_edit_median_ms": statistics.median(b_edits)})
print(json.dumps(out))

"""Latency of the editor requests: 30 warm calls of each, medians and p95.

Measures every request added in 0.4.0 (completion, completionItem/resolve,
signatureHelp, semanticTokens full/range/delta, foldingRange, selectionRange,
documentHighlight, documentLink, inlayHint, prepareRename, rename and the
deferred hover, warm: answered from the help cache) on three inputs:

  corpus  a copy of CORPUS (an absolute path), main.sh open
  big     the large file BIGFILE (gen_big.py, 561 KB), open; also the time from
          didChange to the publishDiagnostics of that version
  chain   with --ws WSDIR (gen_workspace.py, 1,000 files): prepareRename and
          rename of a function whose callers sit in other files of the chain,
          from the middle file and from the last file (which sees the chain)

bench.py's Server starts the process, writes frames and builds positions (it is
imported, not edited). Its reader appends byte by byte, which is quadratic in
the size of a reply, so replies are read here with a buffer; the clock stops
when the last byte of the reply is read, before it is decoded.

Run it with ShellCheck off (BASH_LSP_CONFIG = absolute path of
config-shellcheck-off.toml) so that only native work is timed, and BENCH_CORPUS
is not needed. Record `uptime` before: the bar is a 1-minute load average under 4.

Usage: uv run --no-project python editor_latency.py NAME BIGFILE CORPUS [--ws WSDIR] -- SERVER_CMD...
Prints one JSON object: name, commit, load averages, and per input a table
{method: {median_ms, p95_ms, min_ms, max_ms, n, samples_ms}}."""
import copy, datetime, json, os, select, shutil, statistics, subprocess, sys, tempfile, time
import bench

args = sys.argv[1:]
cmd = args[args.index("--") + 1:]
args = args[:args.index("--")]
ws_dir = None
if "--ws" in args:
    i = args.index("--ws")
    ws_dir = os.path.realpath(args[i + 1])
    del args[i:i + 2]
name, big, corpus = args[0], os.path.realpath(args[1]), os.path.realpath(args[2])
WARMUP, N = 3, 30
REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

CAPS = copy.deepcopy(bench.CAPS)
CAPS["textDocument"]["semanticTokens"] = {
    "requests": {"full": {"delta": True}, "range": True}, "tokenTypes": [], "tokenModifiers": [], "formats": ["relative"]}
CAPS["textDocument"]["completion"] = {"completionItem": {"resolveSupport": {"properties": ["documentation"]}}}


class Client(bench.Server):
    """bench.Server with a buffered reader; frames carry the wall time of their last byte."""

    def __init__(self, cmd, root):
        super().__init__(cmd, root)
        self.fd, self.buf = self.p.stdout.fileno(), bytearray()

    def frame(self, timeout):
        """(body bytes, arrival time) or (None, None) on timeout."""
        end = time.perf_counter() + timeout
        while True:
            i = self.buf.find(b"\r\n\r\n")
            if i >= 0:
                head = bytes(self.buf[:i]).split(b"\r\n")
                n = int([h for h in head if h.lower().startswith(b"content-length")][0].split(b":")[1])
                if len(self.buf) >= i + 4 + n:
                    body = bytes(self.buf[i + 4:i + 4 + n])
                    del self.buf[:i + 4 + n]
                    return body, time.perf_counter()
            rem = end - time.perf_counter()
            if rem <= 0 or not select.select([self.fd], [], [], rem)[0]:
                return None, None
            chunk = os.read(self.fd, 1 << 20)
            if not chunk:
                raise EOFError("server closed its output")
            self.buf += chunk

    def _serve_request(self, m):
        res = None
        if m["method"] == "workspace/configuration":
            res = [None for _ in m["params"].get("items", [])]
        elif m["method"] == "workspace/workspaceFolders":
            res = [{"uri": "file://" + self.root, "name": "root"}]
        self._write({"jsonrpc": "2.0", "id": m["id"], "result": res})

    def _other(self, m):
        if m.get("method") == "textDocument/publishDiagnostics":
            self.diags[m["params"]["uri"]] = (m["params"].get("version"), len(m["params"]["diagnostics"]))
        elif "id" in m and "method" in m:
            self._serve_request(m)

    def timed(self, method, params, timeout=60):
        """(result, ms) of one request: write to the last byte of its reply."""
        self.nid += 1
        i = self.nid
        prefix = b'{"jsonrpc":"2.0","id":%d,' % i
        t0 = time.perf_counter()
        self._write({"jsonrpc": "2.0", "id": i, "method": method, "params": params})
        end = t0 + timeout
        while True:
            body, t1 = self.frame(max(0.0, end - time.perf_counter()))
            if body is None:
                raise TimeoutError(method)
            if body.startswith(prefix):
                m = json.loads(body)
                if "error" in m:
                    raise RuntimeError(f"{method}: {m['error']}")
                return m.get("result"), (t1 - t0) * 1000
            self._other(json.loads(body))

    def call(self, method, params, timeout=60):
        return self.timed(method, params, timeout)[0]

    def pump(self, secs):
        end = time.perf_counter() + secs
        while time.perf_counter() < end:
            body, _ = self.frame(max(0.0, end - time.perf_counter()))
            if body is not None:
                self._other(json.loads(body))

    def wait_push(self, uri, version, timeout=60):
        end = time.perf_counter() + timeout
        while time.perf_counter() < end:
            body, t1 = self.frame(max(0.0, end - time.perf_counter()))
            if body is None:
                break
            m = json.loads(body)
            if m.get("method") == "textDocument/publishDiagnostics" and m["params"]["uri"] == uri \
                    and m["params"].get("version") == version:
                return t1
            self._other(m)
        raise TimeoutError(f"no publishDiagnostics for {uri} version {version}")

    def start(self, root, init_opts=None):
        params = {"processId": os.getpid(), "rootUri": "file://" + root, "rootPath": root,
                  "workspaceFolders": [{"uri": "file://" + root, "name": "root"}], "capabilities": CAPS}
        if init_opts:
            params["initializationOptions"] = init_opts
        r = self.call("initialize", params, timeout=30)
        self.notify("initialized", {})
        return r

    def did_open(self, rel, version=1):
        text = open(os.path.join(self.root, rel)).read()
        self.notify("textDocument/didOpen", {"textDocument": {
            "uri": self.uri(rel), "languageId": "shellscript", "version": version, "text": text}})
        return text


def summarize(samples):
    xs = sorted(samples)
    return {"median_ms": round(statistics.median(xs), 2), "p95_ms": round(xs[min(len(xs) - 1, int(len(xs) * 0.95))], 2),
            "min_ms": round(xs[0], 2), "max_ms": round(xs[-1], 2), "n": len(xs), "samples_ms": [round(x, 2) for x in samples]}


def measure(c, method, params_fn, results, key=None, check=None):
    """WARMUP unrecorded calls, then N recorded ones. params_fn(prev_result) -> params."""
    prev, samples = None, []
    for k in range(WARMUP + N):
        prev, ms = c.timed(method, params_fn(prev))
        if k == 0 and check is not None:
            check(prev)
        if k >= WARMUP:
            samples.append(ms)
    results[key or method] = summarize(samples)
    if isinstance(prev, list):
        results[key or method]["items"] = len(prev)
    return prev


def line_of(text, needle, occ=0):
    seen = 0
    for ln, t in enumerate(text.split("\n")):
        if needle in t:
            if seen == occ:
                return ln, t.index(needle)
            seen += 1
    raise ValueError(needle)


def run_input(c, rel, text, spots, results, lines_range, tag):
    """The editor methods on one open file. spots maps a role to (line, character)."""
    uri = c.uri(rel)
    doc = {"uri": uri}
    pos = lambda role: {"line": spots[role][0], "character": spots[role][1]}
    at = lambda role: {"textDocument": doc, "position": pos(role)}
    nlines = text.count("\n") + 1
    first, last = lines_range
    whole = {"start": {"line": 0, "character": 0}, "end": {"line": nlines, "character": 0}}
    some = {"start": {"line": first, "character": 0}, "end": {"line": last, "character": 0}}

    def nonempty(what):
        def chk(r):
            if r is None or r == [] or r == {}:
                raise RuntimeError(f"{tag} {what}: empty answer {r!r}")
        return chk

    comp = measure(c, "textDocument/completion", lambda _: at("complete"), results, check=nonempty("completion"))
    items = comp["items"] if isinstance(comp, dict) else comp
    item = next((i for i in items if i.get("label") == spots.get("resolve_label")), None)
    if item is not None:
        measure(c, "completionItem/resolve", lambda _: item, results)
    measure(c, "textDocument/signatureHelp", lambda _: at("call"), results, check=nonempty("signatureHelp"))
    full = measure(c, "textDocument/semanticTokens/full", lambda _: {"textDocument": doc}, results, check=nonempty("semanticTokens/full"))
    measure(c, "textDocument/semanticTokens/range", lambda _: {"textDocument": doc, "range": some}, results,
            check=nonempty("semanticTokens/range"))
    rid = full.get("resultId")
    state = {"id": rid}

    def delta_params(prev):
        if isinstance(prev, dict) and prev.get("resultId"):
            state["id"] = prev["resultId"]
        return {"textDocument": doc, "previousResultId": state["id"]}
    measure(c, "textDocument/semanticTokens/full/delta", delta_params, results)
    measure(c, "textDocument/foldingRange", lambda _: {"textDocument": doc}, results, check=nonempty("foldingRange"))
    measure(c, "textDocument/selectionRange", lambda _: {"textDocument": doc, "positions": [pos("highlight")]}, results,
            check=nonempty("selectionRange"))
    measure(c, "textDocument/documentHighlight", lambda _: at("highlight"), results, check=nonempty("documentHighlight"))
    measure(c, "textDocument/documentLink", lambda _: {"textDocument": doc}, results)
    measure(c, "textDocument/inlayHint", lambda _: {"textDocument": doc, "range": whole}, results)
    measure(c, "textDocument/prepareRename", lambda _: at("rename"), results, check=nonempty("prepareRename"))
    measure(c, "textDocument/rename", lambda _: dict(at("rename"), newName=spots["new_name"]), results,
            check=nonempty("rename"))
    if "hover" in spots:
        # the first call runs `help` (cold, recorded apart); the timed ones are answered from the cache
        r, cold = c.timed("textDocument/hover", at("hover"))
        results["hover_cold_ms"] = round(cold, 2)
        measure(c, "textDocument/hover", lambda _: at("hover"), results, check=nonempty("hover"))


def new_root():
    return os.path.realpath(tempfile.mkdtemp(prefix="wb-lat-"))


def start_client(root, init_opts=None):
    c = Client(cmd, root)
    c.start(root, init_opts)
    return c


def load():
    return [round(x, 2) for x in os.getloadavg()]


def sh(*a):
    return subprocess.check_output(a, cwd=REPO, text=True).strip()


out = {"name": name, "date": datetime.datetime.now().astimezone().isoformat(timespec="seconds"),
       "commit": sh("git", "rev-parse", "--short", "HEAD"),
       "dirty": bool(sh("git", "status", "--porcelain", "--", "cmd", "internal", "analysis", "tree")),
       "load_before": load(), "n": N, "warmup": WARMUP}

# ---- corpus ----
root = new_root()
shutil.rmtree(root)
shutil.copytree(corpus, root)
c = start_client(root)
text = c.did_open("main.sh")
c.pump(1.5)
spots = {
    "complete": line_of(text, "build_release \"$name\"")[0:1] + (5,),  # three characters into the word: a prefix
    "call": (line_of(text, "build_release \"$name\"")[0], text.split("\n")[line_of(text, "build_release \"$name\"")[0]].index("\"$name\"")),
    "highlight": (line_of(text, "local name=\"$1\"")[0], line_of(text, "local name=\"$1\"")[1] + 7),
    "rename": (line_of(text, "  build_release \"$name\"")[0], 4),
    "new_name": "build_release_renamed",
    "resolve_label": "build_release",
    "hover": (line_of(text, "  echo \"$name\"")[0], 3),
}
corpus_res = {}
run_input(c, "main.sh", text, spots, corpus_res, (0, 100), "corpus")
c.p.kill()
# inlayHint again with parameter names on (off by default)
c = start_client(root, {"inlay_hints": {"parameter_names": True}})
c.did_open("main.sh")
c.pump(1.5)
hint_res = {}
measure(c, "textDocument/inlayHint", lambda _: {"textDocument": {"uri": c.uri("main.sh")}, "range": {
    "start": {"line": 0, "character": 0}, "end": {"line": text.count("\n") + 1, "character": 0}}}, hint_res)
corpus_res["inlayHint_parameter_names"] = hint_res["textDocument/inlayHint"]
c.p.kill()
shutil.rmtree(root, ignore_errors=True)
out["corpus"] = corpus_res

# ---- the large file ----
root = new_root()
shutil.copy(big, os.path.join(root, "big.sh"))
c = start_client(root)
btext = c.did_open("big.sh")
t0 = time.perf_counter()
while time.perf_counter() - t0 < 120:  # as scale.py: wait for the first documentSymbol
    r = c.call("textDocument/documentSymbol", {"textDocument": {"uri": c.uri("big.sh")}}, timeout=120)
    if isinstance(r, list) and r:
        break
    time.sleep(0.05)
c.pump(2.0)
mid = 1000
fn_ln, _ = line_of(btext, f"fn_{mid}() {{")
call_ln, call_col = line_of(btext, f"fn_{mid - 1} a b")
loc_ln, loc_col = line_of(btext, "local a=\"$1\" b=\"$2\"", mid)
bspots = {
    "complete": (fn_ln + 3, 7),
    "call": (call_ln, call_col + 7),
    "highlight": (loc_ln, loc_col + 6),
    "rename": (fn_ln, 2),
    "new_name": "renamed_function",
}
big_res = {}
run_input(c, "big.sh", btext, bspots, big_res, (fn_ln, fn_ln + 100), "big")

# didChange to publishDiagnostics of that version (ShellCheck off), as scale_diag.py
uri = c.uri("big.sh")
edits = []
version = 1
cur = btext
for k in range(10):
    version += 1
    cur = cur.replace("echo start", "echo started", 1) if k == 0 else cur + f"# edit {version}\n"
    t = time.perf_counter()
    c.notify("textDocument/didChange", {"textDocument": {"uri": uri, "version": version}, "contentChanges": [{"text": cur}]})
    edits.append((c.wait_push(uri, version) - t) * 1000)
big_res["didChange_to_publish"] = summarize(edits)
c.p.kill()
# inlayHint again with parameter names on (off by default): the large file then has a hint on every call argument
c = start_client(root, {"inlay_hints": {"parameter_names": True}})
c.did_open("big.sh")
c.call("textDocument/documentSymbol", {"textDocument": {"uri": uri}}, timeout=120)
c.pump(2.0)
hint_res = {}
nh = measure(c, "textDocument/inlayHint", lambda _: {"textDocument": {"uri": uri}, "range": {
    "start": {"line": 0, "character": 0}, "end": {"line": btext.count("\n") + 1, "character": 0}}}, hint_res)
big_res["inlayHint_parameter_names"] = hint_res["textDocument/inlayHint"]
c.p.kill()
shutil.rmtree(root, ignore_errors=True)
out["big"] = big_res
out["big_bytes"] = len(btext)

# ---- the 1,000-file chain ----
if ws_dir:
    root = os.path.join(new_root(), "ws")
    shutil.copytree(ws_dir, root)
    nfiles = len([f for f in os.listdir(root) if f.endswith(".sh")])
    chain = {"files": nfiles}
    c = start_client(root)
    c.call("workspace/symbol", {"query": "f0_0"}, timeout=180)  # the first workspace answer: every file walked
    for label, idx, k in (("mid", nfiles // 2, 0), ("tail", nfiles - 1, 0)):
        rel = f"s{idx:04d}.sh"
        wtext = c.did_open(rel)
        c.pump(0.5)
        ln, col = line_of(wtext, f"f{idx}_{k}() {{")
        at = {"textDocument": {"uri": c.uri(rel)}, "position": {"line": ln, "character": col + 1}}
        res = {}
        measure(c, "textDocument/prepareRename", lambda _: at, res, check=lambda r: r or (_ for _ in ()).throw(RuntimeError("empty")))
        r = measure(c, "textDocument/rename", lambda _: dict(at, newName=f"renamed_{idx}_{k}"), res)
        res["rename_files_touched"] = len((r or {}).get("changes", {})) or len((r or {}).get("documentChanges", []))
        chain[label] = res
    c.p.kill()
    shutil.rmtree(os.path.dirname(root), ignore_errors=True)
    out["chain"] = chain

out["load_after"] = load()
print(json.dumps(out))

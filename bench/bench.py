"""Accuracy benchmark for bash language servers (M0 of the bash-lsp BRD/FDS).

Usage: uv run --no-project python bench.py NAME -- SERVER_CMD [ARGS...]
Runs every case against the corpus/ workspace (copied to a temp dir) and
prints one JSON object: {name, caps, timings, results:{case: [pass, detail]}}.
"""
import json, os, select, shutil, subprocess, sys, tempfile, time

HERE = os.path.dirname(os.path.abspath(__file__))


class Server:
    def __init__(self, cmd, root):
        self.root = root
        self.p = subprocess.Popen(cmd, stdin=subprocess.PIPE, stdout=subprocess.PIPE, bufsize=0,
                                  stderr=(open(os.environ["BENCH_STDERR"], "wb") if os.environ.get("BENCH_STDERR") else subprocess.DEVNULL), cwd=root)
        self.nid = 0
        self.diags = {}
        self.opened = set()

    def _write(self, m):
        b = json.dumps(m).encode()
        data = memoryview(b"Content-Length: %d\r\n\r\n" % len(b) + b)
        while data:  # unbuffered pipe: a raw write may be partial
            data = data[self.p.stdin.write(data):]

    def _readn(self, n):
        """Read exactly n bytes from the unbuffered pipe (a raw read may return fewer)."""
        b = b""
        while len(b) < n:
            c = self.p.stdout.read(n - len(b))
            if not c:
                return None
            b += c
        return b

    def _read(self, timeout):
        # The pipe is unbuffered (bufsize=0): with a buffered reader a second message
        # already in Python's buffer is invisible to select() and the call stalls.
        if not select.select([self.p.stdout], [], [], timeout)[0]:
            return None
        h = b""
        while not h.endswith(b"\r\n\r\n"):
            c = self._readn(1)
            if c is None:
                return None
            h += c
        n = int([l for l in h.split(b"\r\n") if l.lower().startswith(b"content-length")][0].split(b":")[1])
        body = self._readn(n)
        return None if body is None else json.loads(body)

    def _handle(self, m):
        if m.get("method") == "textDocument/publishDiagnostics":
            self.diags[m["params"]["uri"]] = m["params"]["diagnostics"]
        elif "id" in m and "method" in m:  # server -> client request
            res = None
            if m["method"] == "workspace/configuration":
                res = [None for _ in m["params"].get("items", [])]
            elif m["method"] == "workspace/workspaceFolders":
                res = [{"uri": "file://" + self.root, "name": "corpus"}]
            self._write({"jsonrpc": "2.0", "id": m["id"], "result": res})

    def notify(self, method, params):
        self._write({"jsonrpc": "2.0", "method": method, "params": params})

    def call(self, method, params, timeout=10):
        self.nid += 1
        i = self.nid
        self._write({"jsonrpc": "2.0", "id": i, "method": method, "params": params})
        end = time.time() + timeout
        while time.time() < end:
            m = self._read(max(0.05, end - time.time()))
            if m is None:
                continue
            if m.get("id") == i and "method" not in m:
                if "error" in m:
                    return {"__error__": m["error"]}
                return m.get("result")
            self._handle(m)
        return {"__error__": "timeout"}

    def pump(self, secs):
        end = time.time() + secs
        while time.time() < end:
            m = self._read(0.1)
            if m:
                self._handle(m)

    def diag(self, f):
        """Pushed diagnostics, else pulled (textDocument/diagnostic)."""
        d = self.diags.get(self.uri(f))
        if d:
            return d
        r = self.call("textDocument/diagnostic", {"textDocument": {"uri": self.uri(f)}}, timeout=5)
        if isinstance(r, dict) and isinstance(r.get("items"), list):
            return r["items"]
        return d or []

    def uri(self, f):
        return "file://" + os.path.join(self.root, f)

    def open(self, f, settle=1.5):
        if f in self.opened:
            return
        self.opened.add(f)
        self.notify("textDocument/didOpen", {"textDocument": {
            "uri": self.uri(f), "languageId": "shellscript", "version": 1,
            "text": open(os.path.join(self.root, f)).read()}})
        if settle:
            self.pump(settle)

    def pos(self, f, needle, occ=0, off=0):
        """Position of the occ-th occurrence of needle in f, plus off chars."""
        lines = open(os.path.join(self.root, f)).read().split("\n")
        seen = 0
        for ln, text in enumerate(lines):
            start = 0
            while True:
                c = text.find(needle, start)
                if c < 0:
                    break
                if seen == occ:
                    return {"line": ln, "character": c + off}
                seen += 1
                start = c + 1
        raise ValueError((f, needle, occ))

    def tdp(self, f, needle, occ=0, off=0):
        return {"textDocument": {"uri": self.uri(f)}, "position": self.pos(f, needle, occ, off)}


CAPS = {
    "workspace": {"workspaceFolders": True, "configuration": True,
                  "symbol": {"symbolKind": {"valueSet": list(range(1, 27))}}},
    "textDocument": {
        "synchronization": {"didSave": True},
        "hover": {"contentFormat": ["markdown", "plaintext"]},
        "definition": {"linkSupport": False}, "references": {}, "implementation": {},
        "documentSymbol": {"hierarchicalDocumentSymbolSupport": True,
                           "symbolKind": {"valueSet": list(range(1, 27))}},
        "callHierarchy": {}, "publishDiagnostics": {}, "rename": {"prepareSupport": True},
        "foldingRange": {}, "semanticTokens": {"requests": {"full": True}, "tokenTypes": [],
                                               "tokenModifiers": [], "formats": ["relative"]},
        "codeAction": {}, "completion": {}, "signatureHelp": {}, "inlayHint": {},
        "documentLink": {}, "selectionRange": {}, "formatting": {},
    },
}


def start(cmd, root):
    s = Server(cmd, root)
    t = time.time()
    r = s.call("initialize", {"processId": os.getpid(), "rootUri": "file://" + root,
                              "rootPath": root,
                              "workspaceFolders": [{"uri": "file://" + root, "name": "corpus"}],
                              "capabilities": CAPS}, timeout=20)
    init_ms = round((time.time() - t) * 1000)
    s.notify("initialized", {})
    return s, r, init_ms


def locs(s, r):
    """Normalise Location | Location[] | LocationLink[] to a set of (relpath, line1)."""
    out = set()
    if not r or isinstance(r, dict) and "__error__" in r:
        return out
    if isinstance(r, dict):
        r = [r]
    for x in r:
        uri = x.get("uri") or x.get("targetUri")
        rng = x.get("range") or x.get("targetSelectionRange")
        path = os.path.realpath(uri[len("file://"):])
        out.add((os.path.relpath(path, os.path.realpath(s.root)), rng["start"]["line"] + 1))
    return out


def line(s, f, needle, occ=0):
    return (f, s.pos(f, needle, occ)["line"] + 1)


def err(r):
    return isinstance(r, dict) and "__error__" in r


def run(name, cmd):
    root = tempfile.mkdtemp(prefix="bashlsp-corpus-")
    root = os.path.realpath(root)
    shutil.rmtree(root)
    shutil.copytree(os.path.join(HERE, os.environ.get("BENCH_CORPUS", "corpus")), root)
    res = {}

    def case(cid, desc, fn):
        try:
            ok, detail = fn()
        except Exception as e:  # a crash or malformed answer is a fail
            ok, detail = False, "exception: %r" % (e,)
        res[cid] = [bool(ok), desc, str(detail)[:300]]

    # --- session 1: first documentSymbol immediately after didOpen ------------
    s, init, init_ms = start(cmd, root)
    caps = (init or {}).get("capabilities", {}) if not err(init) else {}
    s.open("main.sh", settle=0)
    r = s.call("textDocument/documentSymbol", {"textDocument": {"uri": s.uri("main.sh")}})
    case("C13", "first documentSymbol right after didOpen is non-empty",
         lambda: (bool(r) and not err(r), "%d symbols" % (len(r) if isinstance(r, list) else 0)))
    s.pump(3)  # let background analysis finish

    util_build = line(s, "lib/util.sh", "build_release() {")
    util_log = line(s, "lib/util.sh", "log() {")

    def c01():
        got = locs(s, s.call("textDocument/definition", s.tdp("main.sh", "  build_release \"$name\"", 0, 3)))
        return got == {util_build}, sorted(got)
    case("C01", "definition across a static `source ./lib/util.sh`", c01)

    def c01d():
        s.open("dyn.sh")
        got = locs(s, s.call("textDocument/definition", s.tdp("dyn.sh", "build_release dyn", 0, 3)))
        return got == {util_build}, sorted(got)
    case("C01d", "definition across `source \"$HERE/lib/util.sh\"` (HERE=$(dirname $0))", c01d)

    def c02():
        got = locs(s, s.call("textDocument/definition", s.tdp("main.sh", "  br \"$name\"", 0, 2)))
        alias_line = line(s, "main.sh", "alias br=")
        return util_build in got, "%s (alias def line %s also acceptable alongside)" % (sorted(got), alias_line[1])
    case("C02", "definition through alias reaches the function", c02)

    def c03():
        got = locs(s, s.call("textDocument/definition", s.tdp("main.sh", "trap 'log bye'", 0, 6)))
        return got == {util_log}, sorted(got)
    case("C03", "definition of function named in trap string", c03)

    refs = locs(s, s.call("textDocument/references", {
        **s.tdp("main.sh", "  build_release \"$name\"", 0, 3), "context": {"includeDeclaration": True}}))

    case("C04", "references exclude same-named function in unrelated orphan.sh",
         lambda: (bool(refs) and not any(f == "orphan.sh" for f, _ in refs), sorted(refs)))
    case("C04b", "references include the definition and the direct call",
         lambda: (util_build in refs and line(s, "main.sh", "  build_release \"$name\"") in refs, sorted(refs)))
    case("C05", "references include the call through alias br",
         lambda: (line(s, "main.sh", "  br \"$name\"") in refs or line(s, "main.sh", "alias br=") in refs, sorted(refs)))
    case("C06", "references include cmd=build_release / \"$cmd\"",
         lambda: (line(s, "main.sh", "local cmd=build_release") in refs or line(s, "main.sh", "\"$cmd\" x") in refs, sorted(refs)))
    case("C07", "references include eval \"build_release y\"",
         lambda: (line(s, "main.sh", "eval \"build_release y\"") in refs, sorted(refs)))

    def c08():
        got = locs(s, s.call("textDocument/references", {
            **s.tdp("main.sh", "local name=\"$1\"", 0, 6), "context": {"includeDeclaration": True}}))
        want = {line(s, "main.sh", "local name=\"$1\""), line(s, "main.sh", "  build_release \"$name\""),
                line(s, "main.sh", "  br \"$name\"")}
        return got == want, sorted(got)
    case("C08", "references of a local variable stay inside its function", c08)

    items = s.call("textDocument/prepareCallHierarchy", s.tdp("lib/util.sh", "build_release() {", 0, 2)) \
        if s.open("lib/util.sh") is None else None
    case("C09", "prepareCallHierarchy returns an item",
         lambda: (isinstance(items, list) and len(items) > 0, items if err(items) else "%d item(s)" % len(items or [])))

    def c10():
        if not isinstance(items, list) or not items:
            return False, "no item"
        r = s.call("callHierarchy/incomingCalls", {"item": items[0]})
        names = [c["from"]["name"] for c in (r or [])] if isinstance(r, list) else r
        return isinstance(names, list) and any("deploy" in n for n in names), names
    case("C10", "incoming calls of build_release include deploy", c10)

    def c11():
        it = s.call("textDocument/prepareCallHierarchy", s.tdp("main.sh", "deploy() {", 0, 2))
        if not isinstance(it, list) or not it:
            return False, it
        r = s.call("callHierarchy/outgoingCalls", {"item": it[0]})
        names = [c["to"]["name"] for c in (r or [])] if isinstance(r, list) else r
        return isinstance(names, list) and any("build_release" in n for n in names), names
    case("C11", "outgoing calls of deploy include build_release", c11)

    def c12():
        r = s.call("textDocument/documentSymbol", {"textDocument": {"uri": s.uri("main.sh")}})
        if not isinstance(r, list) or not r:
            return False, r
        hier = any(x.get("children") for x in r)
        return hier, "hierarchical=%s top=%s" % (hier, [x["name"] for x in r])
    case("C12", "document symbols are hierarchical (locals nested in function)", c12)

    def c14():
        r = s.call("textDocument/hover", s.tdp("lib/util.sh", "build_release() {", 0, 2))
        ok = isinstance(r, dict) and not err(r) and bool(r.get("contents"))
        return ok, json.dumps(r)[:200]
    case("C14", "hover at a function definition is non-empty", c14)

    def c14b():
        r = s.call("textDocument/hover", s.tdp("lib/util.sh", "log() {", 0, 1))
        txt = json.dumps(r)
        return "Log a message to stderr" in txt, txt[:200]
    case("C14b", "hover shows the doc comment above the function", c14b)

    def c24():
        r = s.call("textDocument/rename", {**s.tdp("main.sh", "local name=\"$1\"", 0, 6), "newName": "nm"})
        if not isinstance(r, dict) or err(r):
            return False, r
        got = set()
        for u, edits in (r.get("changes") or {}).items():
            for e in edits:
                got.add((os.path.relpath(os.path.realpath(u[7:]), root), e["range"]["start"]["line"] + 1))
        for dc in r.get("documentChanges") or []:
            for e in dc.get("edits", []):
                got.add((os.path.relpath(os.path.realpath(dc["textDocument"]["uri"][7:]), root),
                         e["range"]["start"]["line"] + 1))
        want = {line(s, "main.sh", "local name=\"$1\""), line(s, "main.sh", "  build_release \"$name\""),
                line(s, "main.sh", "  br \"$name\"")}
        return got == want, sorted(got)
    case("C24", "rename of a local variable edits only its function", c24)

    def c28():
        s.pump(4)
        d = s.diag("main.sh")
        ln = line(s, "main.sh", "rm $f")[1]
        hit = [x for x in d if x["range"]["start"]["line"] + 1 == ln]
        return bool(hit), [(x["range"]["start"]["line"] + 1, x.get("code"), x["message"][:50]) for x in d]
    case("C28", "diagnostic on unquoted `rm $f`", c28)

    def c29():
        got = locs(s, s.call("textDocument/definition", s.tdp("main.sh", "./lib/util.sh", 0, 7)))
        if got and all(f == "lib/util.sh" for f, _ in got):
            return True, sorted(got)
        r = s.call("textDocument/documentLink", {"textDocument": {"uri": s.uri("main.sh")}})
        ok = isinstance(r, list) and any("util.sh" in (x.get("target") or "") for x in r)
        return ok, "definition=%s links=%s" % (sorted(got), r if err(r) else len(r or []))
    case("C29", "source path resolves to lib/util.sh (definition or document link)", c29)

    def c25():
        s.open("orphan.sh")
        got = locs(s, s.call("textDocument/definition", s.tdp("orphan.sh", "build_release", 1, 2)))
        return got == {("orphan.sh", 2)}, sorted(got)
    case("C25", "call inside orphan.sh resolves to its own definition only", c25)

    def c15():
        s.open("app.sh"); s.open("lib/hooks.sh")
        got = locs(s, s.call("textDocument/definition", s.tdp("lib/hooks.sh", "  on_start", 0, 3)))
        return got == {line(s, "app.sh", "on_start() {")}, sorted(got)
    case("C15", "library call resolves to a function defined by the sourcing script", c15)

    def c16():
        got = locs(s, s.call("textDocument/references", {
            **s.tdp("app.sh", "on_start() {", 0, 2), "context": {"includeDeclaration": True}}))
        return line(s, "lib/hooks.sh", "  on_start") in got, sorted(got)
    case("C16", "references from the script find the call inside the library", c16)

    def c17():
        got = locs(s, s.call("textDocument/definition", s.tdp("app.sh", "plugin_a", 0, 2)))
        return got == {line(s, "plugins/a.sh", "plugin_a() {")}, sorted(got)
    case("C17", "definition through a glob loader (for f in plugins/*.sh; . \"$f\")", c17)

    def c22():
        r = s.call("workspace/symbol", {"query": "plugin_a"})
        ok = isinstance(r, list) and any(x.get("name") == "plugin_a" for x in r)
        return ok, [x.get("name") for x in r] if isinstance(r, list) else r
    case("C22", "workspace symbol finds a function in an unopened file", c22)

    def broken(f, fn_after, fn_before):
        s.open(f, settle=2.5)
        r = s.call("textDocument/documentSymbol", {"textDocument": {"uri": s.uri(f)}})
        names = []

        def walk(xs):
            for x in xs or []:
                names.append(x["name"])
                walk(x.get("children"))
        if isinstance(r, list):
            walk(r)
        return names
    def c18():
        n = broken("broken_fi.sh", "after_fi", "before_break")
        return "after_fi" in n and "before_break" in n, n
    case("C18", "missing `fi`: functions before and after are still in symbols", c18)

    def c19():
        n = broken("broken_exp.sh", "after_exp", "before_exp")
        return "after_exp" in n and "before_exp" in n, n
    case("C19", "unterminated `${x`: functions before and after are still in symbols", c19)

    def c20():
        s.pump(2)
        d1 = s.diag("broken_fi.sh")
        d2 = s.diag("broken_exp.sh")
        return bool(d1) and bool(d2), "broken_fi=%d broken_exp=%d" % (len(d1), len(d2))
    case("C20", "syntax errors are reported as diagnostics in both broken files", c20)

    # --- timing: warm definition latency, median of 20 ------------------------
    ts = []
    for _ in range(20):
        t = time.time()
        s.call("textDocument/definition", s.tdp("main.sh", "  build_release \"$name\"", 0, 3))
        ts.append((time.time() - t) * 1000)
    ts.sort()
    try:
        rss = int(subprocess.check_output(["ps", "-o", "rss=", "-p", str(s.p.pid)]).split()[0]) // 1024
    except Exception:
        rss = None
    s.p.kill()
    shutil.rmtree(root, ignore_errors=True)

    capkeys = ["definitionProvider", "referencesProvider", "hoverProvider", "documentSymbolProvider",
               "workspaceSymbolProvider", "implementationProvider", "callHierarchyProvider",
               "renameProvider", "documentFormattingProvider", "documentRangeFormattingProvider",
               "completionProvider", "signatureHelpProvider", "codeActionProvider",
               "semanticTokensProvider", "foldingRangeProvider", "selectionRangeProvider",
               "inlayHintProvider", "documentLinkProvider", "documentHighlightProvider",
               "diagnosticProvider", "codeLensProvider", "colorProvider"]
    return {"name": name,
            "caps": {k: bool(caps.get(k)) for k in capkeys},
            "sync": caps.get("textDocumentSync"),
            "timings": {"initialize_ms": init_ms, "definition_ms_median": round(ts[len(ts) // 2], 1),
                        "rss_mb": rss},
            "results": res}


if __name__ == "__main__":
    i = sys.argv.index("--")
    print(json.dumps(run(sys.argv[1], sys.argv[i + 1:]), indent=1))

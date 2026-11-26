"""Truth-file suite runner.

Usage: uv run --no-project python bench/suite.py NAME -- SERVER_CMD [ARGS...]
Env:   BENCH_CORPUS  absolute path of the corpus for the 28 original cases
                     (default: ../testdata/corpus)
       BENCH_STDERR  file receiving the server's stderr (suite sessions append)
       BENCH_SETTLE  seconds to wait after didOpen (default 2.0)
       SUITE_ONLY    comma-separated case ids: run only those suite cases

Prints one JSON object {name, caps, sync, timings, results, suite_meta}.
`results` holds the 28 verdicts of bench.run unchanged ([pass, desc, detail],
ids C01..) plus one entry per suite case, id S001..: a dict
{pass, tier, category, method, file, detail, ms}. Cases come from
suite_cases/*.json and are read-only inputs; each runs on a fresh server over a
fresh copy of testdata/corpus2 (the server only ever sees the case's own
didOpen).

Conventions of the expect keys: every line number in an expect (and in
error_lines_include, fold_start_lines_*, links_include) is a 1-based file
line; case `at` becomes a 0-based LSP position with a UTF-16 character offset.
An expect key the runner does not know is an error before any server starts,
and a key a checker did not consume fails the case; neither is a pass.
"""
import glob
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import bench  # noqa: E402  (frozen module, imported, never edited)

SETTLE = float(os.environ.get("BENCH_SETTLE", "2.0"))


# ---------------------------------------------------------------- helpers
def u16(s):
    return len(s.encode("utf-16-le")) // 2


def at_position(root, case):
    a = case["at"]
    text = open(os.path.join(root, case["file"]), encoding="utf-8").read().split("\n")
    seen = 0
    for ln, t in enumerate(text):
        start = 0
        while True:
            c = t.find(a["needle"], start)
            if c < 0:
                break
            if seen == a["occ"]:
                return {"line": ln, "character": u16(t[:c + a["delta"]])}
            seen += 1
            start = c + 1
    raise ValueError("needle not found: %r" % (a,))


def rel(root, uri):
    return os.path.relpath(os.path.realpath(bench_path(uri)), os.path.realpath(root))


def bench_path(uri):
    from urllib.parse import unquote, urlparse
    return unquote(urlparse(uri).path)


def is_err(r):
    return isinstance(r, dict) and "__error__" in r


def text_of(root, f):
    return open(os.path.join(root, f), encoding="utf-8").read()


def flatten_text(x):
    if x is None:
        return ""
    if isinstance(x, str):
        return x
    if isinstance(x, list):
        return "\n".join(flatten_text(i) for i in x)
    if isinstance(x, dict):
        return flatten_text(x.get("value", x.get("contents", "")))
    return str(x)


def lines_of(want, default_file):
    """Normalise [3, 4] or [[f, 3]] to a set of (file, line)."""
    out = set()
    for w in want:
        out.add((default_file, w) if isinstance(w, int) else (w[0], w[1]))
    return out


def pairs(want):
    return {(w[0], w[1]) for w in want}


class Expect:
    """Dict wrapper that records which keys a checker consumed."""

    def __init__(self, d):
        self.d = dict(d)
        self.used = set()

    def has(self, k):
        return k in self.d

    def get(self, k):
        self.used.add(k)
        return self.d[k]

    def leftover(self):
        return sorted(set(self.d) - self.used)


def fail(msg):
    return False, msg


# ------------------------------------------------------------- the checkers
# Each checker: fn(ctx, case, pos, ex) -> (ok, detail). ctx has .s (Server),
# .root, .caps, .case_file. A checker consumes every key it supports.

LOC_KEYS = {"locations", "includes", "includes_any", "excludes", "empty", "files",
            "lines", "lines_include", "excludes_lines", "file"}


def check_locations(ctx, case, r, ex):
    if is_err(r):
        return fail("error response: %s" % (r,))
    got = bench.locs(ctx.s, r)
    fl = ex.get("file") if ex.has("file") else None
    problems = []
    if ex.has("empty"):
        if ex.get("empty") and got:
            problems.append("expected no locations")
    if ex.has("locations") and got != pairs(ex.get("locations")):
        problems.append("locations differ from the exact set")
    if ex.has("includes"):
        miss = pairs(ex.get("includes")) - got
        if miss:
            problems.append("missing %s" % sorted(miss))
    if ex.has("includes_any"):
        if not (pairs(ex.get("includes_any")) & got):
            problems.append("none of includes_any")
    if ex.has("excludes"):
        hit = pairs(ex.get("excludes")) & got
        if hit:
            problems.append("forbidden present %s" % sorted(hit))
    if ex.has("files"):
        files = set(ex.get("files"))
        gf = {f for f, _ in got}
        if not got or not (gf & files) or gf - files:
            problems.append("files %s, wanted only within %s and at least one" % (sorted(gf), sorted(files)))
    if ex.has("lines"):
        want = set(ex.get("lines"))
        gl = {l for f, l in got if f == fl}
        other = {x for x in got if x[0] != fl}
        if gl != want or other:
            problems.append("lines %s in %s (other files %s), wanted exactly %s" % (sorted(gl), fl, sorted(other), sorted(want)))
    if ex.has("lines_include"):
        miss = set(ex.get("lines_include")) - {l for f, l in got if f == fl}
        if miss:
            problems.append("missing lines %s" % sorted(miss))
    if ex.has("excludes_lines"):
        hit = set(ex.get("excludes_lines")) & {l for f, l in got if f == fl}
        if hit:
            problems.append("forbidden lines %s" % sorted(hit))
    return (not problems), ("; ".join(problems) if problems else "") + (" got=%s" % sorted(got))


def do_definition(ctx, case, pos, ex):
    r = ctx.s.call("textDocument/definition", ctx.tdp(pos))
    return check_locations(ctx, case, r, ex)


def do_references(ctx, case, pos, ex):
    ctxp = case["params"].get("context", {"includeDeclaration": True})
    r = ctx.s.call("textDocument/references", {**ctx.tdp(pos), "context": ctxp})
    return check_locations(ctx, case, r, ex)


def do_highlight(ctx, case, pos, ex):
    r = ctx.s.call("textDocument/documentHighlight", ctx.tdp(pos))
    if is_err(r) or r is None:
        return fail("response: %s" % (r,))
    got = {h["range"]["start"]["line"] + 1 for h in r}
    fl = ex.get("file")
    ok, msgs = True, []
    if ex.has("lines") and got != set(ex.get("lines")):
        ok = False
        msgs.append("lines differ from the exact set")
    if ex.has("lines_include") and set(ex.get("lines_include")) - got:
        ok = False
        msgs.append("missing lines")
    return ok, "; ".join(msgs) + " file=%s got=%s" % (fl, sorted(got))


def do_prepare_call(ctx, case, pos, ex):
    r = ctx.s.call("textDocument/prepareCallHierarchy", ctx.tdp(pos))
    if is_err(r) or not isinstance(r, list):
        return fail("response: %s" % (r,))
    names = {i["name"] for i in r}
    return names_check(ex, names, [])


def names_check(ex, names, ordered):
    problems = []
    if ex.has("names") and names != set(ex.get("names")):
        problems.append("names differ from %s" % ex.d["names"])
    if ex.has("names_exact") and names != set(ex.get("names_exact")):
        problems.append("names differ from exact %s" % ex.d["names_exact"])
    if ex.has("names_include") and set(ex.get("names_include")) - names:
        problems.append("missing %s" % sorted(set(ex.d["names_include"]) - names))
    if ex.has("names_exclude") and set(ex.get("names_exclude")) & names:
        problems.append("forbidden %s" % sorted(set(ex.d["names_exclude"]) & names))
    return (not problems), "; ".join(problems) + " got=%s" % sorted(names)


def do_calls(direction):
    def f(ctx, case, pos, ex):
        items = ctx.s.call("textDocument/prepareCallHierarchy", ctx.tdp(pos))
        if is_err(items) or not isinstance(items, list) or not items:
            return fail("prepareCallHierarchy returned no item: %s" % (items,))
        r = ctx.s.call("callHierarchy/" + direction + "Calls", {"item": items[0]})
        if is_err(r):
            return fail("error response: %s" % (r,))
        key = "from" if direction == "incoming" else "to"
        names = {c[key]["name"] for c in (r or [])}
        return names_check(ex, names, [])
    return f


def sym_nodes(r):
    """Flatten DocumentSymbol[] (hierarchical) or SymbolInformation[] (flat)."""
    nodes = []

    def walk(xs, parent):
        for x in xs or []:
            n = {"name": x["name"], "kind": x.get("kind"), "parent": parent,
                 "children": [c["name"] for c in x.get("children") or []]}
            if "location" in x:  # flat
                n["range"] = x["location"]["range"]
                n["selection"] = None
                n["parent"] = x.get("containerName") or None
            else:
                n["range"] = x.get("range")
                n["selection"] = x.get("selectionRange")
            nodes.append(n)
            walk(x.get("children"), x["name"])
    walk(r, None)
    if r and "location" in r[0]:  # flat: rebuild children from containerName
        for n in nodes:
            n["children"] = [m["name"] for m in nodes if m["parent"] == n["name"]]
    return nodes


def rng_within(inner, outer):
    a = (inner["start"]["line"], inner["start"]["character"], inner["end"]["line"], inner["end"]["character"])
    b = (outer["start"]["line"], outer["start"]["character"], outer["end"]["line"], outer["end"]["character"])
    return (b[0], b[1]) <= (a[0], a[1]) and (a[2], a[3]) <= (b[2], b[3])


def do_docsymbol(ctx, case, pos, ex):
    r = ctx.s.call("textDocument/documentSymbol", {"textDocument": {"uri": ctx.s.uri(case["file"])}})
    if is_err(r):
        return fail("error response: %s" % (r,))
    if r is None:
        r = []
    nodes = sym_nodes(r)
    names = {n["name"] for n in nodes}
    ok, detail = names_check(ex, names, [])
    problems = [detail] if not ok else []
    if ex.has("kinds"):
        for nm, k in ex.get("kinds").items():
            ks = [n["kind"] for n in nodes if n["name"] == nm]
            if not ks or any(x != k for x in ks):
                problems.append("kind of %s: %s, wanted %s" % (nm, ks, k))
    if ex.has("children_include"):
        for nm, kids in ex.get("children_include").items():
            have = {c for n in nodes if n["name"] == nm for c in n["children"]}
            if set(kids) - have:
                problems.append("children of %s: %s, missing %s" % (nm, sorted(have), sorted(set(kids) - have)))
    if ex.has("range_contains_lines"):
        for nm, (a, b) in ex.get("range_contains_lines").items():
            if not any(n["name"] == nm and n["range"] and n["range"]["start"]["line"] + 1 <= a
                       and n["range"]["end"]["line"] + 1 >= b for n in nodes):
                problems.append("range of %s does not cover lines %d-%d" % (nm, a, b))
    if ex.has("selection_within_range") and ex.get("selection_within_range"):
        if not nodes:
            problems.append("no symbols")
        for n in nodes:
            if n["selection"] is None:
                problems.append("flat SymbolInformation has no selectionRange")
                break
            if not rng_within(n["selection"], n["range"]):
                problems.append("selectionRange outside range for %s" % n["name"])
    return (not problems), "; ".join(problems) + " names=%s" % sorted(names)


def do_wsymbol(ctx, case, pos, ex):
    r = ctx.s.call("workspace/symbol", {"query": case["params"]["query"]})
    if is_err(r):
        return fail("error response: %s" % (r,))
    r = r or []
    names = {x["name"] for x in r}
    ok, detail = names_check(ex, names, [])
    problems = [detail] if not ok else []
    if ex.has("location_files_include"):
        files = {rel(ctx.root, x["location"]["uri"]) for x in r if x.get("location")}
        miss = set(ex.get("location_files_include")) - files
        if miss:
            problems.append("location files %s, missing %s" % (sorted(files), sorted(miss)))
    return (not problems), "; ".join(problems) + " names=%s" % sorted(names)


def do_hover(ctx, case, pos, ex):
    r = ctx.s.call("textDocument/hover", ctx.tdp(pos))
    if is_err(r) or not r:
        return fail("response: %s" % (r,))
    txt = flatten_text(r.get("contents"))
    miss = [c for c in ex.get("contains") if c not in txt]
    return (not miss), ("missing %s " % miss) + "hover=%r" % txt[:200]


def do_completion(ctx, case, pos, ex):
    r = ctx.s.call("textDocument/completion", ctx.tdp(pos))
    if is_err(r):
        return fail("error response: %s" % (r,))
    items = r.get("items", []) if isinstance(r, dict) else (r or [])
    labels = {i["label"] for i in items}
    miss = set(ex.get("labels_include")) - labels
    return (not miss), "missing %s; %d labels" % (sorted(miss), len(labels))


def edits_of(root, r):
    """WorkspaceEdit -> list of (relfile, line1)."""
    out = []
    for u, es in (r.get("changes") or {}).items():
        for e in es:
            out.append((rel(root, u), e["range"]["start"]["line"] + 1))
    for dc in r.get("documentChanges") or []:
        if "textDocument" in dc:
            for e in dc.get("edits", []):
                out.append((rel(root, dc["textDocument"]["uri"]), e["range"]["start"]["line"] + 1))
    return out


def do_rename(ctx, case, pos, ex):
    r = ctx.s.call("textDocument/rename", {**ctx.tdp(pos), "newName": case["params"]["newName"]})
    if ex.has("null_or_error"):
        ex.get("null_or_error")
        if isinstance(r, dict) and r.get("__error__") == "timeout":
            return fail("timeout")
        ok = r is None or is_err(r)
        return ok, "response=%s" % (json.dumps(r)[:200],)
    if is_err(r) or not isinstance(r, dict):
        return fail("response: %s" % (r,))
    got = edits_of(ctx.root, r)
    gs = set(got)
    fl = ex.get("file") if ex.has("file") else None
    problems = []
    if ex.has("edit_lines") and gs != lines_of(ex.get("edit_lines"), fl):
        problems.append("edits differ from the exact set %s" % sorted(lines_of(ex.d["edit_lines"], fl)))
    if ex.has("edits") and gs != pairs(ex.get("edits")):
        problems.append("edits differ from the exact set %s" % ex.d["edits"])
    if ex.has("excludes_lines"):
        hit = {(fl, l) for l in ex.get("excludes_lines")} & gs
        if hit:
            problems.append("forbidden edits %s" % sorted(hit))
    return (not problems), "; ".join(problems) + " got=%s" % sorted(gs)


def do_prepare_rename(ctx, case, pos, ex):
    r = ctx.s.call("textDocument/prepareRename", ctx.tdp(pos))
    if ex.has("null_or_error"):
        ex.get("null_or_error")
        if isinstance(r, dict) and r.get("__error__") == "timeout":
            return fail("timeout")
        return (r is None or is_err(r)), "response=%s" % (json.dumps(r)[:200],)
    if is_err(r) or not r:
        return fail("response: %s" % (r,))
    rng = r.get("range") if "range" in r else (r if "start" in r else None)
    if rng is None:
        return fail("no range in %s" % (json.dumps(r)[:200],))
    lines = text_of(ctx.root, case["file"]).split("\n")
    if rng["start"]["line"] != rng["end"]["line"]:
        txt = "<multi-line>"
    else:
        raw = lines[rng["start"]["line"]].encode("utf-16-le")
        txt = raw[rng["start"]["character"] * 2:rng["end"]["character"] * 2].decode("utf-16-le")
    return txt == ex.get("prepare_range_text"), "range text %r" % txt


def do_folding(ctx, case, pos, ex):
    r = ctx.s.call("textDocument/foldingRange", {"textDocument": {"uri": ctx.s.uri(case["file"])}})
    if is_err(r) or r is None:
        return fail("response: %s" % (r,))
    starts = {f["startLine"] + 1 for f in r}
    problems = []
    if ex.has("fold_start_lines_include") and set(ex.get("fold_start_lines_include")) - starts:
        problems.append("missing fold starts %s" % sorted(set(ex.d["fold_start_lines_include"]) - starts))
    if ex.has("fold_start_lines_exclude") and set(ex.get("fold_start_lines_exclude")) & starts:
        problems.append("forbidden fold starts %s" % sorted(set(ex.d["fold_start_lines_exclude"]) & starts))
    return (not problems), "; ".join(problems) + " starts=%s" % sorted(starts)


def do_links(ctx, case, pos, ex):
    r = ctx.s.call("textDocument/documentLink", {"textDocument": {"uri": ctx.s.uri(case["file"])}})
    if is_err(r) or r is None:
        return fail("response: %s" % (r,))
    problems = []
    if ex.has("links_include"):
        for w in ex.get("links_include"):
            if not any(l["range"]["start"]["line"] + 1 == w["line"] and (l.get("target") or "").endswith(w["target_suffix"])
                       for l in r):
                problems.append("no link on line %d ending %s" % (w["line"], w["target_suffix"]))
    if ex.has("link_lines_exclude"):
        hit = {l["range"]["start"]["line"] + 1 for l in r} & set(ex.get("link_lines_exclude"))
        if hit:
            problems.append("links on forbidden lines %s" % sorted(hit))
    return (not problems), "; ".join(problems) + " links=%s" % [(l["range"]["start"]["line"] + 1, l.get("target")) for l in r]


def do_selection(ctx, case, pos, ex):
    r = ctx.s.call("textDocument/selectionRange", {"textDocument": {"uri": ctx.s.uri(case["file"])}, "positions": [pos]})
    if is_err(r) or not r:
        return fail("response: %s" % (r,))
    chain, x = [], r[0]
    while x:
        chain.append(x["range"])
        x = x.get("parent")

    def contains(o, i):
        return rng_within(i, o)
    p = {"start": pos, "end": pos}
    problems = []
    if ex.has("all_contain_position") and ex.get("all_contain_position"):
        if not all(contains(c, p) for c in chain):
            problems.append("a range does not contain the position")
    if ex.has("parent_contains_child") and ex.get("parent_contains_child"):
        if not all(contains(chain[i + 1], chain[i]) for i in range(len(chain) - 1)):
            problems.append("a parent does not contain its child")
    return (not problems), "; ".join(problems) + " levels=%d" % len(chain)


def apply_edits(text, edits):
    lines = text.split("\n")
    offs = [0]
    for l in lines:
        offs.append(offs[-1] + len(l) + 1)

    def off(p):
        line = lines[p["line"]] if p["line"] < len(lines) else ""
        ch = len(line.encode("utf-16-le")[:p["character"] * 2].decode("utf-16-le", "ignore"))
        return offs[min(p["line"], len(lines))] + ch
    out = text
    for e in sorted(edits, key=lambda e: (e["range"]["start"]["line"], e["range"]["start"]["character"]), reverse=True):
        out = out[:off(e["range"]["start"])] + e["newText"] + out[off(e["range"]["end"]):]
    return out


def bash_n(path):
    return subprocess.run(["/bin/bash", "-n", path], capture_output=True, timeout=20).returncode


def do_formatting(ctx, case, pos, ex):
    r = ctx.s.call("textDocument/formatting", {"textDocument": {"uri": ctx.s.uri(case["file"])},
                                                "options": case["params"]["options"]})
    if is_err(r):
        return fail("error response: %s" % (r,))
    orig = text_of(ctx.root, case["file"])
    new = apply_edits(orig, r or [])
    problems = []
    if ex.has("preserves_tokens") and ex.get("preserves_tokens") and new.split() != orig.split():
        problems.append("token sequence changed")
    if ex.has("result_bash_n_exit"):
        with tempfile.NamedTemporaryFile("w", suffix=".sh", delete=False) as f:
            f.write(new)
        try:
            code = bash_n(f.name)
        finally:
            os.unlink(f.name)
        if code != ex.get("result_bash_n_exit"):
            problems.append("bash -n of the result exits %d" % code)
    if ex.has("changes_text") and (new != orig) != ex.get("changes_text"):
        problems.append("changes_text mismatch")
    return (not problems), "; ".join(problems) + " edits=%d" % len(r or [])


def do_diagnostics(ctx, case, pos, ex):
    s, f = ctx.s, case["file"]
    uri = s.uri(f)
    if ctx.caps.get("diagnosticProvider"):
        r = s.call("textDocument/diagnostic", {"textDocument": {"uri": uri}}, timeout=10)
        if is_err(r) or not isinstance(r, dict) or not isinstance(r.get("items"), list):
            return fail("pull diagnostics response: %s" % (json.dumps(r)[:200],))
        diags = r["items"]
    else:
        end = time.time() + 8
        while uri not in s.diags and time.time() < end:
            s.pump(0.2)
        if uri not in s.diags:
            diags = None
        else:
            diags = s.diags[uri]
    problems = []
    if ex.has("bash_n_exit"):
        code = bash_n(os.path.join(ctx.root, f))
        if code != ex.get("bash_n_exit"):
            return fail("truth precondition: /bin/bash -n exits %d, expect says %s" % (code, ex.d["bash_n_exit"]))
    if diags is None:
        diags = []
        silent = True
    else:
        silent = False
    errs = [d for d in diags if d.get("severity", 1) == 1]
    if ex.has("max_errors") and len(errs) > ex.get("max_errors"):
        problems.append("%d errors, at most %d allowed" % (len(errs), ex.d["max_errors"]))
    if ex.has("min_errors"):
        if len(errs) < ex.get("min_errors"):
            problems.append("%d errors, at least %d wanted" % (len(errs), ex.d["min_errors"]))
    if ex.has("error_lines_include"):
        have = {d["range"]["start"]["line"] + 1 for d in errs}
        miss = set(ex.get("error_lines_include")) - have
        if miss:
            problems.append("no error on lines %s (error lines %s)" % (sorted(miss), sorted(have)))
    return (not problems), "; ".join(problems) + " diags=%s%s" % (
        [(d["range"]["start"]["line"] + 1, d.get("severity"), d.get("code"), d.get("message", "")[:60]) for d in diags][:8],
        " (server never published)" if silent else "")


HANDLERS = {
    "textDocument/definition": (do_definition, LOC_KEYS),
    "textDocument/references": (do_references, LOC_KEYS),
    "textDocument/documentHighlight": (do_highlight, {"lines", "lines_include", "file"}),
    "textDocument/prepareCallHierarchy": (do_prepare_call, {"names", "names_exact", "names_include"}),
    "callHierarchy/incomingCalls": (do_calls("incoming"), {"names", "names_exact", "names_include"}),
    "callHierarchy/outgoingCalls": (do_calls("outgoing"), {"names", "names_exact", "names_include"}),
    "textDocument/documentSymbol": (do_docsymbol, {"names_include", "names_exclude", "names_exact", "kinds",
                                                   "children_include", "range_contains_lines",
                                                   "selection_within_range"}),
    "workspace/symbol": (do_wsymbol, {"names_include", "names_exact", "location_files_include"}),
    "textDocument/hover": (do_hover, {"contains"}),
    "textDocument/completion": (do_completion, {"labels_include"}),
    "textDocument/rename": (do_rename, {"edit_lines", "edits", "excludes_lines", "file", "null_or_error"}),
    "textDocument/prepareRename": (do_prepare_rename, {"prepare_range_text", "null_or_error"}),
    "textDocument/foldingRange": (do_folding, {"fold_start_lines_include", "fold_start_lines_exclude"}),
    "textDocument/documentLink": (do_links, {"links_include", "link_lines_exclude"}),
    "textDocument/selectionRange": (do_selection, {"all_contain_position", "parent_contains_child"}),
    "textDocument/formatting": (do_formatting, {"preserves_tokens", "result_bash_n_exit", "changes_text"}),
    "textDocument/publishDiagnostics": (do_diagnostics, {"bash_n_exit", "max_errors", "min_errors",
                                                         "error_lines_include"}),
}

# Methods whose case needs the file open before the call. workspace/symbol is
# asked of a server that has seen no document, like bench case C22.
NO_OPEN = {"workspace/symbol"}


class Ctx:
    def __init__(self, s, root, caps):
        self.s, self.root, self.caps = s, root, caps

    def tdp(self, pos):
        return {"textDocument": {"uri": self.s.uri(self.case_file)}, "position": pos}


def validate(cases):
    errors = []
    for c in cases:
        h = HANDLERS.get(c["method"])
        if h is None:
            errors.append("%s: unknown method %s" % (c["id"], c["method"]))
            continue
        for k in c["expect"]:
            if k not in h[1]:
                errors.append("%s: unknown expect key %r for %s" % (c["id"], k, c["method"]))
    return errors


def load_cases():
    cases = []
    for f in sorted(glob.glob(os.path.join(HERE, "suite_cases", "*.json"))):
        cases.extend(json.load(open(f)))
    return cases


def run_case(c, cmd, root):
    t0 = time.time()
    s = None
    try:
        s, init, _ = bench.start(cmd, root)
        if bench.err(init):
            return False, "initialize failed: %s" % (init,)
        caps = (init or {}).get("capabilities", {})
        ctx = Ctx(s, root, caps)
        ctx.case_file = c["file"]
        fn, _keys = HANDLERS[c["method"]]
        s.pump(0.5)
        if c["method"] in NO_OPEN:
            s.pump(SETTLE + 1.0)
        else:
            s.open(c["file"], settle=SETTLE)
        pos = at_position(root, c)
        ex = Expect(c["expect"])
        ok, detail = fn(ctx, c, pos, ex)
        left = ex.leftover()
        if left:
            return False, "unconsumed expect keys %s (runner bug)" % left
        return bool(ok), detail
    except Exception as e:  # a crash or malformed answer fails the case
        return False, "exception: %r" % (e,)
    finally:
        if s is not None:
            try:
                s.p.kill()
            except Exception:
                pass
        _ = t0


def main():
    i = sys.argv.index("--")
    name, cmd = sys.argv[1], sys.argv[i + 1:]
    cases = load_cases()
    errs = validate(cases)
    if errs:
        sys.stderr.write("\n".join(errs) + "\n")
        sys.exit(2)
    only = os.environ.get("SUITE_ONLY")
    if only:
        want = set(only.split(","))
        cases = [c for c in cases if c["id"] in want]
    if "BENCH_CORPUS" not in os.environ:
        os.environ["BENCH_CORPUS"] = os.path.join(os.path.dirname(HERE), "testdata", "corpus")
    out = bench.run(name, cmd) if not only else {"name": name, "caps": {}, "timings": {}, "results": {}}
    root = os.path.realpath(tempfile.mkdtemp(prefix="bash-lsp-corpus2-"))
    shutil.rmtree(root)
    shutil.copytree(os.path.join(HERE, "testdata", "corpus2"), root)
    meta = {}
    try:
        for c in cases:
            t = time.time()
            ok, detail = run_case(c, cmd, root)
            ms = round((time.time() - t) * 1000)
            out["results"][c["id"]] = {"pass": bool(ok), "tier": c["tier"], "category": c["category"],
                                       "method": c["method"], "file": c["file"],
                                       "detail": str(detail)[:600], "ms": ms}
            sys.stderr.write("%s %s %s\n" % (c["id"], "pass" if ok else "FAIL", c["category"]))
    finally:
        shutil.rmtree(root, ignore_errors=True)
    print(json.dumps(out, indent=1))


if __name__ == "__main__":
    main()

"""Run-based cross-check of the `bash-trace` and `manual` expectations (Task 14).

Usage (from the repository root):
  uv run --no-project python bench/oracle_trace.py [OUT.json]      (default bench/results/oracle-trace.json)
  uv run --no-project python bench/oracle_trace.py --selftest      (a wrong expectation must disagree; unsafe scripts must be refused)

What it does. It copies bench/testdata/corpus2 into a fresh temporary directory
and runs each corpus script with a real bash on that copy, under
`env -i PATH=/usr/bin:/bin` (plus two instrumentation variables, see below),
with a DEBUG trap (functrace) that logs, before every command, the file and
line, the command text, the function-call depth and name, and the value of
every variable the script mentions. Which definition a call ran is read from
the frame of the command that follows the call (extdebug is not used: the bash
4.4 and later builds shipped here start a debugger hook on `shopt -s extdebug`
and refuse). It then decides,
from that log, the definition cases (and the references cases that a run can
decide) whose oracle is `bash-trace` or `manual`, and compares the answer with
the case's expectation. Disagreement is reported, never written back: the truth
files are read-only inputs.

Trace guard. Nothing runs unless every command word of the script (and of every
file it sources, and of every string it evals) is a function defined in corpus2
or one of ALLOW. The check is a conservative static lexer; anything it does not
understand makes the file fail. Files that fail are skipped and listed with the
reason. The guard also refuses every redirection except a here-string and a
file-descriptor duplication, backticks, process substitution, here-documents,
`read` that is not fed by a here-string, `eval` of anything but literal strings
(loop variables with literal values are expanded), and `source` of anything that
does not resolve to a file inside the copy.

Defence in depth: when /usr/bin/sandbox-exec works, each run is also confined by
a profile that denies network access and every file write outside the temporary
directory (and /dev/null). `sandboxed` in the result says whether it was used.
The instrumentation (BASH_ENV prelude, ORACLE_LOG, ORACLE_VARS) is the only
addition to the environment and lives in the temporary directory.

No network, no file written outside the temporary directory.
"""
import glob
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
CORPUS2 = os.path.join(HERE, "testdata", "corpus2")
BASH = "/bin/bash"
EXTRA_BASH = sorted(glob.glob("/private/tmp/bash-lsp/bash-builds/*/bash-*/bash"))
ALLOW = {"echo", "printf", ":", "true", "false", "test", "[", "local", "declare", "export", "unset", "return",
         "source", ".", "shift", "set", "read", "eval"}
KW_PASS = {"then", "do", "else", "elif", "if", "while", "until", "!", "{", "time"}
KW_END = {"fi", "done", "}"}
MAX_EXP = 64
TRACE_CAP = 400  # events kept per script in the result file (a source cycle logs thousands)
# Names of the executables env -i PATH=/usr/bin:/bin can reach. A corpus function with one of these names is only
# safe where it is defined at the call, which a static check cannot know, so a script that calls one is refused.
EXEC_NAMES = set()
for _d in ("/usr/bin", "/bin"):
    try:
        EXEC_NAMES |= set(os.listdir(_d))
    except OSError:
        pass

PRELUDE = r'''set -o functrace
__otrace() {
  local __o_c="$1" __o_f="$2" __o_l="$3" __o_fr="$4" __o_v
  printf '%s\t%s\t%s\t%s\t' "$__o_f" "$__o_l" "$__o_c" "$__o_fr" >>"$ORACLE_LOG"
  for __o_v in $ORACLE_VARS; do
    if [ -n "${!__o_v+x}" ]; then printf '%s=%s;' "$__o_v" "${!__o_v}" >>"$ORACLE_LOG"; else printf '%s=<unset>;' "$__o_v" >>"$ORACLE_LOG"; fi
  done
  printf '\n' >>"$ORACLE_LOG"
}
trap '__otrace "$BASH_COMMAND" "${BASH_SOURCE[0]}" "$LINENO" "${#FUNCNAME[@]}:${FUNCNAME[0]}"' DEBUG
'''


# ------------------------------------------------------------------ guard
class Tok:
    __slots__ = ("kind", "text", "line")

    def __init__(self, kind, text="", line=0):
        self.kind, self.text, self.line = kind, text, line


NAME = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")
ASSIGN = re.compile(r"^([A-Za-z_][A-Za-z0-9_]*)=(.*)$", re.S)


def match_close(s, i, op, cl):
    """s[i] is the char after the opener; return index of the matching closer, or -1 (quote aware)."""
    depth = 1
    n = len(s)
    while i < n:
        c = s[i]
        if c == "\\":
            i += 2
            continue
        if c == "'" and op != "{":
            j = s.find("'", i + 1)
            if j < 0:
                return -1
            i = j + 1
            continue
        if c == '"':
            j = i + 1
            while j < n and s[j] != '"':
                j += 2 if s[j] == "\\" else 1
            i = j + 1
            continue
        if c == op:
            depth += 1
        elif c == cl:
            depth -= 1
            if depth == 0:
                return i
        i += 1
    return -1


class Guard:
    def __init__(self, root):
        self.root = os.path.realpath(root)
        self.funcs = set()
        self.reasons = []
        self.seen = set()
        self.cwd = self.root
        self.vars = {}
        self.collect = False

    # ---- lexing
    def lex(self, src, where):
        toks, i, n = [], 0, len(src)
        line = 1
        while i < n:
            c = src[i]
            if c in " \t":
                i += 1
                continue
            if c == "\n":
                toks.append(Tok("nl", line=line))
                line += 1
                i += 1
                continue
            if c == "\\" and src[i + 1:i + 2] == "\n":
                i += 2
                line += 1
                continue
            if c == "#":
                j = src.find("\n", i)
                i = n if j < 0 else j
                continue
            two = src[i:i + 2]
            if two in ("&&", "||", ";;"):
                toks.append(Tok("op", two, line))
                i += 2
                continue
            if two == "|&":
                self.bad(where, line, "operator |&")
                i += 2
                continue
            if c in ";&|":
                toks.append(Tok("op", c, line))
                i += 1
                continue
            if c in "()":
                toks.append(Tok("op", c, line))
                i += 1
                continue
            m = re.match(r"[0-9]*([<>])", src[i:i + 12]) if (c in "<>" or c.isdigit()) else None
            if m and (c in "<>" or src[i:i + 12].lstrip("0123456789")[:1] in ("<", ">")):
                j = i + m.end() - 1  # index of the < or > char
                ch = src[j]
                rest = src[j:j + 3]
                if rest == "<<<":
                    toks.append(Tok("here", line=line))
                    i = j + 3
                    continue
                if rest[:2] in (">&", "<&") and re.match(r"[0-9-]", src[j + 2:j + 3] or "x"):
                    i = j + 2
                    while i < n and (src[i].isdigit() or src[i] == "-"):
                        i += 1
                    continue
                if src[j + 1:j + 2] == "(":
                    self.bad(where, line, "process substitution")
                elif rest[:2] == "<<":
                    self.bad(where, line, "here-document")
                else:
                    self.bad(where, line, "redirection %r" % rest[:2])
                i = j + 1
                continue
            word, i, line = self.read_word(src, i, line, where)
            toks.append(Tok("word", word, line))
        return toks

    def read_word(self, s, i, line, where):
        n = len(s)
        start = i
        while i < n:
            c = s[i]
            if c in " \t\n;&|()<>":
                break
            if c == "\\":
                if s[i + 1:i + 2] == "\n":
                    line += 1
                i += 2
                continue
            if c == "`":
                self.bad(where, line, "backtick")
                i += 1
                continue
            if c == "'":
                j = s.find("'", i + 1)
                if j < 0:
                    i = n
                    break
                line += s.count("\n", i, j)
                i = j + 1
                continue
            if c == '"':
                i += 1
                while i < n and s[i] != '"':
                    if s[i] == "\\":
                        i += 2
                        continue
                    if s[i] == "`":
                        self.bad(where, line, "backtick")
                    if s[i] == "$" and s[i + 1:i + 2] in ("(", "{"):
                        i = self.dollar(s, i, line, where)
                        continue
                    if s[i] == "\n":
                        line += 1
                    i += 1
                i += 1
                continue
            if c == "$":
                if s[i + 1:i + 2] == "'":
                    j = i + 2
                    while j < n and s[j] != "'":
                        j += 2 if s[j] == "\\" else 1
                    i = j + 1
                    continue
                i = self.dollar(s, i, line, where)
                continue
            i += 1
        return s[start:i], i, line

    def dollar(self, s, i, line, where):
        """s[i] == '$'. Handles $(...), $((...)), ${...}; returns the index after it."""
        nxt = s[i + 1:i + 2]
        if nxt == "(" and s[i + 2:i + 3] == "(":
            j = match_close(s, i + 2, "(", ")")
            if j < 0:
                return len(s)
            inner = s[i + 3:j]
            if "$(" in inner or "`" in inner:
                self.bad(where, line, "command substitution inside arithmetic")
            return j + 2 if s[j + 1:j + 2] == ")" else j + 1
        if nxt == "(":
            j = match_close(s, i + 2, "(", ")")
            inner = s[i + 2:] if j < 0 else s[i + 2:j]
            self.check_text(inner, where + "$(...)", sub=True)
            return len(s) if j < 0 else j + 1
        if nxt == "{":
            j = match_close(s, i + 2, "{", "}")
            inner = s[i + 2:] if j < 0 else s[i + 2:j]
            for m in re.finditer(r"\$\(", inner):
                k = match_close(inner, m.end(), "(", ")")
                self.check_text(inner[m.end():] if k < 0 else inner[m.end():k], where + "${..$(...)}", sub=True)
            if "`" in inner:
                self.bad(where, line, "backtick")
            return len(s) if j < 0 else j + 1
        return i + 1

    def bad(self, where, line, why):
        if not self.collect:
            self.reasons.append("%s:%d %s" % (where, line, why))

    # ---- expansion of a word to literal strings
    def expand(self, raw, glob_ok=False):
        if raw == "[":
            return ["["]
        outs = [""]
        i, n = 0, len(raw)
        quoted = None
        globby = False
        while i < n:
            c = raw[i]
            if quoted == "'":
                if c == "'":
                    quoted = None
                else:
                    outs = [o + c for o in outs]
                i += 1
                continue
            if c == "'" and quoted is None:
                quoted = "'"
                i += 1
                continue
            if c == '"':
                quoted = None if quoted == '"' else '"'
                i += 1
                continue
            if c == "\\" and i + 1 < n:
                outs = [o + raw[i + 1] for o in outs]
                i += 2
                continue
            if c == "$":
                m = re.match(r"\$\{0%/\*\}", raw[i:])
                if m:
                    outs = [o + "." for o in outs]
                    i += m.end()
                    continue
                m = re.match(r"\$(?:\{([A-Za-z_][A-Za-z0-9_]*)\}|([A-Za-z_][A-Za-z0-9_]*))", raw[i:])
                if not m:
                    return None
                name = m.group(1) or m.group(2)
                vals = self.vars.get(name)
                if not vals:
                    return None
                outs = [o + v for o in outs for v in sorted(vals)]
                if len(outs) > MAX_EXP:
                    return None
                i += m.end()
                continue
            if c in "*?[" and quoted is None:
                globby = True
            outs = [o + c for o in outs]
            i += 1
        if quoted:
            return None
        if globby:
            if not glob_ok:
                return None
            res = []
            for o in outs:
                hits = sorted(glob.glob(os.path.join(self.cwd, o)))
                if hits:
                    pre = "./" if o.startswith("./") else ""
                    res += [pre + os.path.relpath(h, self.cwd) for h in hits]
                else:
                    res.append(o)
            return res
        return outs

    # ---- parsing
    def check_text(self, src, where, sub=False):
        toks = self.lex(src, where)
        self.parse(toks, where)

    def add_var(self, name, raw):
        vals = self.expand(raw, glob_ok=False)
        if vals is None:
            vals = []
        self.vars.setdefault(name, set()).update(vals)

    def parse(self, toks, where):
        i, n = 0, len(toks)
        cmd_start = True
        cur = None  # {'word':..., 'args':[], 'here':False, 'line':}

        def finish():
            nonlocal cur
            if cur is None:
                return
            w = cur["word"]
            if w == "read" and not cur["here"]:
                self.bad(where, cur["line"], "read not fed by a here-string")
            if w in ("source", "."):
                self.check_source(cur, where)
            if w == "eval":
                self.check_eval(cur, where)
            if w in ("local", "declare", "export"):
                for a in cur["args"]:
                    m = ASSIGN.match(a)
                    if m:
                        self.add_var(m.group(1), m.group(2))
            cur = None

        while i < n:
            t = toks[i]
            if t.kind in ("nl",) or (t.kind == "op" and t.text in (";", "&", "&&", "||", "|", ";;")):
                finish()
                cmd_start = True
                i += 1
                continue
            if t.kind == "op" and t.text == "(":
                finish()
                cmd_start = True
                i += 1
                continue
            if t.kind == "op" and t.text == ")":
                finish()
                cmd_start = True
                i += 1
                continue
            if t.kind == "here":
                if cur is not None:
                    cur["here"] = True
                i += 2  # the here-string word is data (its substitutions were already checked at lex time)
                continue
            # a word
            w = t.text
            if not cmd_start:
                if cur is not None:
                    cur["args"].append(w)
                i += 1
                continue
            if w in KW_PASS:
                i += 1
                continue
            if w in KW_END:
                cmd_start = False
                i += 1
                continue
            if w in ("case", "select", "coproc") or w.startswith("(("):
                self.bad(where, t.line, "unsupported keyword %s" % w)
                i += 1
                continue
            if w == "for":
                var = toks[i + 1].text if i + 1 < n and toks[i + 1].kind == "word" else ""
                if not NAME.match(var):
                    self.bad(where, t.line, "unsupported for form")
                    i += 1
                    continue
                j = i + 2
                if j < n and toks[j].kind == "word" and toks[j].text == "in":
                    j += 1
                    vals = set()
                    ok = True
                    while j < n and toks[j].kind == "word":
                        ex = self.expand(toks[j].text, glob_ok=True)
                        if ex is None:
                            ok = False
                        else:
                            vals.update(ex)
                        j += 1
                    if ok:
                        self.vars.setdefault(var, set()).update(vals)
                i = j
                cmd_start = True
                continue
            if w == "function":
                if i + 1 < n and toks[i + 1].kind == "word":
                    self.funcs.add(toks[i + 1].text)
                i += 2
                continue
            if w == "[[":
                while i < n and not (toks[i].kind == "word" and toks[i].text == "]]"):
                    i += 1
                i += 1
                cmd_start = False
                continue
            m = ASSIGN.match(w)
            if m and not w.startswith("="):
                if m.group(2) == "" and i + 1 < n and toks[i + 1].kind == "op" and toks[i + 1].text == "(":
                    self.bad(where, t.line, "array assignment")
                self.add_var(m.group(1), m.group(2))
                i += 1
                continue
            # function definition: NAME ( )
            if NAME.match(w) and i + 2 < n and toks[i + 1].kind == "op" and toks[i + 1].text == "(" \
                    and toks[i + 2].kind == "op" and toks[i + 2].text == ")":
                self.funcs.add(w)
                i += 3
                cmd_start = True
                continue
            # a command word
            cands = self.expand(w)
            if cands is None:
                self.bad(where, t.line, "command word %r cannot be resolved to a literal" % w)
                cmd_start = False
                cur = {"word": w, "args": [], "here": False, "line": t.line}
                i += 1
                continue
            for cw in cands:
                if cw not in ALLOW and cw not in self.funcs:
                    self.bad(where, t.line, "command word %r is not a corpus function or allowed builtin" % cw)
                elif cw not in ALLOW and cw in EXEC_NAMES:
                    self.bad(where, t.line, "command word %r is a corpus function but also an executable on PATH; "
                             "undefined at the call it would run the executable" % cw)
            cur = {"word": cands[0] if len(cands) == 1 else w, "args": [], "here": False, "line": t.line}
            cmd_start = False
            i += 1
        finish()

    def check_eval(self, cur, where):
        if len(cur["args"]) != 1:
            self.bad(where, cur["line"], "eval of other than one literal string")
            return
        strs = self.expand(cur["args"][0])
        if strs is None:
            self.bad(where, cur["line"], "eval argument is not a literal string")
            return
        for s in strs:
            self.check_text(s, where + ":%d(eval)" % cur["line"], sub=True)

    def check_source(self, cur, where):
        if len(cur["args"]) < 1:
            self.bad(where, cur["line"], "source without a file")
            return
        outs = self.expand(cur["args"][0])
        if outs is None:
            self.bad(where, cur["line"], "source target %r cannot be resolved" % cur["args"][0])
            return
        for o in outs:
            p = os.path.realpath(os.path.join(self.cwd, o))
            if not (p == self.root or p.startswith(self.root + os.sep)) or not os.path.isfile(p):
                self.bad(where, cur["line"], "source target %r is not a file of the copy" % o)
                continue
            self.check_file(p)

    def check_file(self, path):
        path = os.path.realpath(path)
        if path in self.seen:
            return
        self.seen.add(path)
        src = open(path, encoding="utf-8").read()
        self.check_text(src, os.path.relpath(path, self.root))

    def collect_funcs(self):
        """Pass 1: every function defined anywhere in corpus2 (definitions, eval strings included)."""
        self.collect = True
        for p in sorted(glob.glob(os.path.join(self.root, "**", "*.sh"), recursive=True)):
            self.vars = {}
            self.cwd = os.path.dirname(p)
            self.seen = set()
            self.check_file_collect(p)
        self.collect = False

    def check_file_collect(self, p):
        src = open(p, encoding="utf-8").read()
        toks = self.lex(src, p)
        self.parse_collect(toks, p)

    def parse_collect(self, toks, where):
        # run the normal parser with problems suppressed; sources are not followed in pass 1
        saved = self.check_source
        self.check_source = lambda cur, where: None
        try:
            self.parse(toks, where)
        finally:
            self.check_source = saved

    def verdict(self, entry):
        """-> (ok, reasons) for running the file `entry` (absolute path in the copy) from its own directory."""
        self.reasons = []
        self.seen = set()
        self.vars = {}
        self.cwd = os.path.dirname(entry)
        self.check_file(entry)
        return (not self.reasons), list(self.reasons)


# --------------------------------------------------------------- the runs
def sandbox_profile(tmp):
    t = os.path.realpath(tmp)
    return ('(version 1)(allow default)(deny network*)'
            '(deny file-write* (require-not (require-any (subpath "%s") (literal "/dev/null") (literal "/dev/dtracehelper"))))' % t)


def sandbox_works(tmp):
    if not os.path.exists("/usr/bin/sandbox-exec"):
        return False
    try:
        r = subprocess.run(["/usr/bin/sandbox-exec", "-p", sandbox_profile(tmp), "/bin/echo", "ok"],
                           capture_output=True, timeout=20)
        if r.returncode != 0:
            return False
        other = tempfile.mkdtemp(prefix="oracle-probe-")
        try:
            r = subprocess.run(["/usr/bin/sandbox-exec", "-p", sandbox_profile(tmp), "/bin/sh", "-c",
                                "echo x > '%s/p'" % other], capture_output=True, timeout=20)
            return r.returncode != 0 and not os.path.exists(os.path.join(other, "p"))
        finally:
            shutil.rmtree(other, ignore_errors=True)
    except Exception:
        return False


def run_bash(bash, tmp, root, rel, varnames, sandboxed):
    """Run `bash ./name` in the file's directory; return (rc, stdout, stderr, events)."""
    d = os.path.join(root, os.path.dirname(rel))
    log = os.path.join(tmp, "events.log")
    open(log, "w").close()
    env = {"PATH": "/usr/bin:/bin", "BASH_ENV": os.path.join(tmp, "prelude.sh"), "ORACLE_LOG": log,
           "ORACLE_VARS": " ".join(sorted(varnames))}
    cmd = [bash, "./" + os.path.basename(rel)]
    if sandboxed:
        cmd = ["/usr/bin/sandbox-exec", "-p", sandbox_profile(tmp)] + cmd
    try:
        r = subprocess.run(cmd, cwd=d, env=env, capture_output=True, timeout=30, stdin=subprocess.DEVNULL)
        rc, out, err = r.returncode, r.stdout.decode("utf-8", "replace"), r.stderr.decode("utf-8", "replace")
    except subprocess.TimeoutExpired:
        rc, out, err = -1, "", "timeout"
    events = parse_log(open(log, encoding="utf-8", errors="replace").read(), d, root)
    return rc, out, err, events


def parse_log(text, cwd, root):
    """Records start with `file<TAB>line<TAB>cmd<TAB>depth:func<TAB>vars`; cmd may hold newlines."""
    events = []
    for rec in re.split(r"\n(?=[^\t\n]*\t[0-9]+\t)", text.rstrip("\n")):
        parts = rec.split("\t")
        if len(parts) < 5:
            continue
        f, ln = parts[0], parts[1]
        cmd = "\t".join(parts[2:-2])
        frame, vars_s = parts[-2], parts[-1]
        if not ln.isdigit():
            continue
        p = f if os.path.isabs(f) else os.path.join(cwd, f)
        relf = os.path.relpath(os.path.realpath(p), os.path.realpath(root)) if f else ""
        depth, _, fn = frame.partition(":")
        vals = {}
        for kv in vars_s.split(";"):
            if "=" in kv:
                k, v = kv.split("=", 1)
                vals[k] = v
        events.append({"file": relf, "line": int(ln), "cmd": cmd, "depth": int(depth or 0), "fn": fn, "vars": vals,
                       "def": None})
    attach_defs(root, events)
    return events


def command_word(e):
    first = e["cmd"].split(" ", 1)[0].strip("\"'")
    if first.startswith("$"):
        nm = first.lstrip("${").rstrip("}")
        return e["vars"].get(nm)
    return first


def attach_defs(root, events):
    """e['def'] = (file, line, name) of the function the command ran, read off the next event's frame."""
    for k, e in enumerate(events[:-1]):
        w = command_word(e)
        nx = events[k + 1]
        if not w or w in ("source", ".", "eval") or nx["depth"] <= e["depth"] or nx["fn"] != w:
            continue
        loc = None
        t = open(os.path.join(root, nx["file"]), encoding="utf-8").read().split("\n") if nx["file"] else []
        for ln in range(min(nx["line"], len(t)), 0, -1):
            if re.match(r"^\s*(function\s+%s\b|%s\s*\(\))" % (re.escape(w), re.escape(w)), t[ln - 1]):
                loc = (nx["file"], ln)
                break
        if loc is None:
            for e2 in reversed(events[:k]):
                if e2["cmd"].startswith("eval"):
                    loc = (e2["file"], e2["line"])
                    break
        if loc:
            e["def"] = (loc[0], loc[1], w)


# --------------------------------------------------------- reading cases
def load_cases():
    cases = []
    for f in sorted(glob.glob(os.path.join(HERE, "suite_cases", "*.json"))):
        cases.extend(json.load(open(f)))
    return cases


def position(root, case):
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
                return ln + 1, c + a["delta"], t
            seen += 1
            start = c + 1
    return None


def query_kind(line, col):
    """What is at the position: ('var', name), ('call', word) or ('func', name) for a definition line."""
    m = None
    for m in re.finditer(r"\$\{?([A-Za-z_][A-Za-z0-9_]*)", line):
        if m.start() <= col <= m.end():
            return "var", m.group(1)
    for m in re.finditer(r"^\s*([A-Za-z_][A-Za-z0-9_]*)\(\)", line):
        if m.start(1) <= col <= m.end(1):
            return "func", m.group(1)
    for m in re.finditer(r"^\s*(?:local\s+)?([A-Za-z_][A-Za-z0-9_]*)=", line):
        if m.start(1) <= col <= m.end(1):
            return "varname", m.group(1)
    pre = re.compile(r"^(?:.*?(?:&&|\|\||[;|({]|\bthen\b|\bdo\b|\belse\b|\beval\s+[\"']))?\s*(?:[A-Za-z_][A-Za-z0-9_]*=\S*\s+)*$")
    for w in re.finditer(r"[A-Za-z_][A-Za-z0-9_]*", line):
        if w.start() <= col <= w.end() and pre.match(line[:w.start()].rstrip('"')) and \
                not line[w.end():w.end() + 1] == "=":
            return "call", w.group(0)
    return None, None


def mentioned_vars(paths):
    names = set()
    for p in paths:
        t = open(p, encoding="utf-8").read()
        names.update(re.findall(r"\$\{?([A-Za-z_][A-Za-z0-9_]*)", t))
        names.update(re.findall(r"(?:^|[\s;&|(])(?:local\s+|declare\s+|export\s+)?([A-Za-z_][A-Za-z0-9_]*)=", t, re.M))
    return names - {"PATH", "HOME", "IFS", "BASH_SOURCE", "PWD", "FUNCNAME", "LINENO", "BASH_COMMAND"}


def literal_value(root, f, ln, name):
    t = open(os.path.join(root, f), encoding="utf-8").read().split("\n")
    if ln > len(t):
        return None
    m = re.search(r"(?:^|[\s;(])(?:local\s+|declare\s+|export\s+)?%s=(\S*)" % re.escape(name), t[ln - 1])
    if not m:
        return None
    v = m.group(1).rstrip(";)")
    if "$" in v or "`" in v:
        return None
    return v.strip("'\"")


def textual_def(root, f, ln, name):
    t = open(os.path.join(root, f), encoding="utf-8").read().split("\n")
    return ln <= len(t) and re.match(r"^\s*(function\s+%s\b|%s\s*\(\))" % (re.escape(name), re.escape(name)), t[ln - 1]) is not None


def observed_defs_for_site(root, events, f, ln, word):
    """Definition locations a call site actually ran, and how many times it ran / was undefined."""
    defs, undefined, hits = set(), 0, 0
    for e in events:
        if e["file"] != f or e["line"] != ln or command_word(e) != word:
            continue
        hits += 1
        if e["def"] is None:
            undefined += 1
        else:
            defs.add((e["def"][0], e["def"][1]))
    return defs, undefined, hits


def pairs(want):
    return {(w[0], w[1]) for w in want}


def compare_locations(ex, defs, undefined):
    """-> (agree, why). ex is the case's expect; defs the observed definition set."""
    problems = []
    if ex.get("empty") and defs:
        problems.append("expected no definition, the run found %s" % sorted(defs))
    if "locations" in ex and defs != pairs(ex["locations"]):
        problems.append("expected exactly %s, the run found %s%s" % (sorted(pairs(ex["locations"])), sorted(defs),
                                                                     " (and an undefined call)" if undefined else ""))
    if "includes" in ex and pairs(ex["includes"]) - defs:
        problems.append("expected to include %s, the run found %s" % (sorted(pairs(ex["includes"]) - defs), sorted(defs)))
    if "includes_any" in ex and not (pairs(ex["includes_any"]) & defs):
        problems.append("expected one of %s, the run found %s" % (sorted(pairs(ex["includes_any"])), sorted(defs)))
    if "excludes" in ex and pairs(ex["excludes"]) & defs:
        problems.append("expected to exclude %s" % sorted(pairs(ex["excludes"]) & defs))
    return (not problems), "; ".join(problems)


def decide(case, root, events, ran):
    """Decide one case from the run. -> dict(status, detail, observed)."""
    ex = case["expect"]
    pos = position(root, case)
    if pos is None:
        return {"status": "undecided", "detail": "needle not found"}
    ln, col, text = pos
    kind, name = query_kind(text, col)
    method = case["method"]
    f = case["file"]
    if method == "textDocument/definition":
        if kind == "call":
            defs, undef, hits = observed_defs_for_site(root, events, f, ln, name)
            if hits == 0:
                return {"status": "undecided", "detail": "the call site at %s:%d never ran" % (f, ln)}
            ok, why = compare_locations(ex, defs, undef)
            return {"status": "agree" if ok else "disagree", "detail": why or "run matches",
                    "observed": {"kind": "call", "word": name, "definitions": sorted(defs), "undefined_runs": undef,
                                 "runs": hits}}
        if kind == "var":
            vals = []
            for e in events:
                if e["file"] == f and e["line"] == ln and name in e["vars"] and re.search(r"\$\{?%s\b" % name, e["cmd"]):
                    vals.append(e["vars"][name])
            if not vals:
                return {"status": "undecided", "detail": "no run of the read of %s at %s:%d" % (name, f, ln)}
            obs = set(vals)
            exp_vals, bad = set(), False
            for loc in ex.get("locations", []):
                v = literal_value(root, loc[0], loc[1], name)
                if v is None:
                    bad = True
                exp_vals.add(v)
            if ex.get("empty"):
                ok = obs <= {"<unset>", ""}
                return {"status": "agree" if ok else "disagree",
                        "detail": "run saw %s of %s; expected no binding" % (sorted(obs), name) if not ok else "run matches",
                        "observed": {"kind": "var", "name": name, "values": sorted(obs)}}
            if "locations" in ex and not bad:
                ok = obs == exp_vals
                return {"status": "agree" if ok else "disagree",
                        "detail": "run saw values %s, the expected locations assign %s" % (sorted(obs), sorted(exp_vals)),
                        "observed": {"kind": "var", "name": name, "values": sorted(obs)}}
            return {"status": "undecided", "detail": "expected assignment is not a literal or the shape is not decidable"}
    if method == "textDocument/references" and kind in ("func", "var", "varname"):
        lines = set()
        if kind == "func":
            dl = (f, ln)
            for e in events:
                d = e["def"]
                if d and (d[0], d[1]) == dl and d[2] == name:
                    lines.add((e["file"], e["line"]))
            ran_lines = lines
            lines = lines | {dl}
        else:
            ran_lines = set()
            for e in events:
                if e["file"] == f and (re.search(r"\$\{?%s\b" % name, e["cmd"]) or re.search(r"(^|\s)(local\s+)?%s=" % name, e["cmd"])):
                    ran_lines.add((e["file"], e["line"]))
            lines = ran_lines
        problems = []
        if "includes" in ex and pairs(ex["includes"]) - lines:
            problems.append("missing %s" % sorted(pairs(ex["includes"]) - lines))
        if "includes_any" in ex and not (pairs(ex["includes_any"]) & lines):
            problems.append("none of %s ran" % sorted(pairs(ex["includes_any"])))
        if "excludes" in ex and pairs(ex["excludes"]) & lines:
            problems.append("forbidden %s ran" % sorted(pairs(ex["excludes"]) & lines))
        if "locations" in ex or "lines" in ex or "empty" in ex:
            return {"status": "undecided", "detail": "an exact reference set is not decidable from a run"}
        return {"status": "agree" if not problems else "disagree", "detail": "; ".join(problems) or "run matches",
                "observed": {"kind": "references", "lines": sorted(lines)}}
    return {"status": "undecided", "detail": "%s at this position is not something a run decides" % method}


def selftest():
    """The oracle must be able to disagree and the guard must be able to refuse. Exit 1 on failure."""
    import copy
    tmp = os.path.realpath(tempfile.mkdtemp(prefix="oracle-selftest-"))
    root = os.path.join(tmp, "corpus2")
    shutil.copytree(CORPUS2, root)
    open(os.path.join(tmp, "prelude.sh"), "w").write(PRELUDE)
    bad = []
    try:
        cases = {c["id"]: c for c in load_cases()}
        wrong = {"S001": {"locations": [["defs/basic.sh", 8]]}, "S031": {"locations": [["scope/dynamic.sh", 6]]},
                 "S057": {"locations": [["source/lib/early.sh", 2]]}, "S044": {"locations": [["scope/s5.sh", 2]]},
                 "S150": {"includes": [["err/ref_swallow.sh", 5]]}}
        vars_ = mentioned_vars(glob.glob(os.path.join(root, "**", "*.sh"), recursive=True))
        for cid, ex in wrong.items():
            c = copy.deepcopy(cases[cid])
            c["expect"] = ex
            ev = run_bash(BASH, tmp, root, c["file"], vars_, False)[3]
            st = decide(c, root, ev, None)["status"]
            if st != "disagree":
                bad.append("%s with a wrong expectation gave %s" % (cid, st))
        os.makedirs(os.path.join(root, "t"), exist_ok=True)
        for name, body in (("rm.sh", "rm -rf x\n"), ("redir.sh", "echo a > f\n"), ("sub.sh", "echo $(date)\n"),
                           ("ev.sh", "eval \"$1\"\n"), ("src.sh", "source /etc/profile\n"),
                           ("pipe.sh", "echo a | read x\n"), ("bt.sh", "echo `id`\n"),
                           ("shadow.sh", "sum() { echo s; }\nsum\n")):
            open(os.path.join(root, "t", name), "w").write(body)
            g = Guard(root)
            g.collect_funcs()
            ok, _ = g.verdict(os.path.join(root, "t", name))
            if ok:
                bad.append("guard accepted %s" % name)
        print("selftest:", "ok" if not bad else bad)
    finally:
        shutil.rmtree(tmp, ignore_errors=True)
    sys.exit(1 if bad else 0)


def main():
    if "--selftest" in sys.argv:
        selftest()
    out_path = sys.argv[1] if len(sys.argv) > 1 else os.path.join(HERE, "results", "oracle-trace.json")
    tmp = os.path.realpath(tempfile.mkdtemp(prefix="oracle-trace-"))
    root = os.path.join(tmp, "corpus2")
    shutil.copytree(CORPUS2, root)
    open(os.path.join(tmp, "prelude.sh"), "w").write(PRELUDE)
    try:
        sandboxed = sandbox_works(tmp)
        g = Guard(root)
        g.collect_funcs()
        cases = [c for c in load_cases() if c["oracle"] in ("bash-trace", "manual")]
        files = sorted({c["file"] for c in cases})
        guard = {}
        for f in files:
            ok, why = g.verdict(os.path.join(root, f))
            guard[f] = {"ok": ok, "reasons": why[:8], "closure": sorted(os.path.relpath(x, root) for x in g.seen)}
        bashes = [BASH] + EXTRA_BASH
        runs = {}
        for b in bashes:
            ver = subprocess.run([b, "--version"], capture_output=True, env={"PATH": "/usr/bin:/bin"}).stdout.decode().split("\n")[0]
            runs[b] = {"version": ver, "files": {}}
            for f in files:
                if not guard[f]["ok"]:
                    continue
                rc, so, se, ev = run_bash(b, tmp, root, f, mentioned_vars([os.path.join(root, x) for x in guard[f]["closure"]]), sandboxed)
                runs[b]["files"][f] = {"rc": rc, "stdout": so[:300], "stderr": se[:300], "events": ev}
        result_cases = {}
        for c in cases:
            r = {"oracle": c["oracle"], "file": c["file"], "method": c["method"], "expect": c["expect"]}
            if not guard[c["file"]]["ok"]:
                r.update(status="skipped", detail="guard: " + "; ".join(guard[c["file"]]["reasons"][:3]))
                r["per_bash"] = {}
            else:
                per = {}
                for b in bashes:
                    fr = runs[b]["files"][c["file"]]
                    per[runs[b]["version"]] = decide(c, root, fr["events"], fr)
                    per[runs[b]["version"]]["rc"] = fr["rc"]
                primary = per[runs[BASH]["version"]]
                r.update(status=primary["status"], detail=primary["detail"], observed=primary.get("observed"))
                r["per_bash"] = {v: {"status": p["status"], "detail": p["detail"]} for v, p in per.items()}
            result_cases[c["id"]] = r
        counts = {}
        for r in result_cases.values():
            counts[r["status"]] = counts.get(r["status"], 0) + 1
        by_bash = {}
        for v in [runs[b]["version"] for b in bashes]:
            d = {}
            for r in result_cases.values():
                s = r["per_bash"].get(v, {}).get("status")
                if s:
                    d[s] = d.get(s, 0) + 1
            by_bash[v] = d
        out = {
            "bash": runs[BASH]["version"],
            "env": "env -i PATH=/usr/bin:/bin (+ BASH_ENV, ORACLE_LOG, ORACLE_VARS: instrumentation)",
            "sandboxed": sandboxed,
            "extra_builds": [runs[b]["version"] for b in EXTRA_BASH],
            "guard": guard,
            "counts": counts,
            "counts_by_bash": by_bash,
            "cases": result_cases,
            "traces": {f: [{**e, "vars": {k: v for k, v in e["vars"].items() if v != "<unset>"}}
                           for e in runs[BASH]["files"][f]["events"][:TRACE_CAP]] for f in runs[BASH]["files"]},
            "traces_truncated": {f: len(runs[BASH]["files"][f]["events"]) for f in runs[BASH]["files"]
                                 if len(runs[BASH]["files"][f]["events"]) > TRACE_CAP},
        }
        os.makedirs(os.path.dirname(os.path.abspath(out_path)), exist_ok=True)
        with open(out_path, "w") as fh:
            json.dump(out, fh, indent=1, sort_keys=True)
        print(json.dumps({"counts": counts, "sandboxed": sandboxed, "by_bash": by_bash}, indent=1))
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


if __name__ == "__main__":
    main()

"""Candidate (a) taken to its limit: a context-free bound that cannot under-count.
Every byte that can open a level of parser recursion is charged the most stack
that byte costs in any context (section 2 of bench/results/nesting-spike-m2a.txt),
closers are ignored (a closer may sit in a quote, comment or heredoc), and the
file is refused when the sum passes the budget. Prints how many of the corpus
files of bench/refusals.py, and which synthetic valid files, it would refuse.
Usage: uv run --no-project python contextfree.py ROOT..."""
import os, re, sys
sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".."))
COST = {b"(": 4002, b"[": 4640, b"{": 1140, b"?": 624, b"!": 596, b"=": 176, b"&": 450, b"*": 177, b"-": 129, b"\n": 128}
WORDS = {rb"\b(if|while|until|for|select|case|elif)\b": 1241, rb"\b(time|coproc|function)\b": 883}
BUDGETS = {"512 MiB (the default limit)": 512 << 20, "64 MiB (the test limit)": 64 << 20}
EXT = (".sh", ".bash", ".bats", ".ksh")
SHEBANG = re.compile(rb"^#![^\n]*\b(sh|bash|dash|ksh|mksh|bats)\b")

def bound(src):
    return sum(src.count(c) * w for c, w in COST.items()) + sum(len(re.findall(p, src)) * w for p, w in WORDS.items())

files = []
for root in sys.argv[1:]:
    for d, dirs, names in os.walk(root):
        dirs[:] = sorted(x for x in dirs if x != ".git")
        for n in sorted(names):
            p = os.path.join(d, n)
            try:
                if os.path.islink(p) or not os.path.isfile(p) or os.path.getsize(p) > 2 << 20:
                    continue
                src = open(p, "rb").read()
            except OSError:
                continue
            if n.endswith(EXT) or SHEBANG.match(src[:128]):
                files.append((bound(src), len(src)))
files.sort(reverse=True)
print(f"corpus files: {len(files)}; largest bound {files[0][0] / (1 << 20):.1f} MiB (a file of {files[0][1]} bytes)")
for name, b in BUDGETS.items():
    print(f"  refused under {name}: {sum(x > b for x, _ in files)}")
synthetic = {
    "500 KB heredoc of (((((": b"cat <<EOF\n" + b"(((((\n" * (500000 // 6) + b"EOF\n",
    "fp.py heredoc_brackets": b"cat <<EOF\n" + b"((((([[[[{{{{ $x\n" * 5000 + b"EOF\n",
    "fp.py comment_smileys": b"# :( :( :( :(\n" * 6000,
    "bench/gen_comments.py shape, 24,000 comment lines": b"# comment line number 12345 with some text to make it realistic\n" * 24000,
    "2 MiB of `x=$(( (a+b) * (c-d) ))` lines": b"x=$(( (a+b) * (c-d) ))\n" * (2097152 // 23),
    "2 MiB of `if [[ -n $a && $b != x ]]; then f \"$(g)\"; fi` lines": b"if [[ -n $a && $b != x ]]; then f \"$(g)\"; fi\n" * (2097152 // 47),
}
for name, src in synthetic.items():
    b = bound(src)
    print(f"{name}: {len(src)} bytes, bound {b / (1 << 20):.0f} MiB, " + ", ".join(f"{'refused' if b > lim else 'accepted'} under {n.split()[0]} MiB" for n, lim in BUDGETS.items()))

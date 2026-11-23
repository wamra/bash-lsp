# bench

The 28-case LSP benchmark and the scale tests, copied from `bash-lsp-evidence`
(2026-11-22) so bash-lsp can be measured the same way as the servers in the
spec's section 0.7. Every script prints JSON on stdout. Python only through
`uv run --no-project python`.

| File | What it does |
|---|---|
| `bench.py NAME -- CMD...` | Runs the 28 cases against a copy of the corpus; prints `{name, caps, timings, results}`. `BENCH_CORPUS` is the absolute path of the corpus directory: `testdata/corpus`, or `testdata/corpus-annotated` (a copy of `bash-lsp-evidence/corpus-annotated`, with `# shellcheck source=` directives, copied into the repo so the `*-ann.json` results can be reproduced). `BENCH_STDERR` is a file that receives the server's stderr. |
| `scale.py NAME BIGFILE -- CMD...` | One large script: didOpen to first documentSymbol, edit to refreshed documentSymbol, definition, RSS. |
| `scale_ws.py NAME WSDIR -- CMD...` | Workspace scale: didOpen of a file in the middle of a source chain, then two kinds of edit timed separately to a refreshed cross-file definition, six samples each: body edits (the source graph is untouched) and edits that add or remove a `source` line (visibility is recomputed). Also the time from start to the first workspace answer. |
| `gen_big.py OUT` | Writes the large script: 2,000 functions, 560,711 bytes (about 561 KB). Each function has a doc comment, a `local`, an `if [[ ]]` with a pipeline, a `for` loop and a `case`, and every function is called by the next one. |
| `gen_comments.py OUTDIR [--decl]` | Writes `c.sh`, 24,000 comment lines (1,860,930 bytes), the input for timing `ast --json`. With `--decl`, a function sits directly under the block (1,860,953 bytes): the input for timing doc comments (`repeat-symbols-comments-decl-*`). |
| `repeat.py OUT LABEL N [--wall] -- CMD...` / `summarize.py FILE KEY...` | Run a measurement N times and keep every raw sample in `results/repeat-*.json` with date, commit and machine; recompute min, median, p95, max. Every number in `docs/m1-acceptance.md` comes from such a file. |
| `gen_workspace.py OUTDIR [FILES=1000] [FUNCS=20]` | Writes the workspace: file `i` does `source ./s{i-1}.sh`, and function `i_k` calls function `(i-1)_k`, so every call crosses a file and the last file sees the whole chain. |
| `report.py` | Builds `results/REPORT.md` from `results/*.json` (run it from this directory). |
| `refusals.py BINARY [--cap N] [--all ROOT]... ROOT...` | Corpus check for the nesting guard: collects shell files under each root (read-only, by extension or shebang, at most 2 MiB each), runs `BINARY ast` on each and prints the files refused with "nesting too deep, or comment block too long, to parse safely". `results/refusals-m2a.json` is its output for `--all testdata /usr /opt/homebrew ~/dev`. |
| `testdata/nesting-spike/` | The scratch programs behind `docs/m2a-nesting-design.md` (stack frames and bytes per nesting level of the mvdan parser, the cost of the runtime's stack measurements, a context-free bound tried on the corpus). Their output is `results/nesting-spike-m2a.txt`. In a `testdata` directory so that the Go tooling and `boundary_test.go` leave them alone. |
| `results/` | Raw results. `bls`, `shuck`, `bashd`, `*-ann` for those three, `mvdan` and `scale-bls/shuck/bashd` are copies of the evidence kit's results. `bash-lsp.json` is `bench.py` on `testdata/corpus`, `bash-lsp-ann.json` is `bench.py` with `BENCH_CORPUS` set to `testdata/corpus-annotated`, `scale-bash-lsp.json` and `scale-ws-bash-lsp.json` are single runs of `scale.py` and `scale_ws.py`; `repeat-*.json`, `go-benchmarks.txt` and `profile-setoverlay-big.txt` are the repeated measurements and profile behind the acceptance document. The `*-after-12b` files (`repeat-big-file`, `repeat-workspace-edits`, `repeat-symbols-cli`, `go-benchmarks`, `profile-setoverlay-big`, `bash-lsp`) are the same measurements after the 12b performance fixes; the files without the suffix stay as the before record. `go-benchmarks-before-12b.txt`, `profile-graphedit-before-12b.txt` and `profile-graphedit-after-12b.txt` hold the graph-edit benchmark before (a worktree of b5488aa with only that benchmark function added) and after. The final numbers were all taken after the harness I/O change below. The `*-m2a-task1` files (`bash-lsp`, `repeat-big-file`, `repeat-workspace-edits`) are the same measurements after the nesting guard replaced the pre-scan (milestone 2a task 1), `fuzz-m2a-task1.txt` holds the result lines of the `FuzzNestingGuard` runs, `guard-cost-m2a-task1.txt` is `BenchmarkGuard` in `internal/front` (the parser with and without the guard), and `nesting-spike-m2a.txt` and `refusals-m2a.json` are described above. The `*-m2a-task1-fix1` files are from the fix round of that task (parses no longer judged by each other's stack): `bash-lsp`, `repeat-big-file` and `repeat-workspace-edits` as before, `guard-cost` is `BenchmarkGuard` again, `refusals` is `refusals.py` again (0 of 1,014 refused), and `concurrency-m2a-task1-fix1.txt` holds the test output behind the design note's "Alone" section. `bash-lsp-m2a-task2.json` is `bench.py` after the public entry point (milestone 2a task 2): the same 20 passes and the same eight failures as `bash-lsp.json`, case by case. `bash-lsp-m2a-task3.json` and `repeat-workspace-edits-m2a-task3.json` are `bench.py` and the 30-run `scale_ws.py` repeat after scope-aware variable binding (milestone 2a task 3): the same 20 passes and eight failures as `bash-lsp.json`, case by case (C04, C08 and C25 pass); workspace-edit median 36.5 ms (body edits) and 35.1 ms (graph edits). The `*-after-final-fixes` files (`bash-lsp`, `repeat-big-file`, `repeat-workspace-edits`, `repeat-symbols-cli`, `repeat-symbols-comments-decl`) are the same measurements after the whole-branch review fixes; `repeat-symbols-comments-decl-before-final-fixes.json` is the binary from 4f8e524 on the same input. |

Local divergence from the evidence copy of `bench.py`: the server pipes are
unbuffered. With a buffered reader a notification and a reply arriving in one
read left the reply invisible to `select()` and a call stalled for its whole
timeout. No case expectation was changed.

## Run

```sh
go build -o /private/tmp/bash-lsp/bash-lsp ./cmd/bash-lsp
export BENCH_CORPUS=$PWD/testdata/corpus
uv run --no-project python bench/bench.py "bash-lsp 0.1.0" -- /private/tmp/bash-lsp/bash-lsp serve > bench/results/bash-lsp.json

uv run --no-project python bench/gen_big.py /private/tmp/bash-lsp/big.sh
uv run --no-project python bench/scale.py "bash-lsp 0.1.0" /private/tmp/bash-lsp/big.sh -- /private/tmp/bash-lsp/bash-lsp serve

uv run --no-project python bench/gen_workspace.py /private/tmp/bash-lsp/ws1000
uv run --no-project python bench/scale_ws.py "bash-lsp 0.1.0" /private/tmp/bash-lsp/ws1000 -- /private/tmp/bash-lsp/bash-lsp serve
/usr/bin/time -p /private/tmp/bash-lsp/bash-lsp symbols /private/tmp/bash-lsp/ws1000 --json > /dev/null
```

`serve` is also the default with no arguments. The first launch of a freshly
built binary on macOS is slower (the system scans a new executable); launch it
once before measuring.

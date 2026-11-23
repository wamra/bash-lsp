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
| `refusals.py BINARY [--cap N] [--all ROOT]... ROOT...` | Corpus check for the parser's nesting limit: collects shell files under each root (read-only, by extension or shebang, at most 2 MiB each), runs `BINARY ast --json` on each and prints the files with the error "nesting is deeper than 10000 levels": skipped because it left no statement (`refused`) or analysed around it (`nesting_errors`), and the files `ast` will not print (`too_deep_to_print`). `results/refusals-m2a-task1-fix2.json` is its output for `--all testdata /usr /opt/homebrew ~/dev`; `refusals-m2a.json` and `refusals-m2a-task1-fix1.json` are from the earlier version of the script, which looked for the message of the guard of that time. |
| `testdata/nesting-spike/` | The scratch programs behind `docs/m2a-nesting-design.md` (stack frames and bytes per nesting level of the mvdan parser at v3.14.1, the cost of the runtime's stack measurements, a context-free bound tried on the corpus). Their output is `results/nesting-spike-m2a.txt`. In a `testdata` directory so that the Go tooling and `boundary_test.go` leave them alone. |
| `results/` | Raw results. `bls`, `shuck`, `bashd`, `*-ann` for those three, `mvdan` and `scale-bls/shuck/bashd` are copies of the evidence kit's results. `bash-lsp.json` is `bench.py` on `testdata/corpus`, `bash-lsp-ann.json` is `bench.py` with `BENCH_CORPUS` set to `testdata/corpus-annotated`, `scale-bash-lsp.json` and `scale-ws-bash-lsp.json` are single runs of `scale.py` and `scale_ws.py`; `repeat-*.json`, `go-benchmarks.txt` and `profile-setoverlay-big.txt` are the repeated measurements and profile behind the acceptance document. The `*-after-12b` files (`repeat-big-file`, `repeat-workspace-edits`, `repeat-symbols-cli`, `go-benchmarks`, `profile-setoverlay-big`, `bash-lsp`) are the same measurements after the 12b performance fixes; the files without the suffix stay as the before record. `go-benchmarks-before-12b.txt`, `profile-graphedit-before-12b.txt` and `profile-graphedit-after-12b.txt` hold the graph-edit benchmark before (a worktree of b5488aa with only that benchmark function added) and after. The final numbers were all taken after the harness I/O change below. The `*-m2a-task1` files (`bash-lsp`, `repeat-big-file`, `repeat-workspace-edits`) are the same measurements after the nesting guard replaced the pre-scan (milestone 2a task 1), `fuzz-m2a-task1.txt` holds the result lines of the `FuzzNestingGuard` runs, `guard-cost-m2a-task1.txt` is `BenchmarkGuard` in `internal/front` (the parser with and without the guard), and `nesting-spike-m2a.txt` and `refusals-m2a.json` are described above. `gen_arith.py OUT` writes a second large file, 552 KB, in the shape of `gen_big.py` with one or two arithmetic constructs per function (about one per 9 lines: `(( ))`, `$(( ))`, C-style `for`, `let`, a subscript and a `${v:off:len}`); `gen_big.py` itself has none. `repeat-arith-file-m2a-task4-fix1.json` is the 10-run `scale.py` repeat on it: open 107 ms and edit 99 ms median. The same file on the binary from before arithmetic support (b703ddd) opens in about 103 ms and edits in about 99 ms (six single runs, not committed), so the tokenizer and the extra symbols cost about 4 ms of the open; the file shape itself sits at the 100 ms line. `bash-lsp-m2a-task4-fix1.json` and `repeat-big-file-m2a-task4-fix1.json` repeat the 28 cases (same as `bash-lsp.json`) and the big file (open 92 ms, edit 85 ms) after the fix round. |

The `*-m2a` files (`bash-lsp-m2a.json`, `repeat-big-file-m2a.json`, `repeat-workspace-edits-m2a.json`, `repeat-arith-file-m2a.json`, `repro-c1-m2a.json`) are the milestone 2a acceptance measurements at cb830e6, and `resync-cost-m2a-task5.txt` and `fuzz-m2a-task5.txt` the Task 5 resync re-measurement and fuzz runs; `docs/m2a-acceptance.md` cites each. `repro-c1/run_all.py` runs every generator of `repro-c1/` against a binary.

The `*-m2a-final` files are the same measurements after the final review's fix
wave, on a binary built from 43196e8 (the last commit that changes non-test
code; c7df9ed after it changes only fuzz budgets in tests):
`bash-lsp-m2a-final.json` (28 cases), `repeat-big-file-m2a-final.json` and
`repeat-arith-file-m2a-final.json` (10 runs of `scale.py` each),
`repeat-workspace-edits-m2a-final.json` (30 runs of `scale_ws.py`),
`repro-c1-m2a-final.json` (`repro-c1/run_all.py`), `fuzz-m2a-final.txt`
(5-minute `FuzzBind` and `FuzzParse` with their per-input time budgets) and
`crash-suite-m2a-final.txt` (`TestNoCrashSubprocess -v`, each case parsed,
walked, bound and built). `lowerer-depth-m2a-final.txt` is the output of
`TestLowererLimitPerConstruct`: the first depth at which the lowerer drops a
statement, per construct. `docs/m2a-acceptance.md` cites each.

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

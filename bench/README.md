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
| `gen_comments.py OUTDIR` | Writes `c.sh`, 24,000 comment lines (1,860,930 bytes), the input for timing `ast --json`. |
| `repeat.py OUT LABEL N [--wall] -- CMD...` / `summarize.py FILE KEY...` | Run a measurement N times and keep every raw sample in `results/repeat-*.json` with date, commit and machine; recompute min, median, p95, max. Every number in `docs/m1-acceptance.md` comes from such a file. |
| `gen_workspace.py OUTDIR [FILES=1000] [FUNCS=20]` | Writes the workspace: file `i` does `source ./s{i-1}.sh`, and function `i_k` calls function `(i-1)_k`, so every call crosses a file and the last file sees the whole chain. |
| `report.py` | Builds `results/REPORT.md` from `results/*.json` (run it from this directory). |
| `results/` | Raw results. `bls`, `shuck`, `bashd`, `*-ann` for those three, `mvdan` and `scale-bls/shuck/bashd` are copies of the evidence kit's results. `bash-lsp.json` is `bench.py` on `testdata/corpus`, `bash-lsp-ann.json` is `bench.py` with `BENCH_CORPUS` set to `testdata/corpus-annotated`, `scale-bash-lsp.json` and `scale-ws-bash-lsp.json` are single runs of `scale.py` and `scale_ws.py`; `repeat-*.json`, `go-benchmarks.txt` and `profile-setoverlay-big.txt` are the repeated measurements and profile behind the acceptance document. The final numbers were all taken after the harness I/O change below. |

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

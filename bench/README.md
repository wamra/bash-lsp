# bench

The benchmarks and scale tests behind [docs/benchmark.md](../docs/benchmark.md).
Every script prints JSON on stdout. Python runs only through
`uv run --no-project python`.

| File | What it does |
|---|---|
| `bench.py NAME -- CMD...` | Runs the 28 original cases against a copy of the corpus; prints `{name, caps, timings, results}`. `BENCH_CORPUS` is the absolute path of the corpus directory: `testdata/corpus`, or `testdata/corpus-annotated` (the same with `# shellcheck source=` directives). `BENCH_STDERR` is a file that receives the server's stderr. |
| `suite.py NAME -- CMD...` | The 28 cases of `bench.py` plus the truth-file suite (`suite_cases/*.json` over `testdata/corpus2`, one fresh server per case); prints `{name, caps, timings, results}` with the suite cases as `{pass, tier, category, ...}`. Needs `BENCH_CORPUS` (absolute) for the 28 cases and, for bash-lsp, `BASH_LSP_CONFIG` set to the absolute path of `config-shellcheck-off.toml`. `SUITE_ONLY=S001,S002` runs only those cases; `BENCH_SETTLE` is the seconds waited after didOpen. |
| `report.py` | With `--compare ...` (usage in the docstring of `suite_doc`), writes `docs/benchmark.md` from a suite result, `suite_triage.json` (the note on each failing case), the competitor results, the settings review and the oracle result. Without arguments, builds the comparison table of `results/REPORT.md` from `results/*.json` (run it from this directory). |
| `oracle_trace.py [OUT.json]` / `--selftest` | Runs the corpus2 scripts that decide a `bash-trace` or `manual` expectation under a real bash (`/bin/bash` 3.2.57 and any other builds you point it at) on a temporary copy, `env -i PATH=/usr/bin:/bin`, with a DEBUG trap, behind a static guard (every command word must be a corpus function or a fixed side-effect-free builtin; no redirections, backticks or process substitution; `eval` of literals only; `source` only of files in the copy) and, where `sandbox-exec` works, a profile that denies network and writes outside the temporary directory. Writes `results/oracle-trace.json` (agree, disagree, undecided, skipped per case and per build, plus the traces). It never edits a truth file. `--selftest` checks that a wrong expectation disagrees and that unsafe scripts are refused. |
| `version_probe.py BASH [OUT]` | Runs one fixed snippet per row of the bash version table under `BASH`; writes `results/version-probe-<version>.json`. `docs/version-table.md` explains the table. |
| `scale.py NAME BIGFILE -- CMD...` | One large script: didOpen to first documentSymbol, edit to refreshed documentSymbol, definition, RSS. |
| `scale_ws.py NAME WSDIR -- CMD...` | Workspace scale: didOpen of a file in the middle of a source chain, then two kinds of edit timed separately to a refreshed cross-file definition, six samples each: body edits (the source graph is untouched) and edits that add or remove a `source` line (visibility is recomputed). Also the time from start to the first workspace answer. |
| `scale_diag.py NAME WSDIR BIGFILE -- CMD...` | Time from `didChange` to the native `publishDiagnostics`, on the workspace and on the large file. |
| `editor_latency.py NAME BIGFILE CORPUS [--ws WSDIR] -- CMD...` | Latency of the editor requests (completion, completionItem/resolve, signatureHelp, semanticTokens full/range/delta, foldingRange, selectionRange, documentHighlight, documentLink, inlayHint, prepareRename, rename, and the warm deferred hover): 3 unrecorded and 30 recorded calls each, on the corpus (`main.sh`), on the 561 KB file (also `didChange` to `publishDiagnostics`, and `inlayHint` again with `inlay_hints.parameter_names` on) and, with `--ws` (a `gen_workspace.py` directory), `prepareRename` and `rename` in the 1,000-file chain from the middle and the last file. The clock stops at the last byte of a reply. Run it with `BASH_LSP_CONFIG` set to the absolute path of `config-shellcheck-off.toml`. The JSON holds medians, p95, every sample, the load average before and after and the commit. |
| `repeat.py OUT LABEL N [--wall] -- CMD...` / `summarize.py FILE KEY...` | Run a measurement N times and keep every raw sample in `results/repeat-*.json` with date, commit and machine; recompute min, median, p95, max. |
| `gen_big.py OUT` | Writes the large script: 2,000 functions, 560,711 bytes (about 561 KB). Each function has a doc comment, a `local`, an `if [[ ]]` with a pipeline, a `for` loop and a `case`, and every function is called by the next one. |
| `gen_arith.py OUT` | A second large file, 552 KB, in the shape of `gen_big.py` with one or two arithmetic constructs per function. |
| `gen_comments.py OUTDIR [--decl]` | Writes `c.sh`, 24,000 comment lines (1,860,930 bytes), the input for timing `ast --json`. With `--decl`, a function sits directly under the block: the input for timing doc comments. |
| `gen_workspace.py OUTDIR [FILES=1000] [FUNCS=20]` | Writes the workspace: file `i` does `source ./s{i-1}.sh`, and function `i_k` calls function `(i-1)_k`, so every call crosses a file and the last file sees the whole chain. |
| `gen_bind_shapes.py`, `gen_product_shapes.py KIND OUT ...` | Write shapes that stress the binder and the call graph: many definitions of one name, `unset -f`, aliases, `"$cmd"` calls, dynamic scope, diagnostic rules that walk scopes. `gen_product_shapes.py` makes shapes whose graph output grows with a product of two counts (`defs_calls`, `unset`, `cmd`, `dynscope`). |
| `shape_cells.py FILE [MS] [RATIO]`, `shape_sizes.py PROBE WORKDIR N SHAPE[:COLS]...` | Read a bind-shapes table and list the cells that are slow or grow faster than linear; time shapes at four sizes with the ratio per doubling. |
| `refusals.py BINARY [--cap N] [--all ROOT]... ROOT...` | Corpus check for the parser's nesting limit: collects shell files under each root (read-only, by extension or shebang, at most 2 MiB each), runs `BINARY ast --json` on each and prints the files with the error "nesting is deeper than 10000 levels". |
| `testdata/shapeprobe/` | The probe behind the shape tables: Parse, Bind, Build and the snapshot queries on one file through the public API (module replace to this repository). |
| `testdata/corpus2/`, `suite_cases/` | The corpus and the cases of the truth-file suite. |
| `config-shellcheck-off.toml` | Settings that turn ShellCheck off, so a run times the native findings only. |

## Run

```sh
go build -o /private/tmp/bash-lsp/bash-lsp ./cmd/bash-lsp
export BENCH_CORPUS=$PWD/testdata/corpus
uv run --no-project python bench/bench.py "bash-lsp" -- /private/tmp/bash-lsp/bash-lsp serve > /tmp/bash-lsp-bench.json

uv run --no-project python bench/gen_big.py /private/tmp/bash-lsp/big.sh
uv run --no-project python bench/scale.py "bash-lsp" /private/tmp/bash-lsp/big.sh -- /private/tmp/bash-lsp/bash-lsp serve

uv run --no-project python bench/gen_workspace.py /private/tmp/bash-lsp/ws1000
uv run --no-project python bench/scale_ws.py "bash-lsp" /private/tmp/bash-lsp/ws1000 -- /private/tmp/bash-lsp/bash-lsp serve
```

`serve` is also the default with no arguments. The first launch of a freshly
built binary on macOS is slower (the system scans a new executable); launch it
once before measuring.

## Results

`results/` holds the committed results that the documents cite. Each file
records the date, the machine and, where the script records one, the commit it
was taken at; those commit ids refer to the development history, not to a
published tag.

| Files | What they are |
|---|---|
| `bls.json`, `shuck.json`, `bashd.json`, their `*-ann.json` runs on the annotated corpus, `mvdan.json`, `scale-bls.json`, `scale-shuck.json`, `scale-bashd.json`, `REPORT.md` | The 28 original cases and the large-file run for the other servers and for the parser alone, from an earlier evidence kit; `REPORT.md` is the comparison table `report.py` builds from them and the `bash-lsp*.json` files below. |
| `bash-lsp.json`, `bash-lsp-ann.json`, `scale-bash-lsp.json`, `scale-ws-bash-lsp.json`, `repeat-big-file.json`, `repeat-workspace-edits.json` | The first bash-lsp runs of the same measurements (the 20 of 28 baseline), kept as the starting point. |
| `bash-lsp-m4.json` | `bench.py` on the 0.4.0 release binary: 28 of 28. |
| `suite-bash-lsp-m4.json`, `suite-bash-lsp-m4-rc.json` | `suite.py` for bash-lsp; the `-rc` file is on the release binary (the darwin/arm64 binary extracted from its archive, ShellCheck off) and is the one `docs/benchmark.md` is built from. |
| `suite-bls-m4.json`, `suite-bls-includeall-m4.json`, `bls-setting-*-m4.json`, `bls-settings-m4.json` | `suite.py` for bash-language-server with default settings and with `INCLUDE_ALL_WORKSPACE_SYMBOLS=true`, and the settings review (each setting run over the cases that fail by default). |
| `oracle-trace.json` | `oracle_trace.py`: each decidable expectation run under real bash builds. |
| `editor-latency-m4.json`, `editor-latency-m4-rc.json`, `repeat-big-file-m4*.json`, `repeat-workspace-edits-m4*.json`, `load-m4-rc.txt` | Latency, large-file and workspace-edit measurements, with the 1-minute load averages of the `-rc` runs. |
| `version-probe-*.json` | The bash version table probed on each real build. |
| `lowerer-depth.txt`, `resync-cost.txt` | The first depth at which the lowerer refuses each construct, and the cost of resync on the slowest input known (cited by `docs/library.md`). |

**A note on `bench.py`'s reader.** Its unbuffered reader appends each raw read
to the message so far, so reading one message costs time quadratic in its size.
It does not matter for the documents the benchmark opens, but a very large
`publishDiagnostics` (a 2 MiB file with 220,000 findings is 32 MB of JSON)
looks super-linear in `scale.py` where a buffered client measures it linear.
`bench.py` stays as it is. A measurement of publish size or diagnostics volume
must use a client that reads with a buffer, as `scale_diag.py` and
`editor_latency.py` do.

**Which median.** A workspace-edit median is the median of all raw samples of
the file (30 runs x 6 edits = 180 per kind), the statistic `summarize.py`
prints, for example
`uv run --no-project python bench/summarize.py bench/results/repeat-workspace-edits-m4-rc.json body_edit_to_definition_ms graph_edit_to_definition_ms`.
Single-file repeats have one sample per run, so both definitions agree there.

# Changelog

One entry per milestone, newest first. Version 0.4.0 is the first version
number; milestones 1 to 3 were never released on their own. Every benchmark
count names the result file it was read from. Nothing here is published: no
binary is released and no tag exists.

## 0.4.0 - release candidate (milestone 4), not published

Built and verified on one machine (`release/0.4.0/verify.txt`); the
acceptance record is `docs/m4-acceptance.md`.

New:

- Editor features: completion (variables, functions, builtins, keywords,
  `${name` operators with the bash version they need, `source` paths, command
  options and `PATH` commands) with resolve; hover on builtins and external
  commands from `help` and `man`; signature help for script functions;
  semantic tokens (full, delta, range); folding ranges; selection ranges;
  document highlights; document links for `source` words and shebangs; inlay
  hints (`source` targets, parameter names); code actions (add a shebang, quote
  an expansion). `source.fixAll` is not offered because no fix is marked safe.
- Rename, on the command line (`bash-lsp rename SYMBOL|FILE:LINE:COL NEW`,
  preview by default, `--apply` to write), in the editor (`prepareRename` and
  `rename`) and over MCP (`preview_rename`). It edits only sites the analysis
  resolved to the symbol and lists the rest. It refuses, with a stable code, when
  it cannot prove the rename: a lone carriage return in an edited file, any
  workspace file or directory that was not analysed (too large, over the file
  limit, unreadable, in an unsupported dialect such as zsh, too deep; the way
  out is `files.exclude`), half-typed buffers, names reached through `eval`,
  `unset`, `declare -n` and similar, dynamic scope, and name conflicts. It warns
  when settings or `.gitignore` left files out. `--apply` by name refuses when
  other symbols of that name exist, writes each file by a temporary file and
  rename, and on a failed write puts the files back and keeps the backups.
- `help` and `man` lookups: the plugin may now start `bash --noprofile --norc
  -c 'help -- "$1"'` and `man --` besides ShellCheck, under the same rules (found
  outside the workspace, filtered environment, time and output limits, nothing
  stored on disk, not on Windows, `help.enabled` turns them off).
- New settings: `inlay_hints.source_targets`, `inlay_hints.parameter_names`,
  `completion.path_commands`, `help.enabled`, `help.bash_path`, `help.man_path`.
- Positions: the editor's line breaks (`\n`, `\r\n`, lone `\r`) and the tools'
  (`\n` only) are two separate definitions; the README says which output uses
  which.
- Diagnostics: a stray `else` is a syntax error; an unclosed here-document is
  a warning at top level and an error inside an open construct; an empty
  `then`, `do`, `{ }` or `( )` body is reported at the closing keyword.
- Version table: negative array subscripts start at bash 4.2 (it said 4.3). Every
  row was checked against real bash builds 3.2.57, 4.0, 4.2, 4.3, 4.4, 5.0, 5.1
  and 5.3 (`bench/results/version-probe-*.json`); 4.1 and 5.2 were not built.
- `scripts/build-release.sh` takes `[VERSION] [OUT]`, refuses a dirty tree and
  writes `binaries.txt` for reproducibility checks; the CI workflow has a
  `test-macos` job that runs on manual dispatch only.
- Benchmark: all 28 cases pass (`bench/results/bash-lsp-m4-task4c.json`, re-run
  on the release binary in `docs/m4-acceptance.md`). A second suite of
  155 cases from truth files written before any result: 127 of 138 gating and
  17 of 17 design cases pass (`bench/results/suite-bash-lsp-m4-fixwave.json`);
  the 11 failing gating cases are known limits, listed in `docs/benchmark.md`.

Not done, and not claimed:

- Check in one real editor (VS Code): not run; the owner deferred it. The
  milestone 4 exit criterion "working in one real editor" is therefore unmet.
- CI has never run; AC-16 (Linux, macOS and Windows) is unmet. Nothing was
  measured on Linux or Windows.
- SARIF output; the code-index patch (AC-14, AC-21); shuck as a second lint
  backend; the ShellCheck memory guard on Linux and Windows; result-size caps
  on references and call hierarchy answers in the editor.
- The signatures of the downloaded bash source tarballs were not verified.

Known gaps: the lexer reads a lone carriage return as a blank (bash reads a
character); `prepareRename` builds the rename plan that `rename` builds again;
every edit re-parses the whole file; `selectionRange` answers only the first
1,000 positions of a request (each later one gets an empty range); four helpers
that cut a text to a length (in `internal/lsp`, `internal/query`,
`cmd/bash-lsp` and `internal/mcp`) have drifted apart; the excluded-directory
warning of rename repeats on every rename in a repository with an ignored
directory; `$(else)` is an error although bash 3.2 to 5.1 accept it.

## Milestone 3 (not released separately)

- Diagnostics: native syntax, `source`, command, scope, quoting and version
  rules, with a ShellCheck bridge; `check` with exit codes and `--fail-on`;
  comment directives to turn a finding off.
- A formatter (`fmt`, and LSP formatting) that refuses what it cannot print
  back to the same tree.
- An MCP server (`mcp`) with the graph tools and search; `search` with
  code-index full-text hits; secret redaction of outgoing text.
- `check --hook`, the plugin's optional `PostToolUse` fallback, off unless the
  user settings enable it; repository files can not set program paths.
- Safety bounds of the ShellCheck run (time, output, concurrency, neutral
  directory); the memory guard is not met.
- Benchmark: 27 of 28, failing only C24 (rename)
  (`bench/results/bash-lsp-m3.json`). SARIF output was not built.

## Milestone 2b (not released separately)

- The call graph with a resolution and a reason on every edge; call hierarchy;
  the graph commands (`status`, `callers`, `callees`, `refs`, `impact`,
  `trace`, `unresolved`, `cycles`); `extract` (JSON lines for code-index).
- Dynamic-scope candidates (a callee's read linked to a caller's `local`) and
  `invokes` edges for scripts run by path.
- Watched files and several workspace folders.
- Benchmark: 26 of 28, failing C24 and C28
  (`bench/results/bash-lsp-m2b-final.json`).

## Milestone 2a (not released separately)

- The crash on deeply nested input is closed: the parser stops at 10,000
  levels with a syntax error.
- A public Go entry point (`docs/library.md`).
- Binding semantics: assignments in subshells and pipelines no longer escape
  them, and arithmetic contexts (`$(( ))`, `(( ))`, `let`, C-style `for`) give
  reads and writes.
- Benchmark: 20 of 28 (`bench/results/bash-lsp-m2a-final.json`).

## Milestone 1 (not released separately)

- Go to definition, references, implementation, hover, and document and
  workspace symbols across `source`d files; syntax errors reported with the
  files around them still analysed.
- `ast`, `symbols` and `doctor` commands; the Claude Code plugin.
- Benchmark: 20 of 28 (`bench/results/bash-lsp.json`).

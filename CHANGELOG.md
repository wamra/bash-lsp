# Changelog

Newest first. Version 0.4.0 is the first version number. Every benchmark count
names the result file it was read from.

## 0.4.0 - preview

The first public version. What it contains:

**Navigation and analysis**

- Go to definition, references, implementation, hover, and document and
  workspace symbols across `source`d files (static, `$(dirname "$0")`-relative
  and glob loaders), through aliases and `"$cmd"` calls, and through the strings
  of `eval`, `trap` and `sh -c`, which are reported as candidates with their
  reason.
- Locals are scoped to their function; assignments in subshells and pipelines
  do not escape them; arithmetic contexts (`$(( ))`, `(( ))`, `let`, C-style
  `for`) give reads and writes. A read in a callee that no declaration in view
  explains is linked, as a candidate, to a caller's `local`.
- The call graph carries a resolution and a reason on every edge: call
  hierarchy in the editor, and the commands `status`, `callers`, `callees`,
  `refs`, `impact`, `trace`, `unresolved` and `cycles`. A script run by path
  gives an `invokes` edge.
- Syntax errors are reported and the files around them still analysed. The
  parser stops at 10,000 levels of nesting with a syntax error.
- Watched files and several workspace folders.

**Editor features**

- Completion (variables, functions, builtins, keywords, `${name` operators with
  the bash version they need, `source` paths, command options and `PATH`
  commands) with resolve; hover on builtins and external commands from `help`
  and `man`; signature help for script functions; semantic tokens (full, delta,
  range); folding ranges; selection ranges; document highlights; document links
  for `source` words and shebangs; inlay hints (`source` targets, parameter
  names); code actions (add a shebang, quote an expansion). `source.fixAll` is
  not offered because no fix is marked safe.

**Diagnostics and formatting**

- Native rules for syntax, `source`, commands, scope, quoting and bash versions
  (a version table checked against real bash builds), with a ShellCheck bridge;
  `check` with exit codes and `--fail-on`; comment directives to turn a finding
  off.
- A formatter (`fmt`, and LSP formatting) that refuses what it cannot print
  back to the same tree.
- A stray `else` is a syntax error; an unclosed here-document is a warning at
  top level and an error inside an open construct; an empty `then`, `do`, `{ }`
  or `( )` body is reported at the closing keyword.

**Rename**

- On the command line (`bash-lsp rename SYMBOL|FILE:LINE:COL NEW`, preview by
  default, `--apply` to write), in the editor (`prepareRename` and `rename`) and
  over MCP (`preview_rename`). It edits only sites the analysis resolved to the
  symbol and lists the rest. It refuses, with a stable code, when it cannot
  prove the rename: a lone carriage return in an edited file, any workspace file
  or directory that was not analysed (too large, over the file limit, unreadable,
  in an unsupported dialect such as zsh, too deep; the way out is
  `files.exclude`), half-typed buffers, names reached through `eval`, `unset`,
  `declare -n` and similar, dynamic scope, and name conflicts. It warns when
  settings or `.gitignore` left files out. `--apply` by name refuses when other
  symbols of that name exist, writes each file by a temporary file and rename,
  and on a failed write puts the files back and keeps the backups.

**Tools and packaging**

- `bash-lsp mcp`, an MCP server with eleven read-only tools; `search`, which
  adds code-index full-text hits to the name matches; secret redaction of
  outgoing text.
- `bash-lsp extract`, the call graph as JSON lines for code-index.
- A public Go entry point (`docs/library.md`).
- The Claude Code plugin, with an optional `PostToolUse` hook (`check --hook`),
  off unless `hook.enabled` is set in the user settings. Repository files cannot
  set program paths or turn the hook on.
- `help` and `man` lookups: the language server may start `bash --noprofile
  --norc -c 'help -- "$1"'` and `man --` besides ShellCheck, under the same
  rules (found outside the workspace, filtered environment, time and output
  limits, nothing stored on disk, not on Windows; `help.enabled` turns them off).
- Settings: `inlay_hints.source_targets`, `inlay_hints.parameter_names`,
  `completion.path_commands`, `help.enabled`, `help.bash_path`, `help.man_path`.
- `scripts/build-release.sh [VERSION] [OUT]` builds six archives, refuses a dirty
  tree and writes `binaries.txt` for reproducibility checks. A CI workflow runs
  `go vet` and the tests on Linux and Windows (and on macOS when started by
  hand).

**Positions.** The editor's line breaks (`\n`, `\r\n`, lone `\r`) and the tools'
(`\n` only) are two separate definitions; the README says which output uses
which.

**Bash versions.** Every row of the version table was checked against real bash
builds 3.2.57, 4.0, 4.2, 4.3, 4.4, 5.0, 5.1 and 5.3
(`bench/results/version-probe-*.json`); 4.1 and 5.2 were not built. Negative
array subscripts start at bash 4.2.

**Benchmark.** All 28 original cases pass (`bench/results/bash-lsp-m4.json`). A
second suite of 155 cases, from truth files written before any result: 127 of
138 gating and 17 of 17 design cases pass
(`bench/results/suite-bash-lsp-m4-rc.json`); the 11 failing gating cases are
known limits, listed in `docs/benchmark.md`.

**Not done**

- A check in VS Code; no other editor than Claude Code was tried.
- Results on Linux and Windows: the CI workflow has not run yet, and nothing was
  measured there.
- SARIF output; a ShellCheck memory limit; result-size caps on references and
  call hierarchy answers in the editor.
- The signatures of the downloaded bash source tarballs were not verified.

**Known limits** are in `docs/limitations.md`: statement-order-free binding,
aliases, functions defined by `eval`, resync past an unclosed quote, a lone
carriage return read as a blank, zsh files, and the rest.

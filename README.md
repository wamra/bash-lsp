# bash-lsp

A Bash language server written in Go. It resolves definitions, references and
symbols across `source`d files, over LSP on stdio.

Status: milestone 2b is built and not yet released. The crash on crafted
deeply nested input is closed in milestone 2a: the parser stops at 10,000
levels of nesting with a syntax error (`docs/m2a-acceptance.md`,
`docs/m2a-nesting-design.md`). Go to definition, references,
implementation, hover, call hierarchy, document and workspace symbols work
across `source`d files (static, `$(dirname "$0")`-relative and glob loaders),
through aliases, through `"$cmd"` calls, and through the strings of `eval`,
`trap` and `sh -c`, which are reported as candidates with their reason; locals
are scoped to their function, and a read in a callee that no declaration in view
explains is linked, as a candidate, to a caller's `local`.
Syntax errors are reported and the files around them still analysed. The
graph CLI commands, `extract` (the call graph as code-index's JSON lines),
watched files and several workspace folders are in. It passes 26 of the 28
cases of the comparison benchmark (`docs/m2b-acceptance.md`); rename and lint
are later milestones (the two failing cases are rename and the unquoted
variable diagnostic), and known gaps are listed in `docs/m2b-acceptance.md`
and `docs/m1-deferred.md`. Linking into code-index (AC-14, AC-21) needs a
patch in code-index first (`docs/code-index-integration.md`). There are no
published binaries and no stable interface yet, and CI has not run.

Licence: MIT (see `LICENSE`). Third-party attribution is in `NOTICE`.

## Turning a diagnostic off

A comment directive silences findings:

```bash
# bash-lsp disable=WB1401,WB1202 -- why, in your words
# shellcheck disable=SC2086
```

- `# bash-lsp disable=` takes rule ids (two letters and four digits, such as
  `WB1401`), separated by commas or spaces. The list ends at the first word
  that is not shaped like a rule id; the rest of the line is your comment. An
  id that names no rule is reported (WB1008).
- `# shellcheck disable=` takes ShellCheck codes (`SC2086` or `2086`), ranges
  (`SC3000-SC4000`) and `all`, and silences the native rules that report the
  same problem as one of the codes.
- Placement follows ShellCheck: before the first command, a directive applies
  to the whole file; otherwise to the next command or compound command (a
  whole `if`, loop or function). It may stand alone on its line or after an
  opener on it (`then`, `do`, `else`, `{`, `(`, a case pattern, a function
  header). After a command on the same line, before `else` or `elif`, between
  case arms, or with no command after it in its block, it applies to nothing.
- The settings key `diagnostics.disable` turns a rule off everywhere.
- The settings key `files.exclude` skips files and directories (gitignore syntax). The user file and the repo file each have their own list with their own `!` negations, and a path is skipped when either list skips it: a repo negation cannot bring back what the user excluded, and a repo pattern skips a file the user's list re-included (user `["*", "!keep.sh"]` with repo `["*.sh"]` skips `keep.sh`).

## Command line

```
bash-lsp                          serve LSP on stdio (same as `serve`)
bash-lsp version
bash-lsp ast FILE [--json]        print the syntax tree of one file
bash-lsp symbols [PATH...] [--json]   functions, aliases and variables; default PATH is .
bash-lsp doctor [PATH]            workspace report: files, dialects, limits, syntax errors, unresolved sources
bash-lsp check [paths] [--format text|json] [--shellcheck auto|off] [--fail-on error|warning|info|hint] [--root DIR]
bash-lsp status [--root DIR] [--json]
bash-lsp callers|callees SYMBOL [--depth N] [--root DIR] [--json]
bash-lsp refs SYMBOL [--root DIR] [--json]
bash-lsp impact SYMBOL|FILE [--depth N] [--root DIR] [--json]
bash-lsp trace A B [--root DIR] [--json]
bash-lsp unresolved [PATH...] [--root DIR] [--json]
bash-lsp cycles [--root DIR] [--json]
bash-lsp extract [--root DIR] [--list-file FILE]   call graph as JSON lines (docs/code-index-integration.md)
```

The graph commands print every edge with its resolution and reason
(`resolved/direct`, `resolved/alias`, `candidate/variable-constant`,
`candidate/string-eval`, ...); SYMBOL is a function name, `path` or
`path:line:col`.
The JSON each command prints with `--json` is described in
[JSON records](#json-records).

Positions are 1-based `line:col`, with columns counted in UTF-16 code units
(the LSP column plus one), printed and accepted alike. Paths are
relative to the workspace root, with forward slashes on every OS. Exit codes:
0 success, 1 runtime failure, 2 usage error.

`check` prints the findings of the named files and directories (default `.`), one
`path:line:col: severity RULE message` line each, or with `--format json` one
object `{"generation", "diagnostics": [...]}`. A path in the output is relative
to the current directory when the file is under it, otherwise absolute, with
forward slashes. The root is `--root`, else the nearest directory above the
first path that holds `.git` or `.bash-lsp.toml` (its files give `source` and
the cross-file rules their context); a file with no such directory above it is
analysed alone. ShellCheck runs when it is found on the `PATH` or set in the
user settings and is not inside the workspace. Exit codes: 0 no finding at or
above `--fail-on` (default `warning`), 1 findings, 2 usage error, 3 runtime
failure (an unreadable path or a failed ShellCheck run, even when findings were
printed). What check did not cover goes to stderr and is not part of the exit
code: `no shell files checked: PATH: why` for a directory with nothing to check
(ignored, or no shell files), `not analysed: FILE: why` for a file that was not
parsed (over the size limit, or a shell the server does not support), the
workspace limits met, and the workspace notices.

## Formatting

`bash-lsp fmt [-w] [--root DIR] [--] FILE...` formats with the in-process
printer (shfmt's), with options from the `.editorconfig` files between the
file and the root, then `[format]` of the settings, then tabs. One file
without `-w` prints the text; several list the files that would change and
exit 1 if any does; `-w` rewrites them. A file is left alone, reported, and
makes the exit code 1 when it has a syntax error, is zsh, has a byte order
mark, NUL bytes or mixed line endings, is over the size cap (2 MiB, or
`[limits] max_file_bytes` if lower), is outside the root (the current
directory unless `--root`), cannot be written, or is an sh file that the POSIX
and the bash grammar read differently (`[[ $a<$b ]]`). Every printed text is
parsed again and must give the same tree as the input.

What it does not do:

- `-w` writes a temporary file and renames it over the original. The mode
  bits are kept; the owner, extended attributes, hard links and setuid or
  sticky bits are not, and a change made to the file between the read and the
  rename is lost.
- A CRLF file is read as line-terminated (bash sees the `\r` as part of the
  last word of a line); the output keeps CRLF.
- `$[ ... ]` and `for ((;;)) { ... }` are refused: the printer does not keep
  them.
- Command substitutions in backquotes are printed as `$( )`.

## JSON records

Every `--json` answer is one object that carries `generation` (the snapshot it
was computed from). Paths are relative to the workspace root, with forward
slashes; `line` and `col` are 1-based with UTF-16 columns. The table lists,
for each command, the keys of that object and the fields of one record; a
field followed by `?` is left out when it does not apply. `status` is flat:
the object is the record. `ast` and `extract` are not in the table (`extract`
prints JSON lines, see `docs/code-index-integration.md`).

<!-- json-records:begin -->
| command | wrapper object | fields of one record |
|---|---|---|
| `status` | flat | `generation`, `root`, `files`, `dialects`, `skipped`, `skipped_files`, `limits`, `nesting_errors`, `cache` |
| `symbols` | `generation`, `symbols` | `path`, `line`, `col`, `kind`, `name`, `container` |
| `callers` | `generation`, `results` | `caller`, `callee`, `relation`, `resolution`, `reason`, `site`, `definition`? |
| `callees` | `generation`, `results` | `caller`, `callee`, `relation`, `resolution`, `reason`, `site`, `definition`? |
| `refs` | `generation`, `results` | `path`, `line`, `col`, `resolution`?, `reason`?, `relation`? |
| `impact` | `generation`, `results` | `path`, `function` |
| `trace` | `generation`, `found`, `results` | `caller`, `callee`, `relation`, `resolution`, `reason`, `site`, `definition`? |
| `unresolved` | `generation`, `results` | `caller`, `callee`, `relation`, `resolution`, `reason`, `site`, `text`? |
| `cycles` | `generation`, `results` | `files` |
<!-- json-records:end -->

In an edge record (`callers`, `callees`, `trace`, `unresolved`), `caller` and
`callee` are `{path, function}` (`function` is null for a file); `site` and
`definition` are `{path, line, col}`. `callee` is null only in `unresolved`,
where `text` holds the source text of the target that could not be resolved.
`definition` is the callee's definition the edge goes to; it is on calls, not
on `source` edges. `found` in `trace` is false, with an empty `results`, when
there is no path. In `refs`, `resolution`, `reason` and `relation` are left
out for a reference that is not a call edge to the function (such as the
`cmd=name` value that a `"$cmd"` call later reaches). The definition itself
is not listed.

## Packaging

`scripts/build-release.sh [VERSION]` cross-builds six archives and checksums into
`dist/`. `scripts/build-plugin.sh OUT [VERSION]` stages the Claude Code plugin
(see `plugin/README.md`).

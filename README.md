# bash-lsp

A Bash language server and analysis toolkit written in Go. It finds
definitions, references and symbols across `source`d files, reports syntax and
lint problems, formats scripts, renames functions and variables safely, and
answers call-graph questions, over the Language Server Protocol on stdio, on
the command line and through an MCP server. It is packaged as a Claude Code
plugin.

It reads shell files and never runs them. The syntax tree comes from a vendored
copy of the `mvdan.cc/sh` parser; the part bash-lsp adds is the layer above
it: which definition a call reaches, through `source` (static,
`$(dirname "$0")`-relative and glob loaders), aliases, `"$cmd"` calls and the
strings of `eval`, `trap` and `sh -c` (reported as candidates, with the reason).
The binary has no dependencies beyond the Go standard library and needs no
CGO.

Status: version 0.4.0, preview. The command line, the settings keys and the
JSON output may still change between versions. What it does not do, or does
differently from bash, is in [docs/limitations.md](docs/limitations.md).

## Install

**As a Claude Code plugin** (macOS, Linux, Windows):

```
claude plugin marketplace add wamra/wammy-plugins
claude plugin install bash-lsp@wammy-plugins
```

The plugin registers the language server for `.sh`, `.bash`, `.bats` and `.ksh`
files and an optional hook that checks a file after Claude edits it. It
contains a small launcher and six binaries (macOS, Linux and Windows, each for
amd64 and arm64). What the hook does, and when it does nothing, is in
[plugin/README.md](plugin/README.md).

**Release archives:** `scripts/build-release.sh [VERSION] [OUT]` cross-builds
six archives (macOS, Linux, Windows; amd64, arm64) into `OUT` (default `dist/`)
with `checksums.txt` (the archives) and `binaries.txt` (each binary). Each
archive holds the binary, `LICENSE`, `THIRD_PARTY_NOTICES.md` and `README.md`.
VERSION defaults to the `VERSION` file. The script refuses a tree with
uncommitted or untracked files and an `OUT` that is not a previous output.
Archives carry modification times, so two builds differ in `checksums.txt`; the
binaries are reproducible, so compare `binaries.txt`. The CI workflow runs the
script and keeps the result for a week as a build artifact. No prebuilt
archive is published yet.

**Build from source** (Go 1.26.0 or newer, the version in `go.mod`; no CGO):

```
go build -o bash-lsp ./cmd/bash-lsp
./bash-lsp version
./bash-lsp doctor        # what it found: files, dialects, limits, ShellCheck, help and man
```

`scripts/build-plugin.sh OUT [VERSION]` stages the plugin tree, with all six
binaries, into `OUT`, and `scripts/check-plugin.sh OUT` checks it.

Any editor that speaks LSP can run `bash-lsp serve` (or `bash-lsp` with no
arguments) as the server for shell files.

## Platforms

macOS is the development platform. `.github/workflows/ci.yml` runs `go vet` and
the tests on Linux (also with the race detector), Windows and macOS on every
push and pull request. Timing assertions are checked on the development
machine; on CI the latency budgets are four times as long and the growth
checks only log. Nothing is started on Windows for the
`help` and `man` lookups. How Claude Code on Windows should start the plugin's
`.exe` is not settled, so launching the plugin there is untested. See
[docs/limitations.md](docs/limitations.md).

## Features

**Language server.** `bash-lsp serve` answers these requests; the table in
[Editor features](#editor-features) lists what each answers.

| Group | Methods |
|---|---|
| Navigation | `textDocument/definition`, `references`, `implementation`, `documentSymbol`, `workspace/symbol`, `prepareCallHierarchy`, `callHierarchy/incomingCalls`, `callHierarchy/outgoingCalls`, `documentHighlight`, `documentLink` |
| Information | `textDocument/hover`, `completion`, `completionItem/resolve`, `signatureHelp`, `inlayHint`, `semanticTokens/full`, `semanticTokens/full/delta`, `semanticTokens/range`, `foldingRange`, `selectionRange` |
| Diagnostics and edits | pushed diagnostics, `textDocument/diagnostic` (pull), `codeAction`, `formatting`, `rangeFormatting`, `prepareRename`, `rename` |
| Workspace | `didOpen`, `didChange`, `didClose`, `didSave`, `workspace/didChangeWatchedFiles`, `didChangeConfiguration`, `didChangeWorkspaceFolders`; several workspace folders |

**Analysis.** Go to definition, references, implementation and hover (with the
`help` or `man` synopsis of a builtin or command); locals scoped to their
function; a read in a callee that no declaration in view explains is linked, as
a candidate, to a caller's `local`. Syntax errors are reported and the files
around them still analysed.

**Lint.** Native rules (`WB` ids) for syntax, `source`, commands, scope,
quoting and bash versions (the version table is in
[docs/version-table.md](docs/version-table.md)), plus a ShellCheck bridge when
ShellCheck is installed. A [comment directive](#turning-a-diagnostic-off)
silences a finding.

**Formatter.** `bash-lsp fmt` and LSP formatting, with a printer that refuses
what it cannot print back to the same tree ([Formatting](#formatting)).

**Rename.** A safe rename on the command line, in the editor and as an MCP
preview ([Rename](#rename)).

**Command line.** The graph commands (`status`, `callers`, `callees`, `refs`,
`impact`, `trace`, `unresolved`, `cycles`), `check`, `fmt`, `symbols`, `ast`,
`doctor`, `search`, `rename`, `extract` and `mcp` ([Command line](#command-line)).

**MCP server.** `bash-lsp mcp [--root DIR]` serves eleven read-only tools on
stdio: `find_symbol`, `find_references`, `get_callers`, `get_callees`,
`trace_dependency`, `analyze_impact`, `list_unresolved`, `get_index_status`,
`search_code`, `get_diagnostics` and `preview_rename`. See
[docs/mcp.md](docs/mcp.md).

**Library.** The packages `bash-lsp`, `analysis`, `parse` and `tree` are
importable ([docs/library.md](docs/library.md)). `bash-lsp extract` prints the
call graph as JSON lines for code-index
([docs/code-index-integration.md](docs/code-index-integration.md)).

SARIF output is not built (`--format sarif` exits 2).

## Configure

Settings are TOML. Three places are read, in this order, each overriding the
one before:

1. the **user file**: `<user config dir>/bash-lsp/config.toml` (on macOS
   `~/Library/Application Support/bash-lsp/config.toml`), or the file named by
   the `BASH_LSP_CONFIG` environment variable (an empty value means no user
   file). It serves the language server, the command line and the hook alike.
2. the **repository file** `<root>/.bash-lsp.toml`, which can arrive with a
   clone and is therefore untrusted (see [Privacy and
   security](#privacy-and-security));
3. the client's `initializationOptions` (the same keys as JSON, sent by the
   editor to the language server only). The Claude Code plugin sends none.

Each file is read up to 64 KiB. A file that does not parse is ignored whole; an
unknown key, a value of the wrong type and a value a file may not set are
ignored one by one, and each is reported with its line (on standard error by
the commands).
Example:

```toml
[shellcheck]
enabled = false

[hook]
enabled = true

[severity]
WB1103 = "warning"
```

| Key | Type | What it does | Set by |
|---|---|---|---|
| `bash.target` | string `"MIN-MAX"` | The bash versions a script must run on (default `3.2-5.3`). Only MIN drives the version rule WB1301. | any |
| `bash.dialect` | `bash`, `sh`, `mksh` or `bats` | Dialect of a file with no directive, no shebang and a `.sh` or no extension. | any |
| `diagnostics.disable` | list of rule ids | Rules turned off everywhere. A repository's list is added to the user's, never subtracted. | any |
| `[severity]` | rule id = `error`, `warning`, `info` or `hint` | Severity of a native rule. A repository cannot change a rule the user disabled or gave a severity. | any |
| `shellcheck.enabled` | bool (default true) | Add ShellCheck findings. | any; a repository can turn it off, not on again |
| `shellcheck.path` | string | The ShellCheck program. | user only |
| `shellcheck.timeout_seconds` | integer 1 to 120 (default 10) | Time limit of one ShellCheck run. | any; a repository can only lower it |
| `codeindex.path` | string | The code-index program that `search` and `search_code` start. | user only |
| `path.profile` | list of directories | Directories searched for commands (default `PATH`). | user only |
| `hook.enabled` | bool (default false) | Turns on `check --hook`, the plugin's `PostToolUse` hook. | user only |
| `files.include`, `files.exclude` | list of gitignore patterns | Files and directories to read, relative to the root. The user's and the repository's `files.exclude` are two lists with their own `!` negations; a path is excluded when either excludes it. | any |
| `sources.paths` | list of directories | Extra directories searched by `source`, relative to the root; one that is absolute, uses `~` or `$`, or resolves outside the root is dropped with a notice. | any |
| `limits.max_files`, `limits.max_file_bytes` | integer | Lower the built-in limits (20,000 files, 2 MiB per file). | any; a repository can only lower them |
| `format.indent`, `format.binary_next_line`, `format.switch_case_indent`, `format.space_redirects`, `format.function_next_line`, `format.simplify` | integer, bool | Formatter options ([Formatting](#formatting)). | any |
| `inlay_hints.source_targets` | bool (default true) | `-> lib/util.sh` hints after dynamic `source` paths. | any |
| `inlay_hints.parameter_names` | bool (default false) | Parameter-name hints on calls of script functions. | any |
| `completion.path_commands` | bool (default true) | Offer commands found on the `PATH`. | any; a repository can turn it off, not on again |
| `help.enabled` | bool (default true) | The `help` and `man` lookups ([below](#editor-features)). | any; a repository can turn it off, not on again |
| `help.bash_path`, `help.man_path` | string | The programs for those lookups. | user only |
| `references.resolved_only` | bool | Leave candidate sites out of find-references (`references.resolvedOnly` is accepted as an alias). | any |

bash-lsp also reads the `source-path=` entries of the nearest `.shellcheckrc`
(held to the same rules as `sources.paths`). ShellCheck itself reads the user's
own `~/.shellcheckrc`; a repository's `.shellcheckrc` is never handed to it.

## Command line

```
bash-lsp                          serve LSP on stdio (same as `serve`)
bash-lsp version
bash-lsp ast FILE [--json]        print the syntax tree of one file
bash-lsp symbols [PATH...] [--json]   functions, aliases and variables; default PATH is .
bash-lsp doctor [PATH]            workspace report: files, dialects, limits, syntax errors, unresolved sources
bash-lsp check [paths] [--format text|json] [--shellcheck auto|off] [--fail-on error|warning|info|hint] [--root DIR]
bash-lsp check --hook             PostToolUse hook of the plugin: JSON on stdin; off unless hook.enabled
bash-lsp fmt [-w] [--root DIR] [--] FILE...   format with the in-process printer
bash-lsp search [--json] [--root DIR] [--] TEXT   native name matches plus code-index hits
bash-lsp rename SYMBOL|FILE:LINE:COL NEW [--preview|--apply] [--json] [--root DIR] [--]   rename a function or variable (below)
bash-lsp mcp [--root DIR]         MCP server on stdio (docs/mcp.md)
bash-lsp help | --help | -h       usage (also `COMMAND --help`); an unknown flag is exit 2
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
(the LSP column plus one), printed and accepted alike. Two definitions of a
line are in use. Tool lines, the ones the CLI and the MCP tools print and
accept and the ones ShellCheck reports, end at `\n` only, as compilers, shells
and grep count; a lone `\r` is not a line break to them. LSP positions on the
wire end a line at `\n`, `\r\n` or a lone `\r`, as the protocol defines. The
two agree on text without a lone `\r`. Hover text and rule messages are text,
not positions: they quote tool line numbers (`Defined in a.sh:5`, `at line 3`),
which on a file with a lone `\r` differ from the editor's line numbers.
Positions are exact on such a file, but its meaning is not: the lexer reads a
lone `\r` as a blank where the shell reads an ordinary character (`foo=1\rbar=2`
is one assignment to `foo` in bash), so `rename` refuses it (see
[Refusals](#refusals)). A path printed by
`check` and `fmt` is relative to the current directory when the file is under
it, otherwise absolute. A path printed by `symbols` is relative to the PATH
argument it came from: to that directory when it is a directory, to the file's
directory when it is a file (so `symbols lib` prints `util.sh`, not
`lib/util.sh`, and a PATH is its own workspace root, with no `.git` search). A
path printed by the graph commands (`callers` to `cycles`, `status`) is
relative to the workspace root, which is the current directory unless `--root`
says otherwise; `mcp` likewise defaults to the current directory. `search`
differs: without `--root` its root is the nearest directory upward from the
current directory that holds `.git` or `.bash-lsp.toml` (the current directory
if none), and the paths it prints are relative to that root. Forward slashes
on every OS. Exit codes: 0 success, 1 runtime
failure, 2 usage error; `rename` also uses 4 for a refusal.

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
code (one exception: a file over the size limit also gets the info finding
WB1002, which counts under `--fail-on info` or `hint`): `no shell files checked: PATH: why` for a directory with nothing to check
(ignored, or no shell files), `not analysed: FILE: why` for a file that was not
parsed (over the size limit, or a shell the server does not support), the
workspace limits met, and the workspace notices.

## Turning a diagnostic off

A comment directive silences findings:

```bash
# bash-lsp disable=WB1401,WB1202 -- why, in your words
# shellcheck disable=SC2086
```

- `# bash-lsp disable=` takes rule ids (such as `WB1401`), separated by commas
  or spaces. The list runs to the end of the line, or to the first whitespace
  that is followed by `#` or `--`: what follows is your comment. Every other
  word in the list is taken as an id, and one that names no rule is reported
  (WB1008).
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

## Formatting

`bash-lsp fmt [-w] [--root DIR] [--] FILE...` formats with the in-process
printer (shfmt's), with options from the `.editorconfig` files between the
file and the root, then `[format]` of the settings, then tabs. One file
without `-w` prints the text; several list the files that would change and
exit 1 if any does; `-w` rewrites them. A file is left alone, reported, and
makes the exit code 1 when it has a syntax error, is zsh, has a byte order
mark, NUL bytes or mixed line endings, is over the size cap (2 MiB, or
`[limits] max_file_bytes` if lower), is outside the root (found from the
current directory as for `check`, unless `--root`), cannot be written, or is
an sh file that the POSIX and the bash grammar read differently (`[[ $a<$b ]]`). Every printed text is
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

## Editor features

What an LSP client gets beyond definition, references, symbols, call hierarchy,
diagnostics and formatting. Every list is capped; where the protocol has a field
for it the answer says it is incomplete, and the server logs the cut once.

| Request | What it answers |
|---|---|
| `textDocument/completion`, `completionItem/resolve` | Variables in scope (the function's locals, then the file's and the sourced files' globals, then environment names the file reads); functions and aliases in view; builtins and keywords; the operators after `${name`, each marked with the bash version it needs when the target range starts below it; files and folders after `source` or `.` (only shell files of the workspace); option flags of a command from its cached `help` or `man` page; with `completion.path_commands` (on by default) commands found on the `PATH`. At most 1,000 items, then `isIncomplete`. Resolve adds the synopsis. Completion itself starts no program. |
| `textDocument/hover` on a builtin, keyword or external command | The synopsis from the `help` builtin or the manual page, labelled with its source (below). |
| `textDocument/signatureHelp` | For a call of a script function: its name, the positional parameters its body reads (the variable each is copied into, else `$N`), `[args...]` when it reads more, the options of a `getopts` string, and its doc comment. Not for builtins, aliases, `"$cmd"` calls or commands inside `eval`. |
| `textDocument/semanticTokens/full`, `/full/delta`, `/range` | Function, variable, parameter and alias (as `macro`) tokens; builtins, external commands and names that resolve to nothing are `function` with a modifier; variables carry their attributes (`local`, `exported`, `readonly`, `integer`, `array`, `associative`, `nameref`), `declaration` and `modification`. A delta is answered only for the exact previous result id. |
| `textDocument/foldingRange`, `selectionRange` | Folds for functions, `if`, loops, `case`, subshells, brace groups, here-documents, runs of whole-line comments and `# region` / `# endregion` pairs; selection ranges from the word outwards (at most 128 levels). |
| `textDocument/documentHighlight` | Declarations and resolved uses of the symbol under the cursor in the file; candidate and dynamic sites are not highlighted. |
| `textDocument/documentLink` | A `source` or `.` word that resolves to exactly one workspace file (including `$(dirname "$0")`-relative paths), a workspace file that a command runs by path, and the interpreter of a `#!` line when that file exists; a glob loader gets none. |
| `textDocument/inlayHint` | `-> lib/util.sh` (or `-> N files`) after a `source` path that is not a plain literal: one with a `# shellcheck source=` directive, a `$(dirname "$0")`-relative path or a loop glob (`inlay_hints.source_targets`, on by default) and the parameter name before each argument of a call to a script function (`inlay_hints.parameter_names`, off by default). At most 5,000 per request. |
| `textDocument/codeAction` | ShellCheck and native quick fixes; "add a shebang" for a file whose first line is not `#!` and that no workspace file sources (quick fix); "quote this expansion" (`refactor.rewrite`). `source.fixAll` applies only fixes marked safe, and no fix is, so it is not advertised. |
| `textDocument/prepareRename`, `textDocument/rename` | The same plan as `bash-lsp rename` (below), with the excluded sites as change annotations. |

`hover` and `completionItem/resolve` may start two programs: `bash --noprofile
--norc -c 'help -- "$1"' bash NAME` for a builtin or keyword, and `man -- NAME`
for a command. The name is one argument, never part of a command line; it must
be a plain name that exists as a program on the `PATH` outside every workspace
root (or a bash builtin); the programs are found like ShellCheck (never inside
the workspace); the answer is read with a 2-second and 256 KiB limit, kept in
memory only (at most 1,024 names), redacted, and never written to disk. `--help`
is never run. Neither program runs on Windows. `doctor` reports which are
available. Set `help.enabled = false` to turn this off; `help.bash_path` and
`help.man_path` (user settings only) name the programs.

The settings keys for these features are `inlay_hints.source_targets`,
`inlay_hints.parameter_names`, `completion.path_commands`, `help.enabled`,
`help.bash_path` and `help.man_path`. A repository's `.bash-lsp.toml` can turn
`completion.path_commands` and `help.enabled` off but never on when the user
settings have them off; the two program paths are read from the user settings
and the client's initialization options, never from a repository file.

## Rename

`bash-lsp rename SYMBOL NEW` plans the rename of a function; `bash-lsp rename
FILE:LINE:COL NEW` plans the rename of the function, alias or variable named at
that position (a local variable is renamed in its function only). Preview is
the default and writes nothing; `--apply` rewrites the files. The plan lists
every place that is edited, every place that mentions the name and is left
alone, and the other symbols of the same name that stay:

```
$ bash-lsp rename build_release ship
edit  dyn.sh:4:1  build_release -> ship
edit  lib/util.sh:4:1  build_release -> ship
edit  main.sh:3:10  build_release -> ship
edit  main.sh:6:3  build_release -> ship
skip  main.sh:8:13  candidate/variable-constant  build_release
skip  main.sh:9:3  candidate/variable-constant  "$cmd"
skip  main.sh:10:9  candidate/string-eval  build_release
other orphan.sh:2:1  different symbol named build_release, not renamed
```

`edit` sites are the ones the analysis resolved to the symbol. `skip` sites
mention the name but are never edited: a call through `"$cmd"`, a name inside
an `eval` or `trap` string, a command word written with quotes or an escape
(`"f"`, `\f`: reason `inexact-name`). Each shows how the analysis resolved it,
why it is left alone, and the text there (credentials redacted, then cut to
200 bytes). `other` lines are declarations of different symbols with the same
name, which are not touched. `warn` lines say what the analysis could not see
(a `source` it could not follow, for one). `--json` prints the same plan as
`{generation, symbol, old, new, edits, excluded, others, warnings, truncated}`
with 1-based lines and UTF-16 columns, the bytes `preview_rename` returns over
MCP. Each list is cut at 10,000 records with `"truncated": true`.

Rename sees the files the workspace read. It refuses (`skipped-file`, naming
each file or directory and why) while any file or directory of the workspace
was not analysed: too large, past `[limits] max_files`, unreadable, in a
dialect the server does not support (zsh, which may still source a bash
library), or in a directory that was too deep or unreadable, because a caller
there could not be checked. The way out is to list the file or directory in
`files.exclude`, which removes the refusal (the `warn` line below counts what
the exclusion left out).
It also refuses (`unsupported-line-breaks`) when a file it would
edit contains a lone `\r`. It does not see files
you excluded: what `files.exclude`, `files.include` or a `.gitignore` leaves
out is not read, the rename goes on, and the plan carries a `warn` line saying
how many excluded files and directories were not checked.

Exit codes of `rename`: 0 done, 1 failure (a failed write, rolled back or not),
2 usage, 4 refused (every refusal, an outside-root argument included). A
refusal prints `refused: CODE: reason` on standard error, then one
`site  path:line:col` line for each place the reason is about (at most about
4,000 bytes of text, whole lines), and changes nothing. With `--json`
standard output carries the refusal as `{"applied": false, "refused": {...}}`.

### Apply

`--apply` renames by name only when no other symbol has that name: if the plan
has `other` lines it refuses with `ambiguous-apply` (exit 4, nothing written,
the plan printed) and asks for the `path:line:col` of the one to rename.
Preview by name still works and shows them. (Without this, running the same
command twice could rename the second symbol of the name.)

It writes only when every file still holds the bytes the plan was built from,
and refuses the whole rename, writing nothing, when any file of the plan:

- lies outside the root once links are resolved (`outside-root`; so does a
  `path:line:col` argument that names a file outside it);
- is not a regular file, a symbolic link included (`not-regular-file`);
- has a setuid, setgid or sticky bit (`special-mode`), which a rewrite would drop;
- has more than one hard link (`hard-link`), which a rewrite would split; this and the owner check need a Unix system and are skipped on Windows;
- is owned by another user than the one running the command, unless that is root, or has a group the rewrite cannot keep (`not-owner`): ownership is never changed silently;
- cannot be written, or lies in a directory that takes no new file (`read-only`);
- holds a NUL byte (`nul-byte`) or text that is not valid UTF-8 (`invalid-utf8`);
- differs from the text the plan was built from (`stale-file`; the comparison is of bytes, not of modification times: a file rewritten with the same bytes is not stale).

A preview flags the files an apply would refuse for a local reason with a
`warn  apply-would-refuse: CODE: file` line (and the same warning in `--json`
and in `preview_rename`); a change made after the preview shows only when the
apply runs.

What a rewrite keeps and what it does not: the permission bits (rwx, nothing
else) are kept; the owner and group are put back when they differ; line
endings are kept (only the bytes of the name change). The rename is refused
(`not-owner`, and a preview warns) when the file belongs to another user, or
to a group that is not the user's own, not one of the user's groups and not the
group of its directory (where new files take the group of their directory); if
the group still cannot be set, the apply stops while writing the temporary
files, before any file is replaced. A rewrite replaces the file by a new one:
extended attributes, ACLs, the modification time and the birth time are not
kept (a rolled-back file is a new file as well, with the original text and
permission bits but none of those), and a file with several hard links is
refused.

The write: each new text is written to a temporary file beside its target and
synced, and so is the original text of every file (for the roll back); every
file is checked once more, and again right before its own rename; the
temporary files are then renamed over their targets in path order and the
directories are synced. If a temporary file cannot be written nothing is
replaced. If a rename fails, the files already replaced are put back from the
saved originals, the directories are synced again, and every temporary file is
removed; the error prints `rolled back:` (the files put back) and `not
written:` (the files that were never replaced), exit 1, and the plan is
printed on standard output all the same. If a roll back step fails too,
`written:` lists the files that still hold the new name and, for each, a line
`mv -- 'BACKUP' 'FILE'` that restores it from the staged original: that backup
is kept, never deleted, and `--json` lists the pairs as `kept_backups`. What
is not covered: a change another program makes to a file after its last check
and before its rename is lost, with or without a roll back (the new text
replaces it, and a roll back restores the text of the snapshot, not that
change); a definition added in a file the plan does not edit after the
snapshot was read is not seen. The sites in `skip` are printed after an apply
as well, because they may still need a hand edit.

With `--json`, an apply prints the plan plus `"applied"` and `"written"`; a
failed apply or a refusal prints the same with `"applied": false`, `"refused"`
(`code`, `reason`, `sites`), and `"rolled_back"`, `"not_written"`, `"kept_backups"` and `"error"`
for a failed write, while standard error keeps the text.

### Refusals

Each refusal has a stable code. These are the codes of `refused: CODE: reason`
(the reason text may be refined; the code is the contract). The same text is
the `isError` result of `preview_rename`.

| Code | Why | Example |
|---|---|---|
| `not-a-symbol` | the name or position is not a function, alias or variable of the workspace (builtins, keywords, external commands, `$1`, environment reads) | `rename echo say` |
| `candidate-site` | the position is a site that may or may not name the symbol (a `"$cmd"` call, an `eval` string); it is not renamed, so it cannot be the start of a rename | `rename a.sh:2:5 other` on `cmd=greet`, line 2 |
| `ambiguous-symbol` | the name or position is bound to several symbols, or several functions of the name are equally widely visible, or more than 256 are; rename one by its `path:line:col` | `rename f g` with `f` defined in `a.sh` and `b.sh` |
| `invalid-name` | the new name is not `[A-Za-z_][A-Za-z0-9_]*` | `rename f 'a b'` |
| `reserved-name` | the new name is a shell builtin or keyword. For a variable the reason says so differently: bash accepts a builtin or reserved word as a variable name (checked on bash 3.2.57), and the rename is refused as a precaution | `rename f if` |
| `name-conflict` | the new name is already a name in the scope of the rename, is called as a command in view, is a special shell variable (`IFS`, `RANDOM`, `PATH`...), appears in an `eval` or `trap` string an edited function reaches, or would join the variable to a local that a caller sees under that name. Renaming a special variable is refused too | `rename f g` with `g` defined |
| `dynamic-scope` | bash resolves a variable by the call stack, so the rename could change which variable a use reads: a global renamed while a function that declares a local of that name reaches a use of it, a local that a callee reads, a prefix assignment (`x=1 cmd`) whose command reads the name, a function that declares the name and makes a call the analysis cannot follow, or a call graph too large to check | `rename a.sh:1:1 y` on `x=1; show() { echo "$x"; }; run() { local x=2; show; }` |
| `name-reference` | the name is written where the analysis finds no use of it: `unset`, `declare -n`, `export -f`, `type`, `command -v`, `test -v`, `${!name}`, `complete -F`, `PROMPT_COMMAND`, a name passed to a function that takes names, or a word of an `eval` or `trap` string | `rename f g` with `unset -f f` in the file |
| `recovered-region` | an edit lies in, or after, a region the parser had to repair or could not read, or such a region in view mentions the name; a half-typed buffer is never renamed | `rename f h` with an unterminated `"` before a call of `f` |
| `skipped-file` | a file of the workspace was skipped or could not be read (over `[limits] max_file_bytes`, past `[limits] max_files`, unreadable, or zsh) and may use the name; the refusal lists the files | `rename f h` with a `big.sh` over `[limits] max_file_bytes` that calls `f` |
| `too-many-sites` | more than 100,000 sites to edit or confirm by hand | `rename f h` with 100,001 calls of `f` |
| `no-op` | the new name is the old name | `rename f f` |
| `unsupported-line-breaks` | a file that would be edited has a lone `\r` (a `\r` not followed by `\n`) outside a comment, or such a file mentions the old or the new name: the lexer reads the `\r` as a blank, the shell as a character, so the sites and scopes found around it cannot be trusted. CRLF files and a lone `\r` inside a comment are fine. Convert the file to LF or CRLF | `rename bar zzz` on `x=1;\rbar() { :; }` |
| `ambiguous-apply` | `--apply` by name while same-named other symbols exist; apply by `path:line:col` | `rename build_release ship --apply` on the corpus (`orphan.sh` also defines it) |
| `outside-root` | the file part of the argument, or a file of the plan, lies outside the root once links are resolved | `rename ../x.sh:1:1 h` |
| `read-only` | a file or its directory cannot be written | `rename f h --apply` with `chmod 444 a.sh` |
| `invalid-utf8` | an edited file is not valid UTF-8 | a file holding a Latin-1 `caf\xe9` in a comment |
| `hard-link`, `special-mode`, `not-owner`, `nul-byte`, `not-regular-file`, `stale-file` | the apply checks above | a second name for the file (`ln a.sh b.sh`); `chmod u+s a.sh`; a file of another user; a NUL byte; a link in place of the file; an edit after the preview |

The apply checks (`stale-file`, `not-regular-file`, `outside-root`,
`read-only`, `nul-byte`, `invalid-utf8`, `hard-link`, `special-mode`,
`not-owner`) and `ambiguous-apply` come from `--apply` only; they print in the
same `refused:` form with exit 4. `preview_rename` never writes and never
raises them; it flags the local ones as `apply-would-refuse` warnings.

`inexact-name` is not a refusal: it is the reason of a `skip` line, a site
whose text is not exactly the name (`"f"`, `\f`, `f""`). Bash runs the
function, but a textual edit would have to rewrite the quoting; the site is
left for you.

What a rename does not prove: a name chosen at run time (`"$cmd"`, `eval`,
`trap` strings), a function called from a file the workspace does not hold, and
code that sources a file chosen at run time are outside what the analysis can
see; the `skip` and `warn` lines name the ones it noticed.

## JSON records

The answer of `check --format json` names its fields differently from the MCP
tool `get_diagnostics`: `check` prints `{"generation", "diagnostics": [...]}`
with `endLine`, `endCol` and `fixes` on each finding (the names of LSP), while
`get_diagnostics` returns `{"generation", "results": [...]}` with `end_line` and
`end_col`, like every other `--json` answer and MCP tool (`docs/mcp.md`). The
two are not interchangeable.

Every `--json` answer is one object that carries `generation` (the snapshot it
was computed from). Paths in the graph commands' answers are relative to the
workspace root, those in the answer of `check` follow the display rule above
(relative to the current directory when under it, else absolute); both use
forward slashes; `line` and `col` are 1-based with UTF-16 columns. The table lists,
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

## Privacy and security

**It never executes the code it analyses.** It reads files as text and builds
a tree. It does not run, source or evaluate a script, a function body or an
`eval` string, and `bash-lsp extract` and every other command only read. It
opens no network connection: `boundary_test.go` fails when any package imports a
network package (`net`, `net/http`, ...), or any package other than
`internal/extool` imports `os/exec`.

**Programs it may start.** The language server, `check` and the plugin's hook
start these, and only these:

- **ShellCheck**, to add its findings: only when it is found, and unless
  `shellcheck.enabled` is false. Without it the native findings are shown
  alone.
- **`bash --noprofile --norc -c 'help -- "$1"' bash NAME`**, to show the
  synopsis of a shell builtin or keyword on hover and in completion details.
- **`man -- NAME`**, with `MANPAGER=cat` and `PAGER=cat`, for the same on an
  external command that exists on the `PATH`.

`bash-lsp search` and the MCP tool `search_code` also start **code-index**
(`code-index search --mode fts --lang bash --k 20 --json -- TEXT`, the lexical
mode, with the text as one argument after `--`), when it is found. Nothing else
is started.

How a program is found: from the setting in the user file (`shellcheck.path`,
`help.bash_path`, `help.man_path`, `codeindex.path`: an absolute path, or a bare
name looked up on the `PATH` profile), else from the `PATH`. The result has its
links resolved, and a program that lies inside a workspace root is refused,
whether by its own path, through a link, or through a `PATH` entry that lists
a directory of the repository. A repository decides nothing about what runs.
The name asked of `help` and `man` is one separate argument, never part of a
command line, and must be a plain name; `--help` is never run (it would execute
the command being asked about). Each program runs with a filtered environment,
a neutral working directory, a time limit and an output limit (the help and man
answers: 2 seconds and 256 KiB), and on Unix in its own process group, which is
killed on timeout. Their output is redacted, kept in memory only (at most 1,024
names) and never written to disk. Nothing is started on Windows for the `help`
and `man` lookups.

What is not limited: the memory and CPU of such a program while it runs, its
writes to files and its use of the network. Only the program you configured or
installed is run, but it should be one you trust. ShellCheck reads your own
`~/.shellcheckrc`; if that sets `external-sources=true`, ShellCheck may read
files outside the workspace for a repository's `source` lines, and
`bash-lsp doctor` prints a warning. A repository's `.shellcheckrc` is never
given to it.

**Repository settings cannot turn a spawning feature on or choose a program.**
`<root>/.bash-lsp.toml` can arrive with a clone. The program paths
(`shellcheck.path`, `help.bash_path`, `help.man_path`, `codeindex.path`),
`path.profile` and `hook.enabled` are accepted only from the user file and the
client's initialization options; a repository file that names one is refused
and reported. A repository can switch ShellCheck, the `help` and `man` lookups
and `PATH` completion off, but not on again once the user turned them off, and
it can only lower the limits and the ShellCheck time limit. The hook does
nothing unless the user file has `hook.enabled = true`.

**What leaves the process.** Text that bash-lsp prints or sends outward passes
through a redaction step: every string of every MCP answer and of `search`,
every error text and log line of the MCP server, the program output shown on
hover, and the site text of a rename plan (cut to 200 bytes). It removes, by pattern:
`AKIA[0-9A-Z]{16}`, `gh[pousr]_[A-Za-z0-9]{36,}`, `github_pat_[A-Za-z0-9_]{22,}`,
`xox[abprs]-[A-Za-z0-9-]{10,}`, `-----BEGIN [A-Z ]*PRIVATE KEY-----`, and the
value of `password=`, `passwd=`, `secret=`, `token=` and `api_key=` (case
insensitive). A secret in a format not listed passes through. The MCP tools
read only inside the root: every path argument is resolved with its links and
refused when it lies outside.

**What `rename --apply` writes, and refuses.** It writes only files of its plan,
each by a temporary file beside it and a rename, after checking again that the
file still holds the bytes the plan was built from. It refuses the whole
rename, writing nothing, when a file of the plan lies outside the root once
links are resolved, is not a regular file (a symbolic link included), has a
setuid, setgid or sticky bit, has more than one hard link, belongs to another
user, cannot be written, holds a NUL byte or text that is not valid UTF-8, or
changed since the plan. It preserves permission bits, owner and group where it
can and line endings; it does not keep extended attributes, ACLs or
timestamps. If a replacement fails midway, the files already replaced are put
back from saved originals. The details and the stable refusal codes are in
[Rename](#rename).

Report a vulnerability as described in [SECURITY.md](SECURITY.md).

## Benchmark

The 28-case comparison benchmark passes 28 of 28 for bash-lsp; the installed
bash-language-server passes 11 of 28 (`bench/bench.py`). A second suite of 155
cases, with expectations written from the bash manual and the LSP typings
before any result was seen, passes 127 of 138 gating cases and 17 of 17 design
cases for bash-lsp, against 94 of 138 (14 not offered) and 7 of 17 for
bash-language-server with its default settings. The 11 failing cases are known
limits, each listed with its cause. A run oracle checks the expectations that a
run of the script can decide against real bash builds 3.2.57 to 5.3; no case
disagreed. Everything was measured on one macOS machine. The method, every
number and its result file under `bench/results/` are in
[docs/benchmark.md](docs/benchmark.md); the tools are described in
[bench/README.md](bench/README.md).

## Tests

```
go vet ./...
BASH_LSP_CONFIG=/nonexistent/bash-lsp.toml go test ./...
go test -race ./...
```

Pointing `BASH_LSP_CONFIG` at a file that does not exist keeps the tests from
reading the settings file of the machine they run on. The tests need no
network. Those that use ShellCheck skip when it is not installed. The fuzz targets (`FuzzParse`, `FuzzBind`, `FuzzEdges`,
`FuzzDiagnostics`, `FuzzFormat`, `FuzzNestingGuard` and others) run over their
seed corpus as ordinary tests; run one with, for example,
`go test -run '^$' -fuzz FuzzParse ./internal/front`. A crash-input suite
parses every known crash case in a child process with a small stack. Tests that
quote documents (the JSON records table, the library example, the cited result
files) fail when a document and the code disagree.

## Third-party software

The binary contains a modified copy of the `mvdan.cc/sh/v3/syntax` package
(BSD 3-Clause) and the Go standard library and runtime (BSD 3-Clause). The
license texts and the list of changes are in
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

## License

MIT, see [LICENSE](LICENSE). Contributing: [CONTRIBUTING.md](CONTRIBUTING.md).

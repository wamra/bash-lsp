# bash-lsp

A Bash language server written in Go. It resolves definitions, references and
symbols across `source`d files, over LSP on stdio.

Status: milestone 3 is built and not yet released. The crash on crafted
deeply nested input is closed in milestone 2a: the parser stops at 10,000
levels of nesting with a syntax error (`docs/m2a-acceptance.md`,
`docs/m2a-nesting-design.md`). Go to definition, references,
implementation, hover, call hierarchy, document and workspace symbols work
across `source`d files (static, `$(dirname "$0")`-relative and glob loaders),
through aliases, through `"$cmd"` calls, and through the strings of `eval`,
`trap` and `sh -c`, which are reported as candidates with their reason; locals
are scoped to their function, and a read in a callee that no declaration in view
explains is linked, as a candidate, to a caller's `local`.
Syntax errors are reported and the files around them still analysed. Native
lint rules with a ShellCheck bridge (`check`, and diagnostics in the editor),
a formatter (`fmt`, and LSP formatting), the graph CLI commands, `extract` (the
call graph as code-index's JSON lines), `search` (names plus code-index
full-text hits) and an MCP server (`mcp`) are in, with watched files and
several workspace folders. It passes 27 of the 28 cases of the comparison
benchmark, failing only C24 (rename); C28 (the unquoted variable diagnostic)
is green (`docs/m3-acceptance.md`). Rename is a later milestone, and known gaps
are listed in `docs/m3-acceptance.md` and `docs/m1-deferred.md`. Linking into
code-index (AC-14, AC-21) needs a patch in code-index first
(`docs/code-index-integration.md`). `check --hook` (the plugin's optional `PostToolUse` fallback, off unless
`hook.enabled` is set in the user settings) is in; SARIF output is not built yet.
There are no published binaries and no
stable interface yet, and CI has not run.

Licence: MIT (see `LICENSE`). Third-party attribution is in `NOTICE`.

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
each file or directory and why) while a file was too large, past
`[limits] max_files`, unreadable or zsh, or a directory was too deep or
unreadable, because a caller there could not be checked. It does not see files
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

## Packaging

`scripts/build-release.sh [VERSION]` cross-builds six archives and checksums into
`dist/`. `scripts/build-plugin.sh OUT [VERSION]` stages the Claude Code plugin
(see `plugin/README.md`).

# Known limits

What bash-lsp does not do, or does differently from bash, as of version
0.4.0. Each item says what you will see. The benchmark cases that fail because
of these limits are listed in [benchmark.md](benchmark.md).

## Statement order

Binding does not follow statement order. A name in file A can refer to a
definition in every file reachable from any file that reaches A, whether the
definition comes before or after the use. The consequences:

- A call before the function is defined, or before the `source` line that
  loads it, still resolves (`resolved/direct`). Bash would fail there; the
  language server does not.
- A function defined twice (`f() { ...; }` and a later `f() { ...; }`) has both
  definitions in view for every call of `f`. Go to definition answers both, and
  the call graph has one `candidate/conditional-definition` edge per
  definition. The native rule WB1103 points at the redefinition.
- A call after `unset -f NAME` stays a candidate edge to the definition, with
  reason `unset-in-view`.
- A function defined inside a subshell, command substitution or pipeline stage
  is confined to it: a call outside is a `candidate`/`conditional-definition`
  edge and a call inside is `resolved/direct`.
- A variable read inside a function that no declaration in view explains is
  linked, as a candidate, to a caller's `local` of that name (reason
  `dynamic-scope`). When a declaration is in view, the read resolves to it and
  the caller's `local` is not offered.

## Aliases

An alias is `resolved/alias` when it is used before its definition, or in a
script without `shopt -s expand_aliases`, although bash expands neither. Alias
use is order-free, so `alias aa=...; aa` on one line resolves through the
alias. `unalias NAME` is not read: a later call still resolves through the
alias.

## Functions defined by `eval`

`eval 'f() { :; }'; f` defines `f` inside a string the analysis does not
parse for declarations. Only calls are read from the strings of `eval`, `trap`
and `sh -c`, and they are reported as candidates with their reason
(`string-eval`, `string-trap`, `string-shell-c`). The call of `f` above is
reported as a command that is not found (WB1102).

## Unclosed quotes

After an unterminated quote the parser cuts at the failure, finds the next
statement boundary at column 0 (a function header, a keyword or a blank line)
and parses the rest of the file separately. Text that bash would read as part
of the quoted string is analysed as code, and functions after the quote are
found. The recovery is bounded: it re-parses from at most 50 later lines, and
a file built to make every attempt fail takes much longer to parse than an
ordinary one (a 2 MiB example takes about 17.5 s; the figure and the input are
in `bench/results/resync-cost.txt`).

Nesting deeper than 10,000 levels is a syntax error at the place the limit is
passed, and the rest of the file is still analysed. `bash-lsp ast` refuses to
print a tree deeper than 1,000 levels.

## A lone carriage return

The lexer reads a lone `\r` (one that is not followed by `\n`) as a blank,
where bash reads an ordinary character: `foo=1\rbar=2` is two assignments to
the analysis and one to bash. Positions stay exact, but the meaning does not.
Rename refuses a file with a lone `\r` outside a comment
(`unsupported-line-breaks`), also when such a file only mentions the old or the
new name. Convert the file to LF or CRLF. Every other feature (symbols,
references, diagnostics, the call graph, `check`) still reads the `\r` as a
blank. A CRLF after a backslash is read as a line continuation, where bash
sees a backslash before a `\r` and then a newline.

## What rename cannot see

- **zsh is not analysed.** A zsh file is listed in the workspace but not
  parsed. Rename refuses (`skipped-file`) while any file of the workspace was
  not analysed, because that file might call the function. A zsh file can
  source a bash library, so this is not a guess. The way out is to list the
  file in `files.exclude`.
- **Files you excluded are not checked.** What `files.exclude`,
  `files.include` or a `.gitignore` leaves out is not read. Rename goes on and
  its plan carries a warning with the number of excluded files and directories
  that were not checked. The warning repeats on every rename in a repository
  that has an ignored directory.
- Names chosen at run time (`"$cmd"`, `eval`, `trap` strings), functions called
  from files the workspace does not hold, and code that sources a file chosen
  at run time are outside what the analysis can see. The `skip` and `warn`
  lines of a rename plan name the ones it noticed.

## Other limits

- `$(else)` and `<(else)` are syntax errors on every bash version, although
  bash 3.2 to 5.1 accept them (only 5.3 rejects them).
- Every edit parses the whole file again; there is no incremental parsing.
- `textDocument/selectionRange` answers the first 1,000 positions of a
  request; each later position gets an empty range.
- `prepareRename` builds the rename plan and `rename` builds it again.
- `source.fixAll` is not offered: no code fix is marked safe to apply
  automatically.
- SARIF output is not built (`check --format sarif` exits 2).
- A ShellCheck run is bounded by time, output size and concurrency, but not by
  memory; the standard library cannot limit the memory of one child process.
  `bash-lsp doctor` says so.
- The server asks the client to watch files (`workspace/didChangeWatchedFiles`)
  by dynamic registration. A client that cannot do that gets changes only
  through `didSave` and reopening.

## Bash versions

The version rules (WB1301, WB1302) rest on a table of first versions
(`docs/version-table.md`) that was checked against real builds of bash 3.2.57,
4.0, 4.2, 4.3, 4.4, 5.0, 5.1 and 5.3 (`bench/results/version-probe-*.json`).
Bash 4.1 and 5.2 were not built, so a first version is confirmed against the
builds on either side of it, not against every release. The signatures of the
downloaded source tarballs were not verified.

## Editors

The server was tested through the language server protocol (the test suite and
the editor-request benchmarks drive it over stdio) and with Claude Code. It has
not been run in VS Code yet, and no other editor was tried.

## Platforms

macOS is the development platform and the only one the benchmarks were run on.
The Linux and Windows binaries are built (`scripts/build-release.sh` produces
all six), and the continuous-integration workflow in `.github/workflows/ci.yml`
runs `go vet` and the tests on Linux and Windows, and on macOS when started by
hand. At the time of writing that workflow has not run, so no Linux or Windows
result is recorded. Nothing is started on Windows for `help` and `man` lookups.
How Claude Code on Windows should start the plugin's `.exe` is not settled, so
launching the plugin on Windows is untested.

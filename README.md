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

## Command line

```
bash-lsp                          serve LSP on stdio (same as `serve`)
bash-lsp version
bash-lsp ast FILE [--json]        print the syntax tree of one file
bash-lsp symbols [PATH...] [--json]   functions, aliases and variables; default PATH is .
bash-lsp doctor [PATH]            workspace report: files, dialects, limits, syntax errors, unresolved sources
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

Positions are 1-based `line:col`, with columns counted in UTF-16 code units
(the LSP column plus one), printed and accepted alike. Paths are
relative to the workspace root, with forward slashes on every OS. Exit codes:
0 success, 1 runtime failure, 2 usage error.

## Packaging

`scripts/build-release.sh [VERSION]` cross-builds six archives and checksums into
`dist/`. `scripts/build-plugin.sh OUT [VERSION]` stages the Claude Code plugin
(see `plugin/README.md`).

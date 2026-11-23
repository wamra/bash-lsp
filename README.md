# bash-lsp

A Bash language server written in Go. It resolves definitions, references and
symbols across `source`d files, over LSP on stdio.

Status: milestone 1 in progress. Nothing is released yet; there are no binaries
and no stable interface.

Licence: MIT (see `LICENSE`). Third-party attribution is in `NOTICE`.

## Command line

```
bash-lsp                          serve LSP on stdio (same as `serve`)
bash-lsp version
bash-lsp ast FILE [--json]        print the syntax tree of one file
bash-lsp symbols [PATH...] [--json]   functions, aliases and variables; default PATH is .
bash-lsp doctor [PATH]            workspace report: files, dialects, limits, syntax errors, unresolved sources
```

Positions are 1-based `line:col`, with columns counted in bytes. Paths are
relative to the workspace root, with forward slashes on every OS. Exit codes:
0 success, 1 runtime failure, 2 usage error.

## Packaging

`scripts/build-release.sh [VERSION]` cross-builds six archives and checksums into
`dist/`. `scripts/build-plugin.sh OUT [VERSION]` stages the Claude Code plugin
(see `plugin/README.md`).

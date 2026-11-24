# bash-lsp plugin

Gives Claude Code a Bash language server: go to definition, find references,
hover, document and workspace symbols, and syntax diagnostics for `.sh`,
`.bash`, `.bats` and `.ksh` files, and lint findings. It reads files and never
runs them, and opens no network connection. The one program it may start is
ShellCheck, to add its findings: only when ShellCheck is found on the `PATH` or
at the `shellcheck.path` of the user settings, outside the workspace (a
ShellCheck inside the workspace is never run), and unless `shellcheck.enabled`
is false in the settings. Without it the native findings are shown alone.

## What is in the plugin

- `.lsp.json` registers the server. It starts `bin/bash-lsp serve`.
- `bin/bash-lsp` is a small `sh` launcher that picks the binary for this OS and
  CPU from `bin/bash-lsp-<os>-<arch>`. Six binaries ship: darwin, linux and
  windows, each for amd64 and arm64.

## Platforms

macOS and Linux: the launcher is a `sh` script, so `.lsp.json` works as shipped.

Windows: the binaries are built and tested in CI, but how Claude Code on Windows
should start the `.exe` is not settled (spec decision D-9). Plugin launch on
Windows is unverified.

## Build

```
scripts/build-plugin.sh OUT [VERSION]
```

stages the plugin tree, with all six binaries, into `OUT`.

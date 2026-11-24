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

- `hooks/hooks.json` registers an optional `PostToolUse` command hook
  (matcher `Write|Edit`) that runs `bin/bash-lsp check --hook`. It reads the
  hook JSON from stdin, takes the edited file from `tool_input.file_path`, and
  checks it like `bash-lsp check FILE`: the file alone, with no walk of the
  directories around it, unless a `.git` or `.bash-lsp.toml` is above it.
  Warnings and errors go to stderr with exit code 2, which Claude Code shows to
  Claude for a `PostToolUse` hook. The hook does nothing (no output, exit 0)
  unless the user settings file has `hook.enabled = true`; a repository file
  cannot enable it. It is a fallback for setups where the language server's
  pushed diagnostics do not reach the conversation.

## Settings

The user settings file is `<user config dir>/bash-lsp/config.toml` (on macOS
`~/Library/Application Support/bash-lsp/config.toml`), or the file named by
`BASH_LSP_CONFIG`. It is the one place that serves the language server, the
CLI and the hook alike, and the only place for `shellcheck.path`,
`path.profile` and `hook.enabled`. Example:

```toml
[shellcheck]
enabled = false

[hook]
enabled = true
```

The Claude Code plugins reference lists `initializationOptions` and `settings`
among the optional fields of a `.lsp.json` server. This plugin ships neither;
the same keys given as `initializationOptions` JSON (for example
`{"shellcheck":{"enabled":false}}`) are read by the language server only, so
the settings file above is the recommended place.

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

# bash-lsp plugin

Gives Claude Code a Bash language server: go to definition, find references,
hover, document and workspace symbols, call hierarchy, syntax diagnostics and
lint findings for `.sh`, `.bash`, `.bats` and `.ksh` files, and the editor
requests listed in the repository README ("Editor features": completion,
signature help, semantic tokens, folding, selection ranges, highlights, links,
inlay hints, code actions, safe rename). It reads files and never runs them,
and opens no network connection.

## Install

```
claude plugin marketplace add wamra/wammy-plugins
claude plugin install bash-lsp@wammy-plugins
```

On every push to `main`, CI builds the installable tree with
`scripts/build-plugin.sh` (the launcher, the six binaries, `.lsp.json`, the hook,
`LICENSE`, `THIRD_PARTY_NOTICES.md` and this README), checks it with
`scripts/check-plugin.sh`, and publishes it as the `plugin` branch of this
repository.

## Programs it may start

Three, and only these, each found outside the workspace (a program inside a
workspace root is refused) and run with a filtered `PATH`, a neutral working
directory, a time limit and an output limit:

- **ShellCheck**, to add its findings: only when it is found on the `PATH` or
  at the `shellcheck.path` of the user settings, and unless
  `shellcheck.enabled` is false in the settings. Without it the native findings
  are shown alone.
- **`bash --noprofile --norc -c 'help -- "$1"' bash NAME`**, to show the
  synopsis of a shell builtin or keyword on hover and in completion details.
- **`man -- NAME`**, with `MANPAGER=cat` and `PAGER=cat`, for the same on an
  external command that exists on the `PATH`.

The name is always one separate argument and must be a plain name; `--help` is
never run (it would execute the command being asked about). The output is read
with a 2-second and 256 KiB limit, redacted, kept in memory only and never
written to disk. Nothing is started on Windows. Set `help.enabled = false` in
the user settings to switch the last two off (a repository file can turn them
off too, never on); `help.bash_path` and `help.man_path` in the user settings
name the programs. `bash-lsp doctor` lists which are available.

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

Windows: the binaries are built, but how Claude Code on Windows should start
the `.exe` is not settled. Launching the plugin on Windows is untested.

## Build

```
scripts/build-plugin.sh OUT [VERSION]
```

stages the plugin tree, with all six binaries, into `OUT`.

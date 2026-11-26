# MCP server and `search`

`bash-lsp mcp [--root DIR]` serves the query engine to an MCP client over standard input and output. It is read-only: the eleven tools below read the workspace and nothing else (`preview_rename` plans a rename and never applies it). `bash-lsp search [--json] [--root DIR] [--] TEXT` is the same search as the `search_code` tool, from the command line.

## Running it

Register `bash-lsp mcp --root /path/to/repo` as a stdio server in the MCP client's settings.

Standard output carries JSON-RPC messages, one per line, and nothing else. Log lines go to standard error. The root of `mcp` defaults to the working directory. `search` without `--root` instead uses the nearest directory upward from the working directory that holds `.git` or `.bash-lsp.toml` (the working directory if none), and prints paths relative to it. Settings come from the user file (`$BASH_LSP_CONFIG`, else the user config directory) and `<root>/.bash-lsp.toml`, as for the other commands.

## Tools

| Tool | Arguments | Answer |
|---|---|---|
| `find_symbol` | `query` | functions, aliases and file-level variables whose name matches; an empty query lists functions. Each record carries the source line of the definition (`signature`, at most 200 bytes) |
| `find_references` | `symbol` | the references to a function, with how each call site resolved |
| `get_callers` | `symbol`, `depth` (default 1) | edges leading to a function or file |
| `get_callees` | `symbol`, `depth` (default 1) | edges leaving a function or file |
| `trace_dependency` | `from`, `to` | the shortest chain of calls, sources and invokes; `found` is false when there is none |
| `analyze_impact` | `target`, `depth` (default 0) | every node that transitively depends on the target |
| `list_unresolved` | `paths` (optional) | dynamic and unresolved edges |
| `get_index_status` | none | generation, file counts by dialect, skipped files, limits met |
| `search_code` | `text` | native name matches plus code-index full-text hits |
| `get_diagnostics` | `paths` (optional) | native findings; ShellCheck is not run here. Fields are `results`, `end_line` and `end_col`; `bash-lsp check --format json` prints `diagnostics`, `endLine`, `endCol` and `fixes` instead |
| `preview_rename` | `symbol`, `new_name` | the plan of a rename: `edits`, `excluded` (sites left alone, with how they resolved, why, and their text redacted and cut to 200 bytes), `others`, `warnings`. A refusal is an `isError` result whose text starts `refused: CODE: reason` followed by `site  path:line:col` lines; the codes are in the README section "Rename". Files an apply would refuse for a local reason show as `apply-would-refuse: CODE: file` warnings. It never writes: applying is `bash-lsp rename --apply` |

A symbol is a function name, a root-relative file path, or `path:line:col` with a UTF-16 column, as the other answers print positions. A name defined in several files is refused with the list of `path:line:col` specs that tell the definitions apart.

The graph answers are the documents the command line prints with `--json`, from the same encoder (`internal/query`): the same question gives the same bytes on both (`TestCLIAndMCPAnswersAreIdentical`). `get_callers` on `lib/util.sh:4:1` equals `bash-lsp callers lib/util.sh:4:1 --json`; `preview_rename` with `build_release` and `ship` equals `bash-lsp rename build_release ship --json` (`TestMCPPreviewRenameParity`). The text content of a result is that document.

Another package adds a tool by calling `mcp.Register`; a tool reaches the refreshed snapshot, the root and the settings through `mcp.SessionFrom(ctx)`.

## What bounds a call

- **Refresh.** Every `tools/call` runs `Workspace.Refresh()` first (size, mtime and hash reconcile) and answers from the new snapshot.
- **Paths.** Every path-like argument (`symbol`, `from`, `to`, `target`, each element of `paths`) is made absolute against the root, its symlinks are resolved, and it must lie inside the root (also resolved). `../../etc/passwd`, an absolute path outside, a symlink to a directory or file outside and a path through one are refused with `isError` and "outside the root". Nothing outside the root is read.
- **Nothing is executed from the workspace.** No tool starts a program of the workspace. `search_code` starts code-index, found by `extool.Find` from `codeindex.path` in the user settings or from `PATH`; a program inside the root, by its own path or after resolving links, is refused, and so is a `PATH` entry inside it.
- **Arguments.** At most 64 KiB, a JSON object nested at most 32 deep (the whole request line too), each string at most 4096 bytes without NUL, at most 256 paths. Unknown members and wrong types are refused. `depth` is 0 or more.
- **Requests.** A line over 1 MiB is read to its end without being kept and answered with an error (id null). Batch arrays get one error and none of their elements run. An id must be a string (at most 256 bytes) or a number; any other id, `null` included, is answered with an error whose id is null. A message with no id is a notification and is never answered. Unknown methods are answered with -32601, a notification method sent with an id with -32600, anything unparsable with -32700. Tools may be called without `initialize`.
- **Graph depth.** `depth` 0 or above 64 runs at 64 levels; the answer carries `"truncated": true` when more levels existed.
- **Lists.** At most 10,000 records per list, `"truncated": true` when cut.
- **Replies.** At most 1 MiB per reply line. When a reply would be longer, records are dropped from the end of the list until it fits, never bytes from the middle of the JSON, and the answer says `"truncated": true`. `truncated` is absent when nothing was cut.
- **Redaction.** Strings in every answer of the server and of `search`, every error text and every log line pass through `internal/redact`. Patterns: `AKIA[0-9A-Z]{16}`, `gh[pousr]_[A-Za-z0-9]{36,}`, `github_pat_[A-Za-z0-9_]{22,}`, `xox[abprs]-[A-Za-z0-9-]{10,}`, `-----BEGIN [A-Z ]*PRIVATE KEY-----`, and `(?i)(password|passwd|secret|token|api_?key)=\S+` (the value only). Answers are redacted string by string inside the JSON, so keys, order and delimiters are untouched. A secret in a format not listed passes through.

## `search`

The native part is `WorkspaceSymbols(text, 50)`. When code-index is found, it runs as

```
code-index search --mode fts --lang bash --k 20 --json -- TEXT
```

with the text as one argument after `--`, so a text such as `--mode=auto` cannot change the fixed flags, and no shell is involved. `--mode fts` is the lexical path and opens no network connection. The output is

```
{"generation": N, "native": [...], "codeindex": {"used": true|false, "reason": "...", "hits": [...]}}
```

A non-zero exit gives `used: false` and `reason` = the first line code-index wrote to standard error, cut to 200 bytes and redacted. The hits are code-index's own records. There is no database check beforehand.

The run goes through the same runner as ShellCheck: environment filtered to what a program needs (a `PATH` without entries inside the root, `HOME`, locale, temp), a 10 s timeout, 1 MiB of output. Its working directory is the file system root, not the workspace. code-index finds its database from the working directory, so the server names it instead with `CODE_INDEX_DB`. The value is the user's own `$CODE_INDEX_DB` when set. Otherwise it is found the way code-index's own lookup finds it from a directory inside the root: the nearest directory upward that holds a `.git` is the repository and its `.Wammy/code.sqlite` is the index (so `--root repo/scripts` finds the index of `repo`); with no `.git`, the nearest directory upward that holds `.Wammy/code.sqlite`; with none, `<root>/.Wammy/code.sqlite`. This walk upward leaves the root: a `.git` or a `.Wammy/code.sqlite` in a directory above the root is trusted as the index of this workspace, so a hostile directory above it could supply the database (it is read by code-index, never run). Set `$CODE_INDEX_DB` to pin the database. A database that is a symlink or any other non-regular file is refused (`used: false`, with the reason). Checked against a real code-index build (index made by `code-index index`, search run by `bash-lsp search` from the file system root): the hits come back and the index is the workspace's.

## Protocol facts

Read offline in the go-sdk module `github.com/modelcontextprotocol/go-sdk` v1.8.0 (`mcp/`), a reference and not a dependency.

- Methods `initialize`, `notifications/initialized`, `tools/list`, `tools/call`, `ping`: `protocol.go:2322-2338`.
- Newline-delimited JSON-RPC 2.0 on stdio: `transport.go:127` (`StdioTransport`), `transport.go:450-453` (`ioConn`). The SDK's connection also accepts batches; this server answers a batch with one error.
- `InitializeParams` `protocol.go:1036`, `InitializeResult` `protocol.go:1078`, `Tool` `protocol.go:1895`, `ListToolsResult` `protocol.go:1326`, `CallToolParams` `protocol.go:219`, `CallToolResult` `protocol.go:266`, text content `content.go:28-48`.
- Versions `2025-11-25`, `2025-06-18`, `2025-03-26`, `2024-11-05` are offered. A client's version is answered with itself when offered, else with `2025-11-25`, as `negotiatedVersion` does (`shared.go:76-90`, the supported list `shared.go:50-64`). `2026-07-28` is not offered: the SDK marks `initialize` deprecated for it (`shared.go:80-82`) and gates other behaviour on it (`client.go:319`, `server.go:782`). A client that speaks only that version cannot connect.
- A tool failure is a result with `isError: true` and the message as text; a protocol failure (unknown tool, bad parameters) is a JSON-RPC error. The result carries `content` only, no `structuredContent`.

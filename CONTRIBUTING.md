# Contributing

## Build and test

You need the Go version named in `go.mod`. No CGO is used and the module has no
dependencies.

```sh
go build -o bash-lsp ./cmd/bash-lsp
go vet ./...
BASH_LSP_CONFIG=/nonexistent/bash-lsp.toml go test ./...
```

The tests need no network. Pointing `BASH_LSP_CONFIG` at a file that does not
exist keeps them from reading your own settings file. Tests that use ShellCheck
skip when it is not installed. `go test -race ./...` is run in CI on Linux.

A few tests measure time. The budgets and growth bounds hold on a development
machine; where `CI` is set (hosted runners are shared two-core VMs) the latency
budgets are four times as long and the growth assertions only log, while the
tests keep their correctness checks.

Some tests read the documents and fail when a document and the code disagree:
the JSON records table in `README.md`, the example in `docs/library.md`, and the
result files cited by name. `docs/benchmark.md` is generated from the files
under `bench/results/` by `bench/report.py`; do not edit it by hand. The
benchmark tools are plain Python, run with `uv run --no-project python`; see
`bench/README.md`.

`scripts/build-plugin.sh OUT` and `scripts/check-plugin.sh OUT` stage and check
the installable plugin tree, and `scripts/build-release.sh` builds the release
archives.

## Pull requests

- Keep a change small and focused, with a test for new behavior.
- Anything that starts a process, reads a path from a settings file or a
  repository, or opens a network connection must respect the trust rules in the
  README ("Privacy and security"). Only `internal/extool` may start a process,
  and nothing may use the network (`boundary_test.go` enforces both). Add a test
  for it.
- Commit messages: a plain imperative summary line.
- No attribution trailers or sign-off lines are required, and there is no DCO or
  CLA. By contributing you agree that your work is released under the MIT license
  in `LICENSE`.

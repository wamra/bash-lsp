package main

import (
	"fmt"
	"io"

	"github.com/wamra/bash-lsp/internal/mcp"
	"github.com/wamra/bash-lsp/internal/query"
)

const mcpUsage = "usage: bash-lsp mcp [--root DIR]"

// cmdMCP serves the MCP tools over standard input and output until the
// client closes it. Only JSON-RPC goes to stdout; logs go to stderr.
func cmdMCP(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	root := "."
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--root":
			if i+1 >= len(args) {
				return usageErrorOf(stderr, mcpUsage, "--root needs a value")
			}
			i++
			root = args[i]
		case len(a) > len("--root=") && a[:len("--root=")] == "--root=":
			root = a[len("--root="):]
		default:
			return usageErrorOf(stderr, mcpUsage, fmt.Sprintf("unexpected argument %q", a))
		}
	}
	st := query.LoadSettings(root, stderr)
	query.NoteRefused(st, stderr)
	if err := mcp.Serve(stdin, stdout, stderr, root, st, version); err != nil {
		return fail(stderr, err)
	}
	return 0
}

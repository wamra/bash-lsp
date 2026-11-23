// Command bash-lsp is a Bash language server and analysis CLI.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/wamra/bash-lsp/internal/lsp"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

const usage = "usage: bash-lsp [serve|version]"

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	// No arguments means serve: editors start the binary bare.
	if len(args) == 0 {
		args = []string{"serve"}
	}
	switch args[0] {
	case "serve":
		if len(args) > 1 {
			fmt.Fprintf(stderr, "bash-lsp: serve takes no arguments\n%s\n", usage)
			return 2
		}
		if err := lsp.Serve(stdin, stdout, stderr, version); err != nil {
			fmt.Fprintf(stderr, "bash-lsp: %v\n", err)
			return 1
		}
		return 0
	case "version":
		fmt.Fprintf(stdout, "bash-lsp %s\n", version)
		return 0
	default:
		fmt.Fprintf(stderr, "bash-lsp: unknown command %q\n%s\n", args[0], usage)
		return 2
	}
}

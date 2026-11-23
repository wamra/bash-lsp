// Command bash-lsp is a Bash language server and analysis CLI.
package main

import (
	"fmt"
	"io"
	"os"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

const usage = "usage: bash-lsp version"

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	// No arguments is reserved for serve (added later).
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintf(stdout, "bash-lsp %s\n", version)
		return 0
	default:
		fmt.Fprintf(stderr, "bash-lsp: unknown command %q\n%s\n", args[0], usage)
		return 2
	}
}

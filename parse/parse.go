// Package parse is the public entry point for turning shell source into a
// tree.File. Feed the result to analysis.Bind and analysis.Build, or use
// analysis.Analyze to do both for a set of files.
//
// The parser stays internal: this package is the only way to reach it from
// outside the module. Parsing never fails; syntax problems are in File.Errors
// and a file that is not parsed at all says why in File.Skipped.
package parse

import (
	"github.com/wamra/bash-lsp/internal/front"
	"github.com/wamra/bash-lsp/tree"
)

// Detect picks the dialect of a file, in order: a `# shellcheck shell=NAME`
// comment in the first 5 lines; the shebang interpreter; the file extension;
// else tree.Bash.
func Detect(path string, src []byte) tree.Dialect { return front.Detect(path, src) }

// File detects the dialect of path and src, then parses. It never returns nil.
func File(path string, src []byte) *tree.File { return FileAs(path, src, Detect(path, src)) }

// FileAs parses src as dialect d. It never returns nil; File.Dialect is d.
func FileAs(path string, src []byte, d tree.Dialect) *tree.File { return front.Parse(path, src, d) }

// Fragment parses src, the body of a string that the shell runs as code
// (an `eval` argument, a `trap` action, the string of `bash -c`), as dialect
// d. The result has no path; its positions are relative to src. It never
// returns nil.
func Fragment(src []byte, d tree.Dialect) *tree.File { return front.Parse("", src, d) }

package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/wamra/bash-lsp/internal/query"
	"github.com/wamra/bash-lsp/internal/redact"
)

const searchUsage = "usage: bash-lsp search [--json] [--root DIR] [--] TEXT"

// cmdSearch is `bash-lsp search [--json] [--root DIR] [--] TEXT`. The text
// (several words are joined by one space) is one argument to the
// code-index program, after "--", and never goes through a shell.
func cmdSearch(args []string, stdout, stderr io.Writer) int {
	asJSON, root := false, ""
	var words []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			words = append(words, args[i+1:]...)
			i = len(args)
		case a == "--json":
			asJSON = true
		case a == "--root":
			if i+1 >= len(args) {
				return usageErrorOf(stderr, searchUsage, "--root needs a value")
			}
			i++
			root = args[i]
		case strings.HasPrefix(a, "--root="):
			root = strings.TrimPrefix(a, "--root=")
		case strings.HasPrefix(a, "-") && a != "-":
			return usageErrorOf(stderr, searchUsage, fmt.Sprintf("unknown flag %q for search (put a text that starts with - after --)", a))
		default:
			words = append(words, a)
		}
	}
	text := strings.Join(words, " ")
	if strings.TrimSpace(text) == "" {
		return usageErrorOf(stderr, searchUsage, "search needs a text")
	}
	if len(text) > query.MaxText {
		return usageErrorOf(stderr, searchUsage, fmt.Sprintf("the text is longer than %d bytes", query.MaxText))
	}
	searchDir, _ := findRoot(nil, root)
	ws, st, prof, err := query.Open(searchDir, stderr)
	if err != nil {
		return fail(stderr, err)
	}
	query.NoteRefused(st, stderr)
	doc := query.Search(context.Background(), ws.Snapshot(), ws.Root(), text, st, prof)
	if asJSON {
		b, err := query.Marshal(doc)
		if err != nil {
			return fail(stderr, err)
		}
		if _, err := stdout.Write(redact.JSON(b)); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	for _, l := range doc.Lines() {
		fmt.Fprintln(stdout, redact.String(l))
	}
	return 0
}

func usageErrorOf(stderr io.Writer, usage, msg string) int {
	fmt.Fprint(stderr, redact.String(fmt.Sprintf("bash-lsp: %s\n%s\n", msg, usage)))
	return 2
}

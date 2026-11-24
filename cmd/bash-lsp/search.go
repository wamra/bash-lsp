package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/wamra/bash-lsp/internal/config"
	"github.com/wamra/bash-lsp/internal/extool"
	"github.com/wamra/bash-lsp/internal/query"
	"github.com/wamra/bash-lsp/internal/workspace"
)

const searchUsage = "usage: bash-lsp search [--json] [--root DIR] [--] TEXT"

// searchRoot is the root `search` works in. Kept apart so the root rules the
// other commands share can replace it in one place.
func searchRoot(flagRoot string) string { return flagRoot }

// openSettled opens root and returns what the workspace was opened with: the
// settings (user file, then root/.bash-lsp.toml) and the PATH profile.
func openSettled(root string, stderr io.Writer) (*workspace.Workspace, config.Settings, *extool.Profile, error) {
	st := config.Load(root, nil)
	for _, w := range st.Warnings {
		fmt.Fprintf(stderr, "bash-lsp: settings: %s\n", w)
	}
	prof := extool.ProfileFromEnv()
	if st.PathProfile != nil {
		prof = extool.NewProfile(st.PathProfile)
	}
	ws, err := workspace.OpenWith(root, st.Limits(), nil, st.Workspace(prof.Commands()))
	return ws, st, prof, err
}

// cmdSearch is `bash-lsp search [--json] [--root DIR] [--] TEXT`. The text
// (several words are joined by one space) is one argument to the
// code-index program, after "--", and never goes through a shell.
func cmdSearch(args []string, stdout, stderr io.Writer) int {
	asJSON, root := false, "."
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
	ws, st, prof, err := openSettled(searchRoot(root), stderr)
	if err != nil {
		return fail(stderr, err)
	}
	doc := query.Search(context.Background(), ws.Snapshot(), ws.Root(), text, st, prof)
	if asJSON {
		if err := query.Write(stdout, doc); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	for _, l := range doc.Lines() {
		fmt.Fprintln(stdout, l)
	}
	return 0
}

func usageErrorOf(stderr io.Writer, usage, msg string) int {
	fmt.Fprintf(stderr, "bash-lsp: %s\n%s\n", msg, usage)
	return 2
}

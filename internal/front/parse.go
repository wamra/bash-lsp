package front

import (
	"mvdan.cc/sh/v3/syntax"

	"github.com/wamra/bash-lsp/tree"
)

func variant(d Dialect) syntax.LangVariant {
	switch d {
	case POSIX:
		return syntax.LangPOSIX
	case Mksh:
		return syntax.LangMirBSDKorn
	case Bats:
		return syntax.LangBats
	case Zsh:
		return syntax.LangZsh
	}
	return syntax.LangBash
}

// Parse never returns nil. For Zsh it returns a File with Skipped set and no
// statements. After an error the parser cannot recover from, the tree holds the
// statements before it, a KErrorGap over the broken region, and (when a later
// line parses cleanly) the statements after it; see resync.go.
func Parse(path string, src []byte, d Dialect) *tree.File {
	f := tree.NewFile(path, src)
	if d == Zsh {
		f.Skipped = "zsh is not supported yet"
		return f
	}
	stmts, errs, comments := parseFile(f, d)
	f.Root.Children = stmts
	for _, s := range stmts {
		s.Parent = f.Root
	}
	f.Errors = errs
	f.Comments = comments
	for _, c := range f.Comments {
		c.Parent = f.NodeAt(c.Range.Start.Offset)
	}
	return f
}

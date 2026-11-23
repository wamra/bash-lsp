package front

import (
	"github.com/wamra/bash-lsp/internal/front/syntax"

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
// statements. Nesting (or a chain of operators, or an elif ladder) deeper
// than the parser's 10,000 levels is a syntax error like any other, with
// the parser's text ("nesting is deeper than 10000 levels"). After an error
// the parser cannot recover from, the tree holds the statements before it, a KErrorGap
// over the broken region, and (when a later line parses cleanly) the
// statements after it; see resync.go.
func Parse(path string, src []byte, d Dialect) *tree.File {
	f := tree.NewFile(path, src)
	f.Dialect = d
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
	loc := f.NewLocator()
	for _, c := range f.Comments {
		c.Parent = loc.At(c.Range.Start.Offset)
	}
	return f
}

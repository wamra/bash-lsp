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

// nestingError is the parser's error for input nested deeper than its limit
// (maxNesting in internal/front/syntax).
const nestingError = "nesting is deeper than 10000 levels"

// Parse never returns nil. For Zsh it returns a File with Skipped set and no
// statements. After an error the parser cannot recover from, the tree holds
// the statements before it, a KErrorGap over the broken region, and (when a
// later line parses cleanly) the statements after it; see resync.go.
//
// Nesting (or a chain of operators, or an elif ladder) deeper than the
// parser's 10,000 levels is such an error, with the parser's text
// (nestingError). Only when it leaves no statement at all is the file
// skipped: Skipped is then that text, and the file has its errors and no
// statements.
func Parse(path string, src []byte, d Dialect) *tree.File {
	f := tree.NewFile(path, src)
	f.Dialect = d
	if d == Zsh {
		f.Skipped = "zsh is not supported yet"
		return f
	}
	stmts, errs, comments, limits := parseFile(f, d)
	f.Errors = errs
	f.Limits = limits
	if nothingBut(stmts, tree.KErrorGap) && errorWith(errs, nestingError) {
		f.Skipped = nestingError
		return f
	}
	f.Root.Children = stmts
	for _, s := range stmts {
		s.Parent = f.Root
	}
	f.Comments = comments
	loc := f.NewLocator()
	for _, c := range f.Comments {
		c.Parent = loc.At(c.Range.Start.Offset)
	}
	return f
}

// nothingBut reports whether every node of stmts is of kind k.
func nothingBut(stmts []*tree.Node, k tree.Kind) bool {
	for _, s := range stmts {
		if s.Kind != k {
			return false
		}
	}
	return true
}

// errorWith reports whether one of errs has exactly the message msg.
func errorWith(errs []tree.SyntaxError, msg string) bool {
	for _, e := range errs {
		if e.Msg == msg {
			return true
		}
	}
	return false
}

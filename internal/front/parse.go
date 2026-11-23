package front

import (
	"fmt"

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
// statements; likewise for a file that nests too deeply to parse safely
// (scan.go), which also carries one error saying so. After an error the parser
// cannot recover from, the tree holds the statements before it, a KErrorGap
// over the broken region, and (when a later line parses cleanly) the
// statements after it; see resync.go.
func Parse(path string, src []byte, d Dialect) *tree.File {
	f := tree.NewFile(path, src)
	if d == Zsh {
		f.Skipped = "zsh is not supported yet"
		return f
	}
	if deep, at := nestingTooDeep(src); deep {
		msg := fmt.Sprintf("nesting deeper than %d levels: file not parsed", maxNesting)
		f.Skipped = msg
		p := f.PosAt(at)
		f.Errors = []tree.SyntaxError{{Range: tree.Range{Start: p, End: p}, Msg: msg}}
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

package front

import (
	"bytes"
	"errors"
	"strings"

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
// statements. On a hard parser error the tree holds the statements before the
// error and Errors has one entry (no resync yet).
func Parse(path string, src []byte, d Dialect) *tree.File {
	f := tree.NewFile(path, src)
	if d == Zsh {
		f.Skipped = "zsh is not supported yet"
		return f
	}
	p := syntax.NewParser(syntax.KeepComments(true), syntax.RecoverErrors(50), syntax.Variant(variant(d)))
	af, err := p.Parse(bytes.NewReader(src), path)
	if af == nil {
		af = &syntax.File{}
	}
	base := f.PosAt(0)
	stmts, errs := lowerStmts(f, base, af.Stmts)
	f.Root.Children = stmts
	for _, s := range stmts {
		s.Parent = f.Root
	}
	f.Errors = errs
	f.Comments = lowerComments(f, base, af)
	for _, c := range f.Comments {
		c.Parent = f.NodeAt(c.Range.Start.Offset)
	}
	if err != nil {
		f.Errors = append(f.Errors, hardError(f, err))
	}
	return f
}

// hardError converts a parser error that recovery could not absorb.
func hardError(f *tree.File, err error) tree.SyntaxError {
	var (
		pe syntax.ParseError
		le syntax.LangError
	)
	switch {
	case errors.As(err, &pe):
		at := f.PosAt(int(pe.Pos.Offset()))
		return tree.SyntaxError{Range: tree.Range{Start: at, End: at}, Msg: pe.Text}
	case errors.As(err, &le):
		at := f.PosAt(int(le.Pos.Offset()))
		le.Filename = ""
		return tree.SyntaxError{Range: tree.Range{Start: at, End: at}, Msg: strings.TrimPrefix(le.Error(), le.Pos.String()+": ")}
	}
	at := f.PosAt(len(f.Src))
	return tree.SyntaxError{Range: tree.Range{Start: at, End: at}, Msg: err.Error()}
}

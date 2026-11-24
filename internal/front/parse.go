package front

import (
	"bytes"

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
	stmts, errs, comments, feats, limits := parseFile(f, d)
	if d == POSIX {
		stmts, errs, comments, feats, limits = shGrammar(f, stmts, errs, comments, feats, limits)
	}
	f.Errors = errs
	f.Limits = limits
	f.Features = feats
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

// bashHints are the texts of the bash forms an sh parse reads without an error
// (`[[` and `source` are ordinary commands to it, `coproc` a command name,
// `$'` a quote and a word).
var bashHints = [][]byte{[]byte("[["), []byte("$'"), []byte("coproc"), []byte("source")}

// shGrammar settles which grammar reads an sh file. The POSIX grammar comes
// first: it is the language the file claims, and the bash grammar reads some
// valid sh differently (`((a; b) | c)` is an arithmetic command, `coproc`,
// `function` and `select` are keywords). When the POSIX parse has errors
// the file is read again with the bash grammar and that result is used if it
// has fewer errors, so a bash-only form is the evidence of a dialect finding
// (WB1302) instead of a cascade of syntax errors. When the POSIX parse is
// clean but the text holds a form the POSIX grammar takes for a command, a
// bash parse supplies the file's features (only if it is clean too); the tree
// stays the POSIX one. The cost is a second parse of the file, only for an
// sh file with errors or with one of bashHints.
func shGrammar(f *tree.File, stmts []*tree.Node, errs []tree.SyntaxError, comments []*tree.Node, feats []tree.Feature, limits []tree.Limit) ([]*tree.Node, []tree.SyntaxError, []*tree.Node, []tree.Feature, []tree.Limit) {
	if len(errs) == 0 && !hasHint(f.Src) {
		return stmts, errs, comments, feats, limits
	}
	r := newResyncer(f, POSIX)
	r.lang = syntax.LangBash
	bs, be, bc, bf, bl := r.run()
	switch {
	case len(errs) > 0 && len(be) < len(errs):
		return bs, be, bc, bf, bl
	case len(errs) == 0 && len(be) == 0:
		return stmts, errs, comments, bf, limits
	}
	return stmts, errs, comments, feats, limits
}

func hasHint(src []byte) bool {
	for _, h := range bashHints {
		if bytes.Contains(src, h) {
			return true
		}
	}
	return false
}

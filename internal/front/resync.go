package front

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/wamra/bash-lsp/internal/front/syntax"

	"github.com/wamra/bash-lsp/tree"
)

// maxResyncAttempts bounds how many candidate lines are parsed per file.
const maxResyncAttempts = 50

// resyncer recovers a tree for the code after a syntax error the parser could
// not absorb: it cuts out the broken region as a KErrorGap and parses the file
// again from the next line that looks like the start of a statement.
type resyncer struct {
	f      *tree.File
	d      Dialect
	budget int // candidate lines still allowed, shared by the whole file
	nul    nulIndex
}

// attempt is one parser run over f.Src[off:], lowered to absolute positions.
type attempt struct {
	stmts    []*tree.Node
	errs     []tree.SyntaxError // lowering errors; excludes the failure itself
	comments []*tree.Node
	failOff  int // absolute offset of the failure, -1 when the run was clean
	failErr  tree.SyntaxError
}

// parseFile parses all of f.Src and returns its statements, errors and
// comments.
func parseFile(f *tree.File, d Dialect) (stmts []*tree.Node, errs []tree.SyntaxError, comments []*tree.Node) {
	r := &resyncer{f: f, d: d, budget: maxResyncAttempts, nul: newNULIndex(f.Src)}
	return r.resolve(r.raw(0))
}

// raw runs the parser once over f.Src[off:]. It does not resync.
func (r *resyncer) raw(off int) attempt {
	f := r.f
	af, err, crashed := parseSafely(f.Src[off:], f.Path, r.d)
	if af == nil {
		af = &syntax.File{}
	}
	base := f.PosAt(off)
	a := attempt{failOff: -1}
	a.stmts, a.errs = lowerStmts(f, base, af.Stmts, r.nul)
	a.comments = lowerComments(f, base, af)
	switch {
	case crashed:
		// No position to go by: the broken region starts where this run did.
		a.failErr = tree.SyntaxError{Range: tree.Range{Start: base, End: base}, Msg: err.Error()}
		a.failOff = base.Offset
	case err != nil:
		a.failErr = hardError(f, base.Offset, err)
		a.failOff = a.failErr.Range.Start.Offset
	}
	if q := unclosedQuote(af); q >= 0 && (a.failOff < 0 || off+q < a.failOff) {
		at := f.PosAt(off + q)
		a.failOff = at.Offset
		a.failErr = tree.SyntaxError{Range: tree.Range{Start: at, End: at}, Msg: "unterminated quote"}
	}
	return a
}

// parseSafely runs the parser over src. The parser can panic on some broken
// input (`A=$((0 A` indexes past a slice); crashed reports that, with err
// describing it, instead of letting the panic end the process.
//
// What cannot be recovered from, a stack overflow, the parser rules out
// itself: it counts nesting and returns an error past 10,000 levels
// (internal/front/syntax, docs/m2a-nesting-design.md).
func parseSafely(src []byte, path string, d Dialect) (af *syntax.File, err error, crashed bool) {
	defer func() {
		if r := recover(); r != nil {
			af, err, crashed = nil, fmt.Errorf("parser failure: %v", r), true
		}
	}()
	p := syntax.NewParser(syntax.KeepComments(true), syntax.RecoverErrors(50), syntax.Variant(variant(d)))
	af, err = p.Parse(bytes.NewReader(src), path)
	return af, err, false
}

// unclosedQuote returns the offset (relative to the parsed text) of the first
// quoted string whose closing quote the parser had to invent, or -1.
func unclosedQuote(af *syntax.File) (off int) {
	off = -1
	defer func() { _ = recover() }() // a partial tree may hold nil nodes
	visitAll(af, func(n syntax.Node) {
		var left, right syntax.Pos
		switch q := n.(type) {
		case *syntax.DblQuoted:
			left, right = q.Left, q.Right
		case *syntax.SglQuoted:
			left, right = q.Left, q.Right
		default:
			return
		}
		if left.IsValid() && (!right.IsValid() || right.IsRecovered()) && (off < 0 || int(left.Offset()) < off) {
			off = int(left.Offset())
		}
	})
	return off
}

// cut returns the offset where the broken region starts and the statements
// that lie wholly before it. A clean run keeps everything (gapStart is len(Src)).
func (a attempt) cut(f *tree.File) (gapStart int, kept []*tree.Node) {
	if a.failOff < 0 {
		return len(f.Src), a.stmts
	}
	at := f.PosAt(a.failOff)
	gapStart = at.Offset - (at.Col - 1) // start of the failing line
	for _, s := range a.stmts {
		if s.Range.Start.Offset >= at.Offset-(at.Col-1) {
			gapStart = s.Range.Start.Offset // the last such statement wins
		}
	}
	for _, s := range a.stmts {
		if s.Range.Start.Offset >= gapStart || s.Range.End.Offset > gapStart {
			break
		}
		kept = append(kept, s)
	}
	return gapStart, kept
}

// resolve turns a run into final statements, resyncing after a failure.
func (r *resyncer) resolve(a attempt) ([]*tree.Node, []tree.SyntaxError, []*tree.Node) {
	f := r.f
	if a.failOff < 0 {
		return a.stmts, a.errs, a.comments
	}
	gapStart, kept := a.cut(f)
	var errs []tree.SyntaxError
	for _, e := range a.errs {
		if e.Range.Start.Offset < gapStart {
			errs = append(errs, e)
		}
	}
	errs = append(errs, a.failErr)
	var comments []*tree.Node
	for _, c := range a.comments {
		if c.Range.Start.Offset < gapStart {
			comments = append(comments, c)
		}
	}

	src := f.Src
	// Candidates start after both the failure and the dropped statements: a
	// gap must not end before it starts.
	for pos := nextLine(src, max(a.failOff, gapStart)); pos < len(src) && r.budget > 0; pos = nextLine(src, pos) {
		if !resyncPoint(src, pos) {
			continue
		}
		r.budget--
		t := r.raw(pos)
		_, tailKept := t.cut(f)
		if len(tailKept) == 0 || errorAt(t.errs, tailKept[0].Range.Start.Offset) {
			continue
		}
		gap := r.gap(gapStart, pos, true)
		tStmts, tErrs, tComments := r.resolve(t)
		kept = append(kept, gap)
		return append(kept, tStmts...), append(errs, tErrs...), append(comments, tComments...)
	}
	return append(kept, r.gap(gapStart, len(src), false)), errs, comments
}

func (r *resyncer) gap(from, to int, recovered bool) *tree.Node {
	return &tree.Node{Kind: tree.KErrorGap, Recovered: recovered, Range: tree.Range{Start: r.f.PosAt(from), End: r.f.PosAt(to)}}
}

// nextLine returns the offset of the first line start after the line holding off.
func nextLine(src []byte, off int) int {
	if off >= len(src) {
		return len(src)
	}
	i := bytes.IndexByte(src[off:], '\n')
	if i < 0 {
		return len(src)
	}
	return off + i + 1
}

// errorAt reports whether an error starts exactly at offset.
func errorAt(errs []tree.SyntaxError, offset int) bool {
	for _, e := range errs {
		if e.Range.Start.Offset == offset {
			return true
		}
	}
	return false
}

var closers = [][]byte{[]byte("fi"), []byte("done"), []byte("esac"), []byte("then"), []byte("else"), []byte("elif"), []byte("do")}

// resyncPoint reports whether the line at pos can start a fresh statement: it
// begins at column 1 with a non-space character and not with a token that only
// closes or continues a construct.
func resyncPoint(src []byte, pos int) bool {
	rest := src[pos:]
	switch rest[0] {
	case ' ', '\t', '\r', '\n', '}', ')':
		return false
	}
	if bytes.HasPrefix(rest, []byte(";;")) {
		return false
	}
	for _, k := range closers {
		if bytes.HasPrefix(rest, k) && (len(rest) == len(k) || !isWordByte(rest[len(k)])) {
			return false
		}
	}
	return true
}

func isWordByte(c byte) bool {
	return c == '_' || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

// hardError converts a parser error that recovery could not absorb. Parser
// offsets are relative to the parsed text, which starts at file offset base.
func hardError(f *tree.File, base int, err error) tree.SyntaxError {
	var (
		pe syntax.ParseError
		le syntax.LangError
	)
	switch {
	case errors.As(err, &pe):
		at := f.PosAt(base + int(pe.Pos.Offset()))
		return tree.SyntaxError{Range: tree.Range{Start: at, End: at}, Msg: pe.Text}
	case errors.As(err, &le):
		at := f.PosAt(base + int(le.Pos.Offset()))
		le.Filename = ""
		return tree.SyntaxError{Range: tree.Range{Start: at, End: at}, Msg: trimPos(le)}
	}
	at := f.PosAt(len(f.Src))
	return tree.SyntaxError{Range: tree.Range{Start: at, End: at}, Msg: err.Error()}
}

func trimPos(le syntax.LangError) string {
	s := le.Error()
	if p := le.Pos.String() + ": "; len(s) >= len(p) && s[:len(p)] == p {
		return s[len(p):]
	}
	return s
}

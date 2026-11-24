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

// The re-parses of one file may read at most resyncByteBudget(len(src))
// bytes in all: a 4 MiB floor (50 re-parses of an 80 KiB file, so files up to
// that size are bounded by maxResyncAttempts alone) plus twice the file. A
// candidate whose tail would not fit in what is left is not tried. Bytes, not
// time, so the result does not depend on the machine.
const (
	resyncByteFloor  = 4 << 20
	resyncByteFactor = 2
)

func resyncByteBudget(size int) int { return resyncByteFloor + resyncByteFactor*size }

// resyncer recovers a tree for the code after a syntax error the parser could
// not absorb: it cuts out the broken region as a KErrorGap and parses the file
// again from the next line that looks like the start of a statement.
type resyncer struct {
	f         *tree.File
	d         Dialect
	budget    int // candidate lines still allowed, shared by the whole file
	bytesLeft int // bytes the candidate re-parses may still read
	parsed    int // bytes the candidate re-parses have read
	reads     []int
	limits    []tree.Limit
	nul       nulIndex
}

// attempt is one parser run over f.Src[off:], lowered to absolute positions.
type attempt struct {
	stmts    []*tree.Node
	errs     []tree.SyntaxError // lowering errors; excludes the failure itself
	feats    []tree.Feature     // features of the statements lowered, in source order
	comments []*tree.Node
	failOff  int // absolute offset of the failure, -1 when the run was clean
	failErr  tree.SyntaxError
}

// parseFile parses all of f.Src and returns its statements, errors, comments
// and the work bounds resync hit.
func parseFile(f *tree.File, d Dialect) (stmts []*tree.Node, errs []tree.SyntaxError, comments []*tree.Node, feats []tree.Feature, limits []tree.Limit) {
	return newResyncer(f, d).run()
}

func newResyncer(f *tree.File, d Dialect) *resyncer {
	return &resyncer{f: f, d: d, budget: maxResyncAttempts, bytesLeft: resyncByteBudget(len(f.Src)), nul: newNULIndex(f.Src)}
}

// run parses the whole file (not charged to the byte budget) and resyncs
// after a failure.
func (r *resyncer) run() ([]*tree.Node, []tree.SyntaxError, []*tree.Node, []tree.Feature, []tree.Limit) {
	a, _ := r.raw(0)
	stmts, errs, comments, feats := r.resolve(a)
	return stmts, errs, comments, feats, r.limits
}

// raw runs the parser once over f.Src[off:] and returns the run and the
// bytes the parser read. It does not resync.
func (r *resyncer) raw(off int) (attempt, int) {
	f := r.f
	af, read, err, crashed := parseSafely(f.Src[off:], f.Path, r.d)
	if af == nil {
		af = &syntax.File{}
	}
	base := f.PosAt(off)
	a := attempt{failOff: -1}
	a.stmts, a.errs, a.feats = lowerStmts(f, base, af.Stmts, r.nul)
	var cmtPanic bool
	a.comments, cmtPanic = lowerComments(f, base, af)
	switch {
	case crashed:
		// No position to go by: the broken region starts where this run did.
		a.failErr = tree.SyntaxError{Range: tree.Range{Start: base, End: base}, Msg: err.Error(), Internal: true}
		a.failOff = base.Offset
	case err != nil:
		a.failErr = hardError(f, base.Offset, err)
		a.failOff = a.failErr.Range.Start.Offset
	}
	q, quotePanic := unclosedQuote(af)
	// After a hard error a partial tree may hold nil nodes, and the region is
	// a gap anyway; on a clean run a panic in these scans is a defect.
	if clean := err == nil && !crashed; clean && cmtPanic {
		at := base
		if n := len(a.comments); n > 0 {
			at = a.comments[n-1].Range.Start
		}
		a.errs = append(a.errs, tree.SyntaxError{Range: tree.Range{Start: at, End: at}, Internal: true,
			Msg: fmt.Sprintf("internal error: comments after line %d not recorded", at.Line)})
	}
	if clean := err == nil && !crashed; clean && quotePanic {
		a.errs = append(a.errs, tree.SyntaxError{Range: tree.Range{Start: base, End: base}, Internal: true, Msg: "internal error: quote check"})
	}
	if q >= 0 && (a.failOff < 0 || off+q < a.failOff) {
		at := f.PosAt(off + q)
		a.failOff = at.Offset
		a.failErr = tree.SyntaxError{Range: tree.Range{Start: at, End: at}, Msg: "unterminated quote"}
	}
	return a, read
}

// parseSafely runs the parser over src. The parser can panic on some broken
// input (`A=$((0 A` indexes past a slice); crashed reports that, with err
// describing it, instead of letting the panic end the process.
//
// What cannot be recovered from, a stack overflow, the parser rules out
// itself: it counts nesting and returns an error past 10,000 levels
// (internal/front/syntax, docs/m2a-nesting-design.md).
//
// read is the number of bytes the parser took from its reader (it reads in
// chunks, so a run that stops at an early error reads little of a long tail).
func parseSafely(src []byte, path string, d Dialect) (af *syntax.File, read int, err error, crashed bool) {
	rd := bytes.NewReader(src)
	defer func() {
		if r := recover(); r != nil {
			af, read, err, crashed = nil, len(src)-rd.Len(), fmt.Errorf("parser failure: %v", r), true
		}
	}()
	p := syntax.NewParser(syntax.KeepComments(true), syntax.RecoverErrors(50), syntax.Variant(variant(d)))
	af, err = p.Parse(rd, path)
	return af, len(src) - rd.Len(), err, false
}

// unclosedQuote returns the offset (relative to the parsed text) of the first
// quoted string whose closing quote the parser had to invent, or -1.
// panicked reports a recovered panic (a partial tree may hold nil nodes).
func unclosedQuote(af *syntax.File) (off int, panicked bool) {
	off = -1
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
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
	return off, false
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
	// A statement that starts before the gap and runs into it is dropped: the
	// gap starts where it starts, so no code goes missing without a gap.
	for _, s := range a.stmts {
		if s.Range.Start.Offset < gapStart && s.Range.End.Offset > gapStart {
			gapStart = s.Range.Start.Offset
			break
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
func (r *resyncer) resolve(a attempt) ([]*tree.Node, []tree.SyntaxError, []*tree.Node, []tree.Feature) {
	f := r.f
	if a.failOff < 0 {
		return a.stmts, a.errs, a.comments, a.feats
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

	var feats []tree.Feature
	for _, ft := range a.feats {
		if ft.Range.Start.Offset < gapStart {
			feats = append(feats, ft)
		}
	}

	src := f.Src
	// Candidates start after both the failure and the dropped statements: a
	// gap must not end before it starts. The loop ends when no candidate is
	// left, or before a candidate when the attempts or the bytes are used up.
	stop := ""
	for pos := nextLine(src, max(a.failOff, gapStart)); pos < len(src); pos = nextLine(src, pos) {
		if !resyncPoint(src, pos) {
			continue
		}
		if r.budget <= 0 {
			stop = "resync-attempts"
			break
		}
		if len(src)-pos > r.bytesLeft {
			stop = "resync-bytes"
			break
		}
		r.budget--
		t, read := r.raw(pos)
		r.reads = append(r.reads, read)
		r.parsed += read
		r.bytesLeft -= read
		_, tailKept := t.cut(f)
		if len(tailKept) == 0 || errorAt(t.errs, tailKept[0].Range.Start.Offset) {
			continue
		}
		gap := r.gap(gapStart, pos, true)
		tStmts, tErrs, tComments, tFeats := r.resolve(t)
		kept = append(kept, gap)
		return append(kept, tStmts...), append(errs, tErrs...), append(comments, tComments...), append(feats, tFeats...)
	}
	gap := r.gap(gapStart, len(src), false)
	switch stop {
	case "resync-attempts":
		r.limits = append(r.limits, tree.Limit{Name: stop, Range: gap.Range,
			Detail: fmt.Sprintf("resync stopped after %d attempts", maxResyncAttempts)})
	case "resync-bytes":
		r.limits = append(r.limits, tree.Limit{Name: stop, Range: gap.Range,
			Detail: fmt.Sprintf("resync stopped after re-parsing %d bytes (budget %d for a %d-byte file)", r.parsed, resyncByteBudget(len(src)), len(src))})
	}
	return append(kept, gap), errs, comments, feats
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

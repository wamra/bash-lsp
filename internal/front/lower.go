package front

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/wamra/bash-lsp/tree"
)

// lowerer turns mvdan nodes into tree nodes. Every parser offset is shifted by
// base.Offset and converted with f.PosAt, so a tail parsed separately lands at
// its true place in the whole file.
type lowerer struct {
	f      *tree.File
	base   tree.Pos
	errs   []tree.SyntaxError
	cursor int // largest absolute end offset produced so far: "nearest valid position"

	// Nodes and the arrays behind their Children come from chunks that grow as
	// the lowering goes, instead of one heap object each: a node is 176 bytes
	// and a 561 KB script has about 220,000 of them.
	nodes []tree.Node
	ptrs  []*tree.Node

	depth int // recursion depth of stmt, word, part and ifClause; see maxDepth
}

// maxDepth bounds the lowerer's recursion. A statement nested deeper is
// dropped and reported, so the tree handed to analysis is at most about
// 3*maxDepth nodes deep (a level adds a statement, its command and a block at
// most) and every recursive walk over it (tree.Node.Walk, Bind, hover, the
// ast view) stays within a few megabytes of stack. The parser has already
// accepted the statement by then: Parse's pre-scan (scan.go) bounds what it
// recurses on. The whole-file scans over the parser's tree do not recurse
// (visit.go).
const maxDepth = 4000

// tooDeep is the panic value of a lowering that crossed maxDepth.
type tooDeep struct{}

func (l *lowerer) enter() {
	l.depth++
	if l.depth > maxDepth {
		panic(tooDeep{})
	}
}

const (
	firstChunk = 32 // nodes (and child pointers) in the first chunk
	maxChunk   = 64 // chunks double up to this many entries
)

// newNode returns a zeroed node from the current chunk.
func (l *lowerer) newNode() *tree.Node {
	if len(l.nodes) == cap(l.nodes) {
		l.nodes = make([]tree.Node, 0, nextChunk(cap(l.nodes)))
	}
	l.nodes = l.nodes[:len(l.nodes)+1]
	return &l.nodes[len(l.nodes)-1]
}

// kidSlice returns an empty slice with capacity n carved from the current
// pointer chunk. The capacity is capped, so an append by a caller copies
// instead of writing into the neighbour's array.
func (l *lowerer) kidSlice(n int) []*tree.Node {
	if cap(l.ptrs)-len(l.ptrs) < n {
		c := nextChunk(cap(l.ptrs))
		for c < n {
			c *= 2
		}
		l.ptrs = make([]*tree.Node, 0, c)
	}
	at := len(l.ptrs)
	l.ptrs = l.ptrs[:at+n]
	return l.ptrs[at : at : at+n]
}

func nextChunk(prev int) int {
	if prev < firstChunk {
		return firstChunk
	}
	if prev >= maxChunk {
		return maxChunk
	}
	return prev * 2
}

// lowerStmts lowers a statement list parsed from f.Src[base.Offset:]. A
// statement whose lowering panics is dropped and reported rather than taking
// the whole file down. Callers use the returned errors to extend File.Errors.
func lowerStmts(f *tree.File, base tree.Pos, stmts []*syntax.Stmt) ([]*tree.Node, []tree.SyntaxError) {
	l := &lowerer{f: f, base: base, cursor: base.Offset}
	var out []*tree.Node
	for _, s := range stmts {
		if n := l.safeStmt(s); n != nil {
			out = append(out, n)
		}
	}
	return out, l.errs
}

func (l *lowerer) safeStmt(s *syntax.Stmt) (n *tree.Node) {
	defer func() {
		if r := recover(); r != nil {
			n = nil
			l.depth = 0
			at := l.f.PosAt(l.cursor)
			msg := "internal error lowering statement"
			if _, ok := r.(tooDeep); ok {
				msg = fmt.Sprintf("statement nested deeper than %d levels: not analysed", maxDepth)
			}
			l.errs = append(l.errs, tree.SyntaxError{Range: tree.Range{Start: at, End: at}, Msg: msg})
		}
	}()
	return l.stmt(s)
}

// lowerComments lowers every comment reachable from the parsed file, in source order.
func lowerComments(f *tree.File, base tree.Pos, af *syntax.File) (out []*tree.Node) {
	defer func() { _ = recover() }() // a partial tree after a hard error may hold nil nodes
	l := &lowerer{f: f, base: base}
	visitAll(af, func(n syntax.Node) {
		if c, ok := n.(*syntax.Comment); ok && c != nil && c.Hash.IsValid() {
			text := strings.TrimRight(c.Text, "\r")
			start := l.pos(c.Hash)
			out = append(out, &tree.Node{
				Kind:  tree.KComment,
				Range: tree.Range{Start: start, End: l.f.PosAt(start.Offset + 1 + len(text))},
				Value: strings.TrimSpace(text),
			})
		}
	})
	sort.SliceStable(out, func(i, j int) bool { return out[i].Range.Start.Offset < out[j].Range.Start.Offset })
	return out
}

func (l *lowerer) pos(p syntax.Pos) tree.Pos {
	return l.f.PosAt(l.base.Offset + int(p.Offset()))
}

// mk builds a node. A missing (invalid or recovered) start falls back to the
// first child, then to the cursor; a missing end to the last child's end.
func (l *lowerer) mk(k tree.Kind, start, end syntax.Pos, kids ...*tree.Node) *tree.Node {
	n := l.newNode()
	n.Kind = k
	live := 0
	for _, c := range kids {
		if c != nil {
			live++
		}
	}
	if live > 0 {
		n.Children = l.kidSlice(live)
		for _, c := range kids {
			if c != nil {
				c.Parent = n
				n.Children = append(n.Children, c)
			}
		}
	}
	switch {
	case start.IsValid():
		n.Range.Start = l.pos(start)
	case len(n.Children) > 0:
		n.Range.Start = n.Children[0].Range.Start
	default:
		n.Range.Start = l.f.PosAt(l.cursor)
	}
	if end.IsValid() {
		n.Range.End = l.pos(end)
	} else {
		n.Range.End = n.Range.Start
		for _, c := range n.Children {
			if c.Range.End.Offset > n.Range.End.Offset {
				n.Range.End = c.Range.End
			}
		}
	}
	if n.Range.End.Offset < n.Range.Start.Offset {
		n.Range.End = n.Range.Start
	}
	// A parent always covers its children (a heredoc body, for one, lies past
	// the end the parser reports for the enclosing pipeline stage).
	for _, c := range n.Children {
		if c.Range.Start.Offset < n.Range.Start.Offset {
			n.Range.Start = c.Range.Start
		}
		if c.Range.End.Offset > n.Range.End.Offset {
			n.Range.End = c.Range.End
		}
	}
	if n.Range.End.Offset > l.cursor {
		l.cursor = n.Range.End.Offset
	}
	return n
}

// missing marks n recovered and reports `missing <tok>` at n's end, when p is a
// position the parser invented.
func (l *lowerer) missing(n *tree.Node, p syntax.Pos, tok string) {
	if !p.IsRecovered() {
		return
	}
	n.Recovered = true
	l.errs = append(l.errs, tree.SyntaxError{Range: tree.Range{Start: n.Range.End, End: n.Range.End}, Msg: "missing " + tok})
}

func (l *lowerer) rng(a, b syntax.Pos) tree.Range {
	return tree.Range{Start: l.pos(a), End: l.pos(b)}
}

func (l *lowerer) stmts(list []*syntax.Stmt) []*tree.Node {
	out := make([]*tree.Node, 0, len(list))
	for _, s := range list {
		out = append(out, l.stmt(s))
	}
	return out
}

// group wraps a statement list in a synthetic KBlock spanning its statements.
func (l *lowerer) group(list []*syntax.Stmt) *tree.Node {
	return l.mk(tree.KBlock, syntax.Pos{}, syntax.Pos{}, l.stmts(list)...)
}

func (l *lowerer) stmt(s *syntax.Stmt) *tree.Node {
	l.enter()
	n := l.stmt1(s)
	l.depth--
	return n
}

func (l *lowerer) stmt1(s *syntax.Stmt) *tree.Node {
	var kids []*tree.Node
	if s.Cmd != nil {
		kids = append(kids, l.command(s.Cmd))
	}
	for _, r := range s.Redirs {
		kids = append(kids, l.redirect(r))
	}
	if s.Position.IsRecovered() {
		n := l.mk(tree.KStmt, syntax.Pos{}, syntax.Pos{}, kids...)
		l.missing(n, s.Position, "command")
		return n
	}
	n := l.mk(tree.KStmt, s.Position, stmtEnd(s), kids...)
	return n
}

// stmtEnd is s.End(), except for the commands whose End descends through
// every statement nested in them (time, coproc, function bodies): calling it
// at each level of a chain of those is quadratic, so the end is left invalid
// and mk takes it from the children, which cover the same text.
func stmtEnd(s *syntax.Stmt) syntax.Pos {
	switch s.Cmd.(type) {
	case *syntax.TimeClause, *syntax.CoprocClause, *syntax.FuncDecl:
		if !s.Semicolon.IsValid() {
			return syntax.Pos{}
		}
	}
	return s.End()
}

func (l *lowerer) command(c syntax.Command) *tree.Node {
	switch c := c.(type) {
	case *syntax.CallExpr:
		if n := l.declCall(c); n != nil {
			return n
		}
		var kids []*tree.Node
		for _, a := range c.Assigns {
			kids = append(kids, l.assign(a))
		}
		for _, w := range c.Args {
			kids = append(kids, l.word(w))
		}
		return l.mk(tree.KCall, syntax.Pos{}, syntax.Pos{}, kids...)
	case *syntax.DeclClause:
		var kids []*tree.Node
		for _, a := range c.Args {
			kids = append(kids, l.assign(a))
		}
		n := l.mk(tree.KDecl, c.Pos(), c.End(), kids...)
		n.Name, n.NameRange = c.Variant.Value, l.rng(c.Variant.ValuePos, c.Variant.ValueEnd)
		return n
	case *syntax.FuncDecl:
		n := l.mk(tree.KFuncDecl, c.Position, syntax.Pos{}, l.stmt(c.Body))
		if c.Name != nil {
			n.Name, n.NameRange = c.Name.Value, l.rng(c.Name.ValuePos, c.Name.ValueEnd)
		}
		return n
	case *syntax.TestDecl: // bats: @test "description" { ... }
		n := l.mk(tree.KFuncDecl, c.Position, syntax.Pos{}, l.stmt(c.Body))
		n.Name = l.literalText(c.Description)
		if len(c.Description.Parts) > 0 {
			n.NameRange = l.rng(c.Description.Pos(), c.Description.End())
		}
		return n
	case *syntax.IfClause:
		return l.ifClause(c, true)
	case *syntax.WhileClause:
		n := l.mk(tree.KWhile, c.WhilePos, c.End(), l.group(c.Cond), l.group(c.Do))
		l.missing(n, c.DoPos, "do")
		l.missing(n, c.DonePos, "done")
		return n
	case *syntax.ForClause:
		return l.forClause(c)
	case *syntax.CaseClause:
		kids := []*tree.Node{l.word(c.Word)}
		for _, it := range c.Items {
			var ik []*tree.Node
			for _, p := range it.Patterns {
				ik = append(ik, l.word(p))
			}
			ik = append(ik, l.stmts(it.Stmts)...)
			// Pos() panics without patterns; a pattern-less item only follows a hard error.
			if len(it.Patterns) == 0 {
				continue
			}
			kids = append(kids, l.mk(tree.KBlock, it.Pos(), it.End(), ik...))
		}
		n := l.mk(tree.KCase, c.Case, c.End(), kids...)
		l.missing(n, c.In, "in")
		l.missing(n, c.Esac, "esac")
		return n
	case *syntax.Block:
		n := l.mk(tree.KBlock, c.Lbrace, c.End(), l.stmts(c.Stmts)...)
		l.missing(n, c.Rbrace, "}")
		return n
	case *syntax.Subshell:
		n := l.mk(tree.KSubshell, c.Lparen, c.End(), l.stmts(c.Stmts)...)
		l.missing(n, c.Rparen, ")")
		return n
	case *syntax.BinaryCmd:
		switch c.Op {
		case syntax.Pipe, syntax.PipeAll:
			var kids []*tree.Node
			for _, s := range chain(c, isPipe) {
				kids = append(kids, l.stmt(s))
			}
			return l.mk(tree.KPipeline, syntax.Pos{}, syntax.Pos{}, kids...)
		default: // && and ||: one node over the whole chain, statements in source order
			var kids []*tree.Node
			for _, s := range chain(c, isAndOr) {
				kids = append(kids, l.stmt(s))
			}
			return l.mk(tree.KAndOr, syntax.Pos{}, syntax.Pos{}, kids...)
		}
	case *syntax.ArithmCmd:
		n := l.mk(tree.KArith, c.Left, c.End(), l.arith(c.X)...)
		l.missing(n, c.Right, "))")
		return n
	case *syntax.TestClause:
		n := l.mk(tree.KTest, c.Left, c.End(), l.test(c.X)...)
		l.missing(n, c.Right, "]]")
		return n
	case *syntax.LetClause:
		var kids []*tree.Node
		for _, e := range c.Exprs {
			kids = append(kids, l.arith(e)...)
		}
		return l.mk(tree.KLet, c.Let, syntax.Pos{}, kids...)
	case *syntax.TimeClause:
		if c.Stmt == nil {
			return l.mk(tree.KTime, c.Time, c.End())
		}
		return l.mk(tree.KTime, c.Time, syntax.Pos{}, l.stmt(c.Stmt)) // c.End() would recurse; see stmtEnd
	case *syntax.CoprocClause:
		var kids []*tree.Node
		if c.Name != nil {
			kids = append(kids, l.word(c.Name))
		}
		kids = append(kids, l.stmt(c.Stmt))
		return l.mk(tree.KCoproc, c.Coproc, syntax.Pos{}, kids...)
	}
	// Unknown command type: keep a placeholder so statement structure survives.
	return l.mk(tree.KCall, c.Pos(), c.End())
}

func isPipe(op syntax.BinCmdOperator) bool { return op == syntax.Pipe || op == syntax.PipeAll }

func isAndOr(op syntax.BinCmdOperator) bool { return op == syntax.AndStmt || op == syntax.OrStmt }

// chain flattens an unparenthesised `a | b | c` (or `a && b || c`) into its
// statements in source order. It keeps its own stack: the parser nests a long
// chain one BinaryCmd per operator, and recursing over it would overflow.
// Linear in the chain length.
func chain(c *syntax.BinaryCmd, same func(syntax.BinCmdOperator) bool) []*syntax.Stmt {
	var out []*syntax.Stmt
	stack := []*syntax.Stmt{c.Y, c.X}
	for len(stack) > 0 {
		s := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if b, ok := s.Cmd.(*syntax.BinaryCmd); ok && same(b.Op) && len(s.Redirs) == 0 && !s.Negated && !s.Background {
			stack = append(stack, b.Y, b.X)
		} else {
			out = append(out, s)
		}
	}
	return out
}

func (l *lowerer) ifClause(c *syntax.IfClause, top bool) *tree.Node {
	l.enter()
	n := l.ifClause1(c, top)
	l.depth--
	return n
}

func (l *lowerer) ifClause1(c *syntax.IfClause, top bool) *tree.Node {
	kids := []*tree.Node{l.group(c.Cond), l.group(c.Then)}
	if e := c.Else; e != nil {
		if e.ThenPos.IsValid() || e.ThenPos.IsRecovered() { // elif
			kids = append(kids, l.ifClause(e, false))
		} else { // plain else
			kids = append(kids, l.group(e.Then))
		}
	}
	n := l.mk(tree.KIf, c.Position, c.End(), kids...)
	l.missing(n, c.ThenPos, "then")
	if top { // elif/else clauses share the top clause's FiPos
		l.missing(n, c.FiPos, "fi")
	}
	return n
}

func (l *lowerer) forClause(c *syntax.ForClause) *tree.Node {
	open, closeTok := "do", "done"
	if c.Braces {
		open, closeTok = "{", "}"
	}
	body := l.mk(tree.KBlock, c.DoPos, c.End(), l.stmts(c.Do)...)
	var n *tree.Node
	switch loop := c.Loop.(type) {
	case *syntax.WordIter:
		var kids []*tree.Node
		for _, w := range loop.Items {
			kids = append(kids, l.word(w))
		}
		kids = append(kids, body)
		n = l.mk(tree.KFor, c.ForPos, c.End(), kids...)
		n.Name, n.NameRange = loop.Name.Value, l.rng(loop.Name.ValuePos, loop.Name.ValueEnd)
	case *syntax.CStyleLoop:
		var kids []*tree.Node
		kids = append(kids, l.arith(loop.Init)...)
		kids = append(kids, l.arith(loop.Cond)...)
		kids = append(kids, l.arith(loop.Post)...)
		kids = append(kids, body)
		n = l.mk(tree.KCStyleFor, c.ForPos, c.End(), kids...)
	default:
		n = l.mk(tree.KFor, c.ForPos, c.End(), body)
	}
	l.missing(n, c.DoPos, open)
	l.missing(n, c.DonePos, closeTok)
	return n
}

func (l *lowerer) assign(a *syntax.Assign) *tree.Node {
	var kids []*tree.Node
	kids = append(kids, l.arith(a.Index)...)
	if a.Value != nil {
		kids = append(kids, l.word(a.Value))
	}
	var rparen syntax.Pos
	if a.Array != nil {
		for _, el := range a.Array.Elems {
			kids = append(kids, l.arith(el.Index)...)
			if el.Value != nil {
				kids = append(kids, l.word(el.Value))
			}
		}
		rparen = a.Array.Rparen
	}
	n := l.mk(tree.KAssign, a.Pos(), a.End(), kids...)
	if a.Name != nil {
		n.Name, n.NameRange = a.Name.Value, l.rng(a.Name.ValuePos, a.Name.ValueEnd)
	}
	l.missing(n, rparen, ")")
	return n
}

func (l *lowerer) redirect(r *syntax.Redirect) *tree.Node {
	var kids []*tree.Node
	if r.Word != nil {
		kids = append(kids, l.word(r.Word))
	}
	n := l.mk(tree.KRedirect, r.OpPos, syntax.Pos{}, kids...)
	if r.N != nil {
		// The fd number precedes the operator; never shrink past a child that a
		// recovered position pulled further left.
		if p := l.pos(r.N.ValuePos); p.Offset < n.Range.Start.Offset {
			n.Range.Start = p
		}
	}
	if r.Hdoc != nil {
		var body []*tree.Node
		if len(r.Hdoc.Parts) > 0 {
			body = append(body, l.word(r.Hdoc))
		}
		var hd *tree.Node
		if len(r.Hdoc.Parts) > 0 {
			hd = l.mk(tree.KHeredoc, r.Hdoc.Pos(), r.Hdoc.End(), body...)
		} else {
			hd = l.mk(tree.KHeredoc, syntax.Pos{}, syntax.Pos{})
			hd.Range = tree.Range{Start: n.Range.End, End: n.Range.End}
		}
		if r.Word != nil {
			hd.Name = l.literalText(r.Word)
			if len(r.Word.Parts) > 0 {
				hd.NameRange = l.rng(r.Word.Pos(), r.Word.End())
			}
		}
		hd.Parent = n
		n.Children = append(n.Children, hd)
		if hd.Range.End.Offset > n.Range.End.Offset {
			n.Range.End = hd.Range.End // the body follows the operator line; the redirect owns it
		}
	}
	return n
}

// literalText concatenates a word's literal and quoted text; any non-literal
// part is ignored. Used for heredoc tags and bats test names.
func (l *lowerer) literalText(w *syntax.Word) string {
	if w == nil {
		return ""
	}
	var sb strings.Builder
	var walk func(parts []syntax.WordPart)
	walk = func(parts []syntax.WordPart) {
		for _, p := range parts {
			switch p := p.(type) {
			case *syntax.Lit:
				sb.WriteString(p.Value)
			case *syntax.SglQuoted:
				sb.WriteString(p.Value)
			case *syntax.DblQuoted:
				walk(p.Parts)
			}
		}
	}
	walk(w.Parts)
	return sb.String()
}

func (l *lowerer) word(w *syntax.Word) *tree.Node {
	l.enter()
	n := l.word1(w)
	l.depth--
	return n
}

func (l *lowerer) word1(w *syntax.Word) *tree.Node {
	var kids []*tree.Node
	for _, p := range w.Parts {
		kids = append(kids, l.part(p))
	}
	if len(kids) == 0 {
		return l.mk(tree.KWord, syntax.Pos{}, syntax.Pos{})
	}
	n := l.mk(tree.KWord, syntax.Pos{}, syntax.Pos{}, kids...)
	// The parser's own range for the word wins when it is valid (a Lit's
	// ValueEnd survives escaped newlines).
	if w.Pos().IsValid() {
		n.Range.Start = l.pos(w.Pos())
	}
	if w.End().IsValid() {
		n.Range.End = l.pos(w.End())
	}
	for _, c := range kids {
		if c.Recovered {
			n.Recovered = true
		}
		// A recovered part can lie outside the parser's word range; the word
		// still covers every child.
		if c.Range.Start.Offset < n.Range.Start.Offset {
			n.Range.Start = c.Range.Start
		}
		if c.Range.End.Offset > n.Range.End.Offset {
			n.Range.End = c.Range.End
		}
	}
	return n
}

func (l *lowerer) part(p syntax.WordPart) *tree.Node {
	l.enter()
	n := l.part1(p)
	l.depth--
	return n
}

func (l *lowerer) part1(p syntax.WordPart) *tree.Node {
	switch p := p.(type) {
	case *syntax.Lit:
		if p.ValuePos.IsRecovered() { // the parser invented a missing word
			n := l.mk(tree.KLit, syntax.Pos{}, syntax.Pos{})
			l.missing(n, p.ValuePos, "word")
			return n
		}
		n := l.mk(tree.KLit, p.ValuePos, p.ValueEnd)
		n.Value = p.Value
		return n
	case *syntax.SglQuoted:
		n := l.mk(tree.KSglQuoted, p.Left, p.End())
		n.Value = p.Value
		l.missing(n, p.Right, "'")
		return n
	case *syntax.DblQuoted:
		var kids []*tree.Node
		for _, q := range p.Parts {
			kids = append(kids, l.part(q))
		}
		n := l.mk(tree.KDblQuoted, p.Left, p.End(), kids...)
		l.missing(n, p.Right, `"`)
		return n
	case *syntax.ParamExp:
		var kids []*tree.Node
		kids = append(kids, l.arith(p.Index)...)
		if p.Slice != nil {
			kids = append(kids, l.arith(p.Slice.Offset)...)
			kids = append(kids, l.arith(p.Slice.Length)...)
		}
		if p.Repl != nil {
			if p.Repl.Orig != nil {
				kids = append(kids, l.word(p.Repl.Orig))
			}
			if p.Repl.With != nil {
				kids = append(kids, l.word(p.Repl.With))
			}
		}
		if p.Exp != nil && p.Exp.Word != nil {
			kids = append(kids, l.word(p.Exp.Word))
		}
		n := l.mk(tree.KParamExp, p.Pos(), p.End(), kids...)
		if p.Param != nil {
			n.Name, n.NameRange = p.Param.Value, l.rng(p.Param.ValuePos, p.Param.ValueEnd)
		}
		if !p.Short {
			l.missing(n, p.Rbrace, "}")
		}
		return n
	case *syntax.CmdSubst:
		n := l.mk(tree.KCmdSubst, p.Left, p.End(), l.stmts(p.Stmts)...)
		l.missing(n, p.Right, ")")
		return n
	case *syntax.ArithmExp:
		n := l.mk(tree.KArithExp, p.Left, p.End(), l.arith(p.X)...)
		l.missing(n, p.Right, "))")
		return n
	case *syntax.ProcSubst:
		n := l.mk(tree.KProcSubst, p.OpPos, p.End(), l.stmts(p.Stmts)...)
		l.missing(n, p.Rparen, ")")
		return n
	case *syntax.ExtGlob:
		n := l.mk(tree.KExtGlob, p.OpPos, p.End())
		if p.Pattern != nil {
			n.Value = p.Pattern.Value
		}
		return n
	case *syntax.BraceExp:
		var kids []*tree.Node
		for _, w := range p.Elems {
			kids = append(kids, l.word(w))
		}
		return l.mk(tree.KBraceExp, p.Pos(), p.End(), kids...)
	}
	return l.mk(tree.KLit, p.Pos(), p.End())
}

// arith flattens an arithmetic expression into the words it contains, in
// source order. Iterative: operator chains nest one node per operator.
func (l *lowerer) arith(e syntax.ArithmExpr) []*tree.Node {
	var out []*tree.Node
	stack := []syntax.ArithmExpr{e}
	for len(stack) > 0 {
		e := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		switch e := e.(type) {
		case *syntax.Word:
			out = append(out, l.word(e))
		case *syntax.BinaryArithm:
			stack = append(stack, e.Y, e.X)
		case *syntax.UnaryArithm:
			stack = append(stack, e.X)
		case *syntax.ParenArithm:
			stack = append(stack, e.X)
		}
	}
	return out
}

// test flattens a [[ ]] expression into the words it contains, in source
// order. Iterative, like arith.
func (l *lowerer) test(e syntax.TestExpr) []*tree.Node {
	var out []*tree.Node
	stack := []syntax.TestExpr{e}
	for len(stack) > 0 {
		e := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		switch e := e.(type) {
		case *syntax.Word:
			out = append(out, l.word(e))
		case *syntax.BinaryTest:
			stack = append(stack, e.Y, e.X)
		case *syntax.UnaryTest:
			stack = append(stack, e.X)
		case *syntax.ParenTest:
			stack = append(stack, e.X)
		}
	}
	return out
}

var declKeywords = map[string]bool{"local": true, "declare": true, "typeset": true, "export": true, "readonly": true}

var identPrefix = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*`)

// plainLit returns the text of a word that is a single unquoted literal.
func plainLit(w *syntax.Word) (*syntax.Lit, bool) {
	if len(w.Parts) != 1 {
		return nil, false
	}
	lit, ok := w.Parts[0].(*syntax.Lit)
	return lit, ok && lit.ValuePos.IsValid()
}

// declCall lowers `local|declare|typeset|export|readonly ARGS` that the parser
// gave as a plain call (every dialect but Bash) into the same KDecl shape the
// Bash DeclClause produces. Calls with env-assignment prefixes stay calls.
func (l *lowerer) declCall(c *syntax.CallExpr) *tree.Node {
	if len(c.Assigns) > 0 || len(c.Args) == 0 {
		return nil
	}
	kw, ok := plainLit(c.Args[0])
	if !ok || !declKeywords[kw.Value] {
		return nil
	}
	var kids []*tree.Node
	for _, w := range c.Args[1:] {
		kids = append(kids, l.declArg(w))
	}
	n := l.mk(tree.KDecl, c.Args[0].Pos(), c.End(), kids...)
	n.Name, n.NameRange = kw.Value, l.rng(kw.ValuePos, kw.ValueEnd)
	return n
}

// declArg lowers one declaration argument: `name`, `name=value`, `name+=value`
// become a KAssign named name; anything else a nameless KAssign wrapping the word.
func (l *lowerer) declArg(w *syntax.Word) *tree.Node {
	if lit, ok := w.Parts[0].(*syntax.Lit); ok && lit.ValuePos.IsValid() {
		if name := identPrefix.FindString(lit.Value); name != "" {
			rest := lit.Value[len(name):]
			start := l.pos(lit.ValuePos)
			nameRange := tree.Range{Start: start, End: l.f.PosAt(start.Offset + len(name))}
			bare := len(w.Parts) == 1 && rest == ""
			valued := strings.HasPrefix(rest, "=") || strings.HasPrefix(rest, "+=")
			if bare || valued {
				var kids []*tree.Node
				if valued {
					eq := len(name) + strings.IndexByte(rest, '=') + 1 // offset just past '='
					var parts []*tree.Node
					if eq < len(lit.Value) {
						ls := l.f.PosAt(start.Offset + eq)
						ln := &tree.Node{Kind: tree.KLit, Value: lit.Value[eq:], Range: tree.Range{Start: ls, End: l.pos(lit.ValueEnd)}}
						parts = append(parts, ln)
					}
					for _, p := range w.Parts[1:] {
						parts = append(parts, l.part(p))
					}
					if len(parts) > 0 {
						vw := &tree.Node{Kind: tree.KWord, Children: parts,
							Range: tree.Range{Start: parts[0].Range.Start, End: l.pos(w.End())}}
						for _, p := range parts {
							p.Parent = vw
						}
						kids = append(kids, vw)
					}
				}
				n := l.mk(tree.KAssign, w.Pos(), w.End(), kids...)
				n.Name, n.NameRange = name, nameRange
				return n
			}
		}
	}
	return l.mk(tree.KAssign, syntax.Pos{}, syntax.Pos{}, l.word(w))
}

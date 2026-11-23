package analysis

import (
	"bytes"
	"slices"

	"github.com/wamra/bash-lsp/tree"
)

// Names inside arithmetic. The lowering flattens an arithmetic expression into
// the words it contains and drops the operators, so the binder reads the
// expression's source text instead: the operator after a name says whether it
// is a write. Nested `$x`, `${x}`, `$(cmd)`, `$((..))` and `<(..)` keep going
// through the existing expansion path; their byte ranges are masked here so
// nothing is counted twice. Every scan is one linear pass over the bytes.

type span struct{ s, e int }

func isExpansion(k tree.Kind) bool {
	return k == tree.KParamExp || k == tree.KCmdSubst || k == tree.KArithExp || k == tree.KProcSubst
}

// collectMasks returns the ranges of the outermost expansion nodes under
// nodes, in source order. Iterative; it does not descend into an expansion.
func collectMasks(nodes []*tree.Node) []span {
	var out []span
	stack := make([]*tree.Node, 0, 8)
	for i := len(nodes) - 1; i >= 0; i-- {
		stack = append(stack, nodes[i])
	}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if n == nil {
			continue
		}
		if isExpansion(n.Kind) {
			out = append(out, span{n.Range.Start.Offset, n.Range.End.Offset})
			continue
		}
		for i := len(n.Children) - 1; i >= 0; i-- {
			stack = append(stack, n.Children[i])
		}
	}
	if !slices.IsSortedFunc(out, func(a, b span) int { return a.s - b.s }) {
		slices.SortStableFunc(out, func(a, b span) int { return a.s - b.s })
	}
	return out
}

func isDigit(c byte) bool      { return c >= '0' && c <= '9' }
func isIdentStart(c byte) bool { return c == '_' || (c|0x20 >= 'a' && c|0x20 <= 'z') }
func isIdentChar(c byte) bool  { return isIdentStart(c) || isDigit(c) }

// matchBracket returns the index of the `]` that closes the `[` at open, or -1
// when it does not close before limit.
func (b *binder) matchBracket(open, limit int) int {
	depth := 0
	for i := open; i < limit && i < len(b.src); i++ {
		switch b.src[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// assignOpAt reports whether an assignment operator starts at i (after blanks):
// plain is `=` alone, compound is one of the op= forms.
func (b *binder) assignOpAt(i, limit int) (plain, compound bool) {
	for i < limit && (b.src[i] == ' ' || b.src[i] == '\t' || b.src[i] == '\n') {
		i++
	}
	if i >= limit {
		return false, false
	}
	at := func(k int) byte {
		if i+k < limit {
			return b.src[i+k]
		}
		return 0
	}
	switch b.src[i] {
	case '=':
		return at(1) != '=', false
	case '+', '-', '*', '/', '%', '&', '^', '|':
		return false, at(1) == '='
	case '<', '>':
		return false, at(1) == b.src[i] && at(2) == '='
	}
	return false, false
}

// incDecAround reports a `++` or `--` written directly before start or directly after end.
func (b *binder) incDecAround(start, end, lo, limit int) bool {
	if end+1 < limit && (b.src[end] == '+' || b.src[end] == '-') && b.src[end+1] == b.src[end] {
		return true
	}
	if start-2 >= lo && (b.src[start-1] == '+' || b.src[start-1] == '-') && b.src[start-2] == b.src[start-1] {
		if start-3 < lo {
			return true
		}
		p := b.src[start-3]
		return !(isIdentChar(p) || p == ']' || p == ')')
	}
	return false
}

// arithAccess classifies a name occupying [start, end) in arithmetic text bounded by [lo, limit).
func (b *binder) arithAccess(start, end, lo, limit int) (read, write bool) {
	plain, compound := b.assignOpAt(end, limit)
	switch {
	case plain:
		return false, true
	case compound || b.incDecAround(start, end, lo, limit):
		return true, true
	}
	return true, false
}

// isDeclared reports whether a declaration of name is already in view at sc,
// by the rules the snapshot binds with: a private scope on the way up (passing
// function scopes), a local of the enclosing function, then any plain one.
func (b *binder) isDeclared(name string, sc *Scope) bool {
	b.startTracking()
	for s := sc; s != nil && s.Kind != ScopeFile; s = s.Parent {
		if s.Kind.Private() && b.declPriv[privDecl{s, name}] > 0 {
			return true
		}
	}
	if fn := funcBody(sc); fn != nil && b.declLocal[localDecl{fn, name}] > 0 {
		return true
	}
	return b.declGlobal[name] > 0
}

type privDecl struct {
	sc   *Scope
	name string
}

type localDecl struct {
	fn   *tree.Node
	name string
}

// startTracking builds the declaration index on the first arithmetic write, from
// the declarations made so far, so files without arithmetic writes never pay for it.
func (b *binder) startTracking() {
	if b.tracking {
		return
	}
	b.tracking = true
	for _, d := range b.ff.Decls {
		if !d.Temp {
			b.noteDecl(d, 1)
		}
	}
}

// noteDecl records (delta 1) or forgets (delta -1) a variable declaration for isDeclared.
func (b *binder) noteDecl(d *Decl, delta int) {
	if !b.tracking || d.Kind != DeclVariable {
		return
	}
	switch fn := funcBody(d.Scope); {
	case privateScope(d.Scope) != nil:
		b.declPriv[privDecl{privateScope(d.Scope), d.Name}] += delta
	case fn != nil && d.Attrs.Local:
		b.declLocal[localDecl{fn, d.Name}] += delta
	default:
		b.declGlobal[d.Name] += delta
	}
}

// arithWrite records a write to a name in arithmetic: a bare write when the
// name is already declared in view, else a declaration too (bash makes the
// variable, global inside a function, like a plain assignment).
func (b *binder) arithWrite(name string, r tree.Range, n *tree.Node, sc *Scope) {
	if b.isDeclared(name, sc) {
		b.use(UseVarWrite, name, r, sc, n)
		return
	}
	b.variable(name, r, n, sc, VarAttrs{}, "", "arith")
}

func (b *binder) arithName(name string, start, end, lo, limit int, n *tree.Node, sc *Scope) {
	r := tree.Range{Start: b.ff.File.PosAt(start), End: b.ff.File.PosAt(end)}
	read, write := b.arithAccess(start, end, lo, limit)
	if read {
		b.use(UseVarRead, name, r, sc, n)
	}
	if write {
		b.arithWrite(name, r, n, sc)
	}
}

// intern returns the string for b without allocating when it was seen before
// (the map lookup on string(b) does not copy).
func (b *binder) intern(raw []byte) string {
	if s, ok := b.names[string(raw)]; ok {
		return s
	}
	s := string(raw)
	b.names[s] = s
	return s
}

// arithText records the names in the arithmetic source b.src[s:e]. masks are
// the expansion ranges to skip, in source order.
func (b *binder) arithText(s, e int, masks []span, sc *Scope, n *tree.Node) {
	s, e = max(s, 0), min(e, len(b.src))
	src := b.src
	mi := 0
	for i := s; i < e; {
		for mi < len(masks) && masks[mi].e <= i {
			mi++
		}
		if mi < len(masks) && masks[mi].s <= i {
			i = masks[mi].e
			continue
		}
		c := src[i]
		switch {
		case isDigit(c): // a number: 0x1F, 16#ff, 1e3
			j, hash := i+1, false
			for j < e && (isIdentChar(src[j]) || src[j] == '#' || (hash && src[j] == '@')) {
				hash = hash || src[j] == '#'
				j++
			}
			i = j
		case isIdentStart(c) || c >= 0x80:
			j, ascii := i+1, c < 0x80
			for j < e && (isIdentChar(src[j]) || src[j] >= 0x80) {
				ascii = ascii && src[j] < 0x80
				j++
			}
			if mi < len(masks) && masks[mi].s > i && masks[mi].s < j {
				j = masks[mi].s
			}
			next := j
			if ascii && (i == s || src[i-1] != '$') {
				name, exprEnd := b.intern(src[i:j]), j
				if j < e && src[j] == '[' {
					if cl := b.matchBracket(j, e); cl > 0 {
						exprEnd = cl + 1
						if b.isAssoc(name) { // the subscript is a string
							next = exprEnd
						}
					}
				}
				r := tree.Range{Start: b.ff.File.PosAt(i), End: b.ff.File.PosAt(j)}
				read, write := b.arithAccess(i, exprEnd, s, e)
				if read {
					b.use(UseVarRead, name, r, sc, n)
				}
				if write {
					b.arithWrite(name, r, n, sc)
				}
			}
			i = next
		default:
			i++
		}
	}
}

// arithBody records the names of an arithmetic command or expansion whose
// source starts with open (`((` or `$((`) and ends with `))`.
func (b *binder) arithBody(n *tree.Node, sc *Scope, open string) {
	s, e := n.Range.Start.Offset, n.Range.End.Offset
	if s < 0 || e > len(b.src) || !bytes.HasPrefix(b.src[s:e], []byte(open)) {
		return
	}
	ts := s + len(open)
	if !n.Recovered && e-2 >= ts && b.src[e-1] == ')' && b.src[e-2] == ')' {
		e -= 2
	}
	b.arithText(ts, e, collectMasks(n.Children), sc, n)
}

// letBody records the names in `let` arguments. The lowered node ends at the
// last word, so a trailing `++` or `--` is added back.
func (b *binder) letBody(n *tree.Node, sc *Scope) {
	s, e := n.Range.Start.Offset, n.Range.End.Offset
	if s < 0 || e > len(b.src) || !bytes.HasPrefix(b.src[s:], []byte("let")) {
		return
	}
	for e+1 < len(b.src) && (b.src[e] == '+' || b.src[e] == '-') && b.src[e+1] == b.src[e] {
		e += 2
	}
	b.arithText(s+3, e, collectMasks(n.Children), sc, n)
}

// cStyleBody records the names in the three clauses of `for ((;;))`.
func (b *binder) cStyleBody(n *tree.Node, sc *Scope) {
	i, end := n.Range.Start.Offset+3, min(n.Range.End.Offset, len(b.src))
	for i < end && (b.src[i] == ' ' || b.src[i] == '\t' || b.src[i] == '\n' || b.src[i] == '\\') {
		i++
	}
	if i < 0 || i+1 >= end || b.src[i] != '(' || b.src[i+1] != '(' {
		return
	}
	ts, depth, closeAt := i+2, 0, -1
	for k := ts; k < end && closeAt < 0; k++ {
		switch b.src[k] {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				closeAt = k
			} else {
				depth--
			}
		}
	}
	if closeAt < 0 {
		closeAt = end
	}
	var clauses []*tree.Node
	for _, c := range n.Children {
		if c.Range.Start.Offset < closeAt {
			clauses = append(clauses, c)
		}
	}
	b.arithText(ts, closeAt, collectMasks(clauses), sc, n)
}

// paramArith handles what a parameter expansion holds besides its name: the
// array subscript (an arithmetic expression unless the array is associative)
// and the offset and length of `${v:off:len}`.
func (b *binder) paramArith(n *tree.Node, sc *Scope) {
	i, end := n.NameRange.End.Offset, min(n.Range.End.Offset, len(b.src))
	if n.Name == "" || i <= 0 || i >= end {
		return
	}
	var masks []span
	masked := false
	scan := func(s, e int) {
		if !masked {
			masks, masked = collectMasks(n.Children), true
		}
		b.arithText(s, e, masks, sc, n)
	}
	p := i
	if b.src[p] == '[' {
		if cl := b.matchBracket(p, end); cl > 0 {
			if !b.isAssoc(n.Name) {
				scan(p+1, cl)
			}
			p = cl + 1
		}
	}
	if p+1 < end && b.src[p] == ':' && !bytes.ContainsRune([]byte("-=+?"), rune(b.src[p+1])) {
		e := end
		if b.src[e-1] == '}' {
			e--
		}
		scan(p+1, e)
	}
}

// assignSubscript handles `name[expr]=value`.
func (b *binder) assignSubscript(n *tree.Node, assoc bool, sc *Scope) {
	i, end := n.NameRange.End.Offset, min(n.Range.End.Offset, len(b.src))
	if n.Name == "" || i <= 0 || i >= end || b.src[i] != '[' || assoc || b.isAssoc(n.Name) {
		return
	}
	if cl := b.matchBracket(i, end); cl > 0 {
		b.arithText(i+1, cl, collectMasks(n.Children), sc, n)
	}
}

// isAssoc reports whether name is declared with the associative attribute
// anywhere in the file. The file is scanned on the first call, and only when
// its text could hold an option cluster with an `A`.
func (b *binder) isAssoc(name string) bool {
	if !b.assocScanned {
		b.assocScanned = true
		if mayDeclareAssoc(b.src) && b.ff.File.Root != nil {
			b.scanAssoc(b.ff.File.Root)
		}
	}
	return b.assoc[name]
}

// mayDeclareAssoc reports whether src holds a `-` followed by letters with an `A` among them.
func mayDeclareAssoc(src []byte) bool {
	for i := 0; i < len(src); i++ {
		if src[i] != '-' {
			continue
		}
		for j := i + 1; j < len(src) && (src[j]|0x20 >= 'a' && src[j]|0x20 <= 'z'); j++ {
			if src[j] == 'A' {
				return true
			}
		}
	}
	return false
}

// scanAssoc records every name declared with the associative attribute
// anywhere in the file, so a subscript is read as a string even where it
// precedes the declaration in the source (a function above `declare -A`).
func (b *binder) scanAssoc(root *tree.Node) {
	stack := []*tree.Node{root}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if n.Kind == tree.KDecl {
			if attrs, _, skip := declFlags(n); attrs.Assoc && !skip {
				for _, a := range n.Children {
					if a.Kind == tree.KAssign && a.Name != "" {
						b.assoc[a.Name] = true
					}
				}
			}
		}
		stack = append(stack, n.Children...)
	}
}

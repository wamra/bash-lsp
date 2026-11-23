// Package analysis extracts facts from the owned syntax tree and answers
// queries over them. It never imports the parser library.
package analysis

import (
	"bytes"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/wamra/bash-lsp/tree"
)

// ScopeKind says what opened a scope.
type ScopeKind uint8

const (
	ScopeFile ScopeKind = iota + 1
	ScopeFunction
	ScopeSubshell
	ScopeCmdSubst
	ScopePipeline
	ScopeProcSubst // `<( )` and `>( )` body
	ScopeCoproc    // `coproc` body
)

// Private reports whether a variable declared directly in a scope of this kind
// is visible only inside that scope (and scopes nested in it): a subshell, a
// command substitution, a pipeline stage, a process substitution or a coproc.
func (k ScopeKind) Private() bool {
	return k == ScopeSubshell || k == ScopeCmdSubst || k == ScopePipeline || k == ScopeProcSubst || k == ScopeCoproc
}

// privateScope returns the nearest enclosing private scope of sc within its
// function (the walk stops at a function or file scope), nil when there is none.
func privateScope(sc *Scope) *Scope {
	for ; sc != nil; sc = sc.Parent {
		if sc.Kind.Private() {
			return sc
		}
		if sc.Kind == ScopeFunction || sc.Kind == ScopeFile {
			return nil
		}
	}
	return nil
}

// confining returns the innermost private scope (see ScopeKind.Private) of
// sc or its ancestors, function scopes included; nil when there is none.
func confining(sc *Scope) *Scope {
	for ; sc != nil; sc = sc.Parent {
		if sc.Kind.Private() {
			return sc
		}
	}
	return nil
}

// Scope is a lexical scope. Func is the name of the enclosing function, "" at file level.
type Scope struct {
	Kind   ScopeKind
	Parent *Scope
	Node   *tree.Node
	Func   string
}

// DeclKind says what a Decl declares.
type DeclKind uint8

const (
	DeclFunction DeclKind = iota + 1
	DeclVariable
	DeclAlias
)

// VarAttrs are the attributes a declaration gives a variable.
type VarAttrs struct{ Local, Exported, Readonly, Integer, Array, Assoc, Nameref, Global bool }

// Decl is one declaration. Read-only: snapshots built from the same FileFacts
// share its Decls.
type Decl struct {
	Kind      DeclKind
	Name      string
	NameRange tree.Range
	Scope     *Scope
	Attrs     VarAttrs
	Doc       string // contiguous '#' comment lines ending on the line directly above the declaration, joined with "\n"; not the shebang; at most the 50 lines and 4 KiB nearest the declaration
	Node      *tree.Node
	AliasTo   string // DeclAlias: first word of the alias value when literal, else ""
	Literal   string // DeclVariable: assigned value when it is a single literal or fully-literal quoted string, else ""
	Implicit  string // "" for assignments/declare; else the builtin that declares it: "read", "mapfile", "readarray", "getopts", "printf -v", "for", "select"
	Temp      bool   // true for an assignment prefixed to a command (`X=1 cmd`): it lives only for that command and must not be offered as a definition for later reads
	// Confined, for a DeclFunction: the innermost subshell, command
	// substitution, pipeline stage, process substitution or coproc body
	// around the definition (a function body is no boundary: a function
	// defined in another's body is global once that body runs); nil when there
	// is none. A call outside it cannot be sure to reach this definition.
	Confined *Scope
}

// UseKind says how a name is used.
type UseKind uint8

const (
	UseCall UseKind = iota + 1
	UseVarRead
	UseVarWrite
)

// Use is one use of a name. Read-only: snapshots built from the same FileFacts
// share its Uses. ViaAlias: the use is the target named inside an alias definition.
type Use struct {
	Kind     UseKind
	Name     string
	Range    tree.Range
	Scope    *Scope
	Node     *tree.Node
	ViaAlias bool
}

// SourceRef is a `source`/`.` (or bats `load`) command. Directive is the path
// from a `# shellcheck source=` comment on the line above, "" if none.
type SourceRef struct {
	Node      *tree.Node
	Word      *tree.Node
	Range     tree.Range
	Directive string
}

// FileFacts is everything Bind extracts from one file. Read-only: shared
// between snapshots (a snapshot built after an edit reuses the facts of the
// files that did not change), and with them every Decl and Use.
type FileFacts struct {
	Path    string
	File    *tree.File
	Root    *Scope
	Decls   []*Decl
	Uses    []*Use
	Sources []SourceRef
}

var (
	identRe     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	directiveRe = regexp.MustCompile(`^\s*shellcheck\s+(?:.*\s)?source=(\S+)`)
)

type binder struct {
	ff       *FileFacts
	src      []byte
	bats     bool
	wholeCmt map[int]*tree.Node // line -> comment that is alone on its line (shebang excluded)

	useSlab      []Use
	declSlab     []Decl
	ints         map[intKey]bool // integer-attribute declarations, by where they bind
	tracking     bool            // declGlobal, declLocal and declPriv are maintained (from the first arithmetic write)
	assocScanned bool
	names        map[string]string // interned arithmetic names
	assoc        map[string]bool   // names declared -A anywhere in the file: their subscripts are strings
	declGlobal   map[string]int    // declarations in view, by where they bind (see isDeclared)
	declLocal    map[localDecl]int
	declPriv     map[privDecl]int
}

// Bind walks f once, carrying a scope stack, and returns its facts.
func Bind(f *tree.File) *FileFacts {
	ff := &FileFacts{File: f}
	if f == nil {
		ff.Root = &Scope{Kind: ScopeFile}
		return ff
	}
	ff.Path = f.Path
	ff.Root = &Scope{Kind: ScopeFile, Node: f.Root}
	b := &binder{ff: ff, src: f.Src, wholeCmt: map[int]*tree.Node{}, assoc: map[string]bool{}, names: map[string]string{},
		declGlobal: map[string]int{}, declLocal: map[localDecl]int{}, declPriv: map[privDecl]int{}}
	b.bats = f.Dialect == tree.Bats
	for _, c := range f.Comments {
		start := c.Range.Start
		if start.Line == 1 && start.Offset+1 < len(f.Src) && f.Src[start.Offset+1] == '!' {
			continue
		}
		lineStart := start.Offset - (start.Col - 1)
		if lineStart >= 0 && strings.TrimSpace(string(f.Src[lineStart:start.Offset])) == "" {
			b.wholeCmt[start.Line] = c
		}
	}
	if f.Root != nil {
		b.children(f.Root, ff.Root)
	}
	return ff
}

func (b *binder) children(n *tree.Node, sc *Scope) {
	for _, c := range n.Children {
		b.visit(c, sc)
	}
}

func child(sc *Scope, k ScopeKind, n *tree.Node) *Scope {
	return &Scope{Kind: k, Parent: sc, Node: n, Func: sc.Func}
}

func (b *binder) visit(n *tree.Node, sc *Scope) {
	switch n.Kind {
	case tree.KFuncDecl:
		if n.Name != "" {
			b.ff.Decls = append(b.ff.Decls, &Decl{Kind: DeclFunction, Name: n.Name, NameRange: n.NameRange,
				Scope: b.ff.Root, Doc: b.docAbove(n.Range.Start.Line), Node: n, Confined: confining(sc)})
		}
		fs := child(sc, ScopeFunction, n)
		fs.Func = n.Name
		b.children(n, fs)
	case tree.KSubshell:
		b.children(n, child(sc, ScopeSubshell, n))
	case tree.KCmdSubst:
		b.children(n, child(sc, ScopeCmdSubst, n))
	case tree.KProcSubst:
		b.children(n, child(sc, ScopeProcSubst, n))
	case tree.KCoproc:
		cs := child(sc, ScopeCoproc, n)
		for _, c := range n.Children {
			if c.Kind == tree.KStmt {
				b.visit(c, cs)
			} else {
				b.visit(c, sc)
			}
		}
	case tree.KPipeline:
		for _, stage := range n.Children {
			b.children(stage, child(sc, ScopePipeline, stage))
		}
	case tree.KParamExp:
		b.paramExp(n, sc)
	case tree.KArith:
		b.arithBody(n, sc, "((")
		b.children(n, sc)
	case tree.KArithExp:
		b.arithBody(n, sc, "$((")
		b.children(n, sc)
	case tree.KLet:
		b.letBody(n, sc)
		b.children(n, sc)
	case tree.KCStyleFor:
		b.cStyleBody(n, sc)
		b.children(n, sc)
	case tree.KAssign:
		b.assign(n, sc, VarAttrs{}, "")
	case tree.KDecl:
		b.declClause(n, sc)
	case tree.KFor:
		if n.Name != "" {
			impl := "for"
			if o := n.Range.Start.Offset; o >= 0 && o <= len(b.src) && bytes.HasPrefix(b.src[o:], []byte("select")) {
				impl = "select"
			}
			b.variable(n.Name, n.NameRange, n, sc, VarAttrs{}, "", impl)
		}
		b.children(n, sc)
	case tree.KCall:
		b.call(n, sc)
	default:
		b.children(n, sc)
	}
}

// paramExp records the read of a parameter expansion and the arithmetic it
// holds. `name[expr]` written inside arithmetic (no `$`) is also a write when
// an assignment operator or `++`/`--` is attached to it.
func (b *binder) paramExp(n *tree.Node, sc *Scope) {
	if n.Name != "" && !isSpecialParam(n.Name) {
		read, write := true, false
		if s := n.Range.Start.Offset; s >= 0 && s < len(b.src) && b.src[s] != '$' {
			read, write = b.arithAccess(s, n.Range.End.Offset, 0, len(b.src))
		}
		if read {
			b.use(UseVarRead, n.Name, n.NameRange, sc, n)
		}
		if write {
			b.arithWrite(n.Name, n.NameRange, n, sc)
		}
	}
	b.paramArith(n, sc)
	b.children(n, sc)
}

func isSpecialParam(name string) bool {
	switch name {
	case "@", "*", "#", "?", "-", "$", "!":
		return true
	}
	for _, r := range name {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (b *binder) use(k UseKind, name string, r tree.Range, sc *Scope, n *tree.Node) *Use {
	if len(b.useSlab) == 0 { // Uses are allocated in chunks: one allocation per 256 instead of 256
		b.useSlab = make([]Use, 256)
	}
	u := &b.useSlab[0]
	b.useSlab = b.useSlab[1:]
	*u = Use{Kind: k, Name: name, Range: r, Scope: sc, Node: n}
	b.ff.Uses = append(b.ff.Uses, u)
	return u
}

// variable records a DeclVariable and its UseVarWrite.
func (b *binder) variable(name string, r tree.Range, n *tree.Node, sc *Scope, attrs VarAttrs, literal, implicit string) {
	if len(b.declSlab) == 0 {
		b.declSlab = make([]Decl, 128)
	}
	d := &b.declSlab[0]
	b.declSlab = b.declSlab[1:]
	*d = Decl{Kind: DeclVariable, Name: name, NameRange: r, Scope: sc, Attrs: attrs,
		Doc: b.docAbove(n.Range.Start.Line), Node: n, Literal: literal, Implicit: implicit}
	b.ff.Decls = append(b.ff.Decls, d)
	b.noteDecl(d, 1)
	if attrs.Integer {
		b.noteInteger(d)
	}
	b.use(UseVarWrite, name, r, sc, n)
}

// assign handles a named KAssign (plain, env prefix or declaration argument).
// A nameless one is an option word wrapper and only has its children visited.
func (b *binder) assign(n *tree.Node, sc *Scope, attrs VarAttrs, implicit string) {
	if n.Name != "" {
		if b.isArrayAssign(n) {
			attrs.Array = true
		}
		b.variable(n.Name, n.NameRange, n, sc, attrs, b.assignLiteral(n), implicit)
		b.assignSubscript(n, attrs.Assoc, sc)
		b.arrayLiteralSubscripts(n, attrs.Assoc, sc)
		if attrs.Integer || b.isInteger(n.Name, sc) {
			b.assignValue(n, sc)
		}
	}
	b.children(n, sc)
}

// isArrayAssign reports whether the source after the name reads `=(` or `+=(`.
func (b *binder) isArrayAssign(n *tree.Node) bool {
	i := n.NameRange.End.Offset
	if i <= 0 || i >= len(b.src) {
		return false
	}
	if b.src[i] == '+' {
		i++
	}
	return i+1 < len(b.src) && b.src[i] == '=' && b.src[i+1] == '('
}

func (b *binder) assignLiteral(n *tree.Node) string {
	if b.isArrayAssign(n) {
		return ""
	}
	if i := n.NameRange.End.Offset; i < len(b.src) && b.src[i] == '[' { // subscripted: children mix index and value
		return ""
	}
	for i := len(n.Children) - 1; i >= 0; i-- {
		if c := n.Children[i]; c.Kind == tree.KWord {
			s, _ := literalText(c)
			return s
		}
	}
	return ""
}

// literalText returns the text of a word made only of literals and
// fully-literal quoted strings. ok is false for any expansion.
func literalText(w *tree.Node) (string, bool) {
	var sb strings.Builder
	for _, p := range w.Children {
		switch p.Kind {
		case tree.KLit:
			if p.Recovered {
				return "", false
			}
			sb.WriteString(p.Value)
		case tree.KSglQuoted:
			sb.WriteString(p.Value)
		case tree.KDblQuoted:
			for _, q := range p.Children {
				if q.Kind != tree.KLit {
					return "", false
				}
				sb.WriteString(q.Value)
			}
		default:
			return "", false
		}
	}
	return sb.String(), true
}

// declFlags reads the option words of a declaration command.
func declFlags(n *tree.Node) (attrs VarAttrs, dashN, skip bool) {
	// dashN: -n is nameref for declare/typeset/local, "remove the export attribute" for export/readonly
	// skip: -f/-F/-p are the function or listing form, no variables declared
	for _, a := range n.Children {
		if a.Kind != tree.KAssign || a.Name != "" || len(a.Children) == 0 || a.Children[0].Kind != tree.KWord {
			continue
		}
		txt, ok := literalText(a.Children[0])
		if !ok || len(txt) < 2 || txt[0] != '-' {
			continue
		}
		for _, ch := range txt[1:] {
			switch ch {
			case 'x':
				attrs.Exported = true
			case 'r':
				attrs.Readonly = true
			case 'i':
				attrs.Integer = true
			case 'a':
				attrs.Array = true
			case 'A':
				attrs.Assoc = true
			case 'n':
				dashN = true
			case 'g':
				attrs.Global = true
			case 'f', 'F', 'p':
				skip = true
			}
		}
	}
	return
}

func (b *binder) declClause(n *tree.Node, sc *Scope) {
	attrs, dashN, skip := declFlags(n)
	switch n.Name {
	case "export":
		attrs.Exported = !dashN
	case "readonly":
		attrs.Readonly = true
	case "local":
		attrs.Local = true
		attrs.Nameref = dashN
	case "declare", "typeset":
		attrs.Local = sc.Func != "" && !attrs.Global
		attrs.Nameref = dashN
	}
	for _, a := range n.Children {
		if skip && a.Kind == tree.KAssign && a.Name != "" {
			b.children(a, sc)
			continue
		}
		if a.Kind == tree.KAssign {
			b.assign(a, sc, attrs, "")
		} else {
			b.visit(a, sc)
		}
	}
}

func (b *binder) call(n *tree.Node, sc *Scope) {
	var words []*tree.Node
	for _, c := range n.Children {
		if c.Kind == tree.KWord {
			words = append(words, c)
		}
	}
	if len(words) > 0 {
		if name, ok := literalText(words[0]); ok && name != "" {
			if b.builtin(n, name, words, sc) {
				b.callChildren(n, sc, len(words) > 0)
				return
			}
			b.use(UseCall, name, words[0].Range, sc, n)
		}
	}
	b.callChildren(n, sc, len(words) > 0)
}

// callChildren visits a call's children; env-prefix assignments are Temp when a command follows.
func (b *binder) callChildren(n *tree.Node, sc *Scope, hasCmd bool) {
	for _, c := range n.Children {
		if c.Kind != tree.KAssign || !hasCmd {
			b.visit(c, sc)
			continue
		}
		start := len(b.ff.Decls)
		b.assign(c, sc, VarAttrs{}, "")
		for _, d := range b.ff.Decls[start:] {
			if d.Node == c {
				d.Temp = true
				b.noteDecl(d, -1)
			}
		}
	}
}

// builtin handles calls with special meaning. It returns true when the call
// must not be recorded as a UseCall.
func (b *binder) builtin(n *tree.Node, name string, words []*tree.Node, sc *Scope) bool {
	args := words[1:]
	switch name {
	case "source", ".", "load":
		if name == "load" && !b.bats {
			return false
		}
		if len(args) == 0 {
			return true
		}
		b.ff.Sources = append(b.ff.Sources, SourceRef{Node: n, Word: args[0], Range: args[0].Range,
			Directive: b.directiveAbove(n.Range.Start.Line)})
		return true
	case "alias":
		for _, w := range args {
			b.aliasArg(n, w, sc)
		}
		return true
	case "read":
		b.readNames(n, args, sc, "read", "pdnNtui")
	case "mapfile", "readarray":
		b.firstName(n, args, sc, name, "dnOsCcu", true)
	case "getopts":
		if len(args) >= 2 {
			if v, ok := literalText(args[1]); ok && identRe.MatchString(v) {
				b.variable(v, args[1].Range, n, sc, VarAttrs{}, "", "getopts")
			}
		}
	case "printf":
		for i, w := range args {
			if v, ok := literalText(w); ok && v == "-v" && i+1 < len(args) {
				if nm, ok := literalText(args[i+1]); ok && identRe.MatchString(nm) {
					b.variable(nm, args[i+1].Range, n, sc, VarAttrs{}, "", "printf -v")
				}
				break
			}
		}
	}
	return false
}

// optionWalk walks option words per the argTaking letters and calls name for
// each remaining word until name returns false.
func optionWalk(args []*tree.Node, argTaking string, name func(*tree.Node) bool) {
	for i := 0; i < len(args); i++ {
		txt, ok := literalText(args[i])
		if ok && txt == "--" {
			for _, w := range args[i+1:] {
				if !name(w) {
					return
				}
			}
			return
		}
		if ok && len(txt) > 1 && txt[0] == '-' {
			for j := 1; j < len(txt); j++ {
				if strings.IndexByte(argTaking, txt[j]) >= 0 {
					if j == len(txt)-1 {
						i++ // the argument is the next word
					}
					break
				}
			}
			continue
		}
		if !name(args[i]) {
			return
		}
	}
}

func hasOption(args []*tree.Node, letter byte) bool {
	for _, w := range args {
		if txt, ok := literalText(w); ok && len(txt) > 1 && txt[0] == '-' && txt[1] != '-' && strings.IndexByte(txt, letter) >= 0 {
			return true
		}
	}
	return false
}

func (b *binder) readNames(n *tree.Node, args []*tree.Node, sc *Scope, impl, argTaking string) {
	attrs := VarAttrs{Array: hasOption(args, 'a')}
	optionWalk(args, argTaking, func(w *tree.Node) bool {
		if v, ok := literalText(w); ok && identRe.MatchString(v) {
			b.variable(v, w.Range, n, sc, attrs, "", impl)
		}
		return true
	})
}

func (b *binder) firstName(n *tree.Node, args []*tree.Node, sc *Scope, impl, argTaking string, array bool) {
	attrs := VarAttrs{Array: array}
	optionWalk(args, argTaking, func(w *tree.Node) bool {
		if v, ok := literalText(w); ok && identRe.MatchString(v) {
			b.variable(v, w.Range, n, sc, attrs, "", impl)
		}
		return false
	})
}

// aliasArg handles one `NAME=VALUE` argument of alias.
func (b *binder) aliasArg(call, w *tree.Node, sc *Scope) {
	s, e := w.Range.Start.Offset, w.Range.End.Offset
	if s < 0 || e > len(b.src) || s >= e {
		return
	}
	raw := string(b.src[s:e])
	eq := strings.IndexByte(raw, '=')
	if eq <= 0 || strings.ContainsAny(raw[:eq], "'\"$`\\ \t") {
		return
	}
	d := &Decl{Kind: DeclAlias, Name: raw[:eq], Scope: sc, Node: call, Doc: b.docAbove(call.Range.Start.Line)}
	d.NameRange = tree.Range{Start: w.Range.Start, End: b.ff.File.PosAt(s + eq)}
	if _, lit := literalText(w); lit {
		off := s + eq + 1
		val := raw[eq+1:]
		if val != "" && (val[0] == '\'' || val[0] == '"') {
			q := val[0]
			off++
			val = val[1:]
			if j := strings.IndexByte(val, q); j >= 0 {
				val = val[:j]
			}
		}
		trimmed := strings.TrimLeft(val, " \t")
		off += len(val) - len(trimmed)
		if j := strings.IndexAny(trimmed, " \t"); j >= 0 {
			trimmed = trimmed[:j]
		}
		if trimmed != "" {
			d.AliasTo = trimmed
			u := b.use(UseCall, trimmed, tree.Range{Start: b.ff.File.PosAt(off), End: b.ff.File.PosAt(off + len(trimmed))}, sc, call)
			u.ViaAlias = true
		}
	}
	b.ff.Decls = append(b.ff.Decls, d)
}

// A declaration's doc is cut to the comment lines nearest it: a block of
// thousands of lines above a function (a licence glued to it, or a generated
// file) would otherwise be copied into every hover and rescanned per edit.
const (
	maxDocLines = 50
	maxDocBytes = 4096
)

// docAbove joins the contiguous whole-line comments ending on the line above
// line: at most maxDocLines lines and maxDocBytes bytes, the nearest kept.
// It reads at most maxDocLines comments, so it is O(1) per declaration.
func (b *binder) docAbove(line int) string {
	var lines []string // nearest first
	size := 0
	for l := line - 1; l >= 1 && len(lines) < maxDocLines; l-- {
		c := b.wholeCmt[l]
		if c == nil {
			break
		}
		v := c.Value
		if size+len(v) > maxDocBytes {
			if len(lines) == 0 { // a single over-long line: keep its start
				lines = append(lines, truncUTF8(v, maxDocBytes))
			}
			break
		}
		size += len(v) + 1
		lines = append(lines, v)
	}
	slices.Reverse(lines)
	return strings.Join(lines, "\n")
}

// truncUTF8 cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// directiveAbove returns the path of a `# shellcheck source=` comment on the line above line.
func (b *binder) directiveAbove(line int) string {
	if c := b.wholeCmt[line-1]; c != nil {
		if m := directiveRe.FindStringSubmatch(c.Value); m != nil {
			return m[1]
		}
	}
	return ""
}

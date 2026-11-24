package analysis

import (
	"slices"
	"strings"

	"github.com/wamra/bash-lsp/tree"
)

// nodeAtter finds the graph node whose code holds an offset, with one Locator
// per file so that a run of lookups costs a descent each.
type nodeAtter struct {
	s    *Snapshot
	locs map[int]*tree.Locator
}

func (s *Snapshot) newNodeAtter() *nodeAtter { return &nodeAtter{s: s, locs: map[int]*tree.Locator{}} }

func (a *nodeAtter) at(file, offset int) Node {
	n := Node{Path: a.s.paths[file]}
	f := a.s.facts[file].File
	if f == nil || f.Root == nil || offset < 0 || offset >= len(f.Src) {
		return n
	}
	loc := a.locs[file]
	if loc == nil {
		loc = f.NewLocator()
		a.locs[file] = loc
	}
	if d := a.s.funcDeclAround(file, loc.At(offset)); d != nil {
		n.Func = a.s.declSym[d]
	}
	return n
}

// editNodes returns the graph nodes that hold the edits.
func (s *Snapshot) editNodes(at *nodeAtter, edits []Location) map[Node]bool {
	out := map[Node]bool{}
	for _, e := range edits {
		if f, ok := s.idx[e.Path]; ok {
			out[at.at(f, e.Range.Start.Offset)] = true
		}
	}
	return out
}

// conditionalKind reports the constructs that make a statement inside them run
// only sometimes, or in a copy of the shell.
func conditionalKind(k tree.Kind) bool {
	switch k {
	case tree.KAndOr, tree.KSubshell, tree.KPipeline, tree.KCmdSubst, tree.KProcSubst, tree.KCoproc, tree.KFuncDecl:
		return true
	}
	return false
}

// shadowSpan is the part of a function that a local declaration hides the
// caller's variable from: the declared name itself, and everything after the
// declaration (an initializer is evaluated before the local exists) to the end
// of the block that holds it.
type shadowSpan struct{ nameStart, nameEnd, end, until int }

func (sp shadowSpan) covers(off int) bool {
	return off >= sp.nameStart && off < sp.nameEnd || off >= sp.end && off < sp.until
}

// shadowStarts returns, per function body of file, the spans a local of name
// shadows. A declaration shadows the uses textually after it in the block it
// stands in, when it is a statement of that block; one under a short-circuit,
// a group or a pipeline shadows nothing. A use outside the block, or before
// the declaration, reads the variable of the caller.
func (s *Snapshot) shadowStarts(file int, name string) map[*tree.Node][]shadowSpan {
	out := map[*tree.Node][]shadowSpan{}
	for _, d := range s.facts[file].Decls {
		if d.Kind != DeclVariable || d.Name != name || !d.Attrs.Local || d.Temp {
			continue
		}
		fn := funcBody(d.Scope)
		if fn == nil || privateScope(d.Scope) != nil || d.Node == nil {
			continue
		}
		p := d.Node
		for p != nil && p.Kind != tree.KBlock && !conditionalKind(p.Kind) {
			p = p.Parent
		}
		if p == nil || p.Kind != tree.KBlock {
			continue
		}
		out[fn] = append(out[fn], shadowSpan{d.NameRange.Start.Offset, d.NameRange.End.Offset, d.Node.Range.End.Offset, p.Range.End.Offset})
	}
	return out
}

// observer reports whether a node other than n is in targets: code a dynamic
// call from n could reach and that would see the name.
func observer(targets map[Node]bool, n Node) bool {
	for t := range targets {
		if t != n {
			return true
		}
	}
	return false
}

// varAccessNodes returns the graph nodes that read or write the variable name
// in the files of in, except at the places skip names: the nodes whose
// accesses dynamic scope may join to a local of a function that calls them. A
// use is left out where a local of that name hides the caller's variable (see
// shadowStarts).
func (s *Snapshot) varAccessNodes(name string, in bitset, skip func(path string, off int) bool) map[Node]bool {
	out := map[Node]bool{}
	in.each(func(f int) {
		var shadow map[*tree.Node][]shadowSpan
		for _, u := range s.facts[f].Uses {
			if u.Kind == UseCall || u.Name != name {
				continue
			}
			off := u.Range.Start.Offset
			if skip != nil && skip(s.paths[f], off) {
				continue
			}
			if fn := funcBody(u.Scope); fn != nil && s.locals[f][fn][name] {
				if shadow == nil {
					shadow = s.shadowStarts(f, name)
				}
				if slices.ContainsFunc(shadow[fn], func(sp shadowSpan) bool { return sp.covers(off) }) {
					continue
				}
			}
			out[s.scopeNode(f, u.Scope)] = true
		}
	})
	return out
}

// declarers returns the nodes of in that declare the variable name: the
// functions with a local of it, and the places that hold a variable of it
// private to a subshell (the enclosing function or file), with the places of
// the declarations.
func (s *Snapshot) declarers(name string, in bitset) (locals, privs []Node, sites []Location) {
	in.each(func(f int) {
		for fn, names := range s.locals[f] {
			if !names[name] {
				continue
			}
			d := s.funcDeclOf(f, fn)
			if d == nil {
				continue
			}
			locals = append(locals, Node{Path: s.paths[f], Func: s.declSym[d]})
			if ls := s.localSyms[localKey{f, fn, name}]; ls != nil {
				sites = append(sites, s.symbolDecls(ls)...)
			}
		}
		for _, e := range s.privByFile[f] {
			if e.sym.Name == name {
				privs = append(privs, s.scopeNode(f, e.sc))
				sites = append(sites, s.symbolDecls(e.sym)...)
			}
		}
	})
	return locals, privs, sites
}

// reachFrom returns every node the call graph reaches from starts, by resolved
// and candidate calls and sources, starts included when inclusive and
// otherwise only when a call leads back to them; ok is false when the search
// met more than maxRenameReach nodes.
func (s *Snapshot) reachFrom(starts []Node, inclusive bool) (map[Node]bool, bool) {
	g := s.adjacency()
	seen := make(map[Node]bool, len(starts))
	queue := append([]Node(nil), starts...)
	if inclusive {
		for _, n := range starts {
			seen[n] = true
		}
	}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for _, i := range g.out[n] {
			to := g.steps[i].To
			if !seen[to] {
				if len(seen) >= maxRenameReach {
					return seen, false
				}
				seen[to] = true
				queue = append(queue, to)
			}
		}
	}
	return seen, true
}

// reachesAny reports whether the call graph from starts reaches a node of
// targets; ok is false when the search was cut.
func (s *Snapshot) reachesAny(starts []Node, inclusive bool, targets map[Node]bool) (found, ok bool) {
	if len(starts) == 0 || len(targets) == 0 {
		return false, true
	}
	r, ok := s.reachFrom(starts, inclusive)
	for n := range targets {
		if r[n] {
			return true, ok
		}
	}
	return false, ok
}

// reachingSet returns the targets and every node from which the call graph
// reaches one of them, one search for all starts; ok is false when the search
// found more than maxRenameReach nodes besides the targets.
func (s *Snapshot) reachingSet(targets map[Node]bool) (map[Node]bool, bool) {
	g := s.adjacency()
	seen := make(map[Node]bool, len(targets))
	queue := make([]Node, 0, len(targets))
	for n := range targets {
		seen[n] = true
		queue = append(queue, n)
	}
	added := 0
	for head := 0; head < len(queue); head++ {
		for _, i := range g.in[queue[head]] {
			from := g.steps[i].From
			if !seen[from] {
				if added >= maxRenameReach {
					return seen, false
				}
				added++
				seen[from] = true
				queue = append(queue, from)
			}
		}
	}
	return seen, true
}

// effectiveCall returns the command word of a call as the shell runs it, and
// the words after it, looking through the builtin and command wrappers.
func effectiveCall(src []byte, words [][2]int) (string, [][2]int) {
	if len(words) == 0 {
		return "", nil
	}
	cmd, args := bareText(src, words[0]), words[1:]
	for len(args) > 0 && (cmd == "builtin" || cmd == "command") {
		if cmd == "command" {
			for len(args) > 0 {
				if t := bareText(src, args[0]); t != "-p" && t != "--" {
					break
				}
				args = args[1:]
			}
			if len(args) == 0 {
				break
			}
			if t := bareText(src, args[0]); t == "-v" || t == "-V" {
				break
			}
		}
		cmd, args = bareText(src, args[0]), args[1:]
	}
	return cmd, args
}

// bareText is the text of a word without one pair of surrounding quotes.
func bareText(src []byte, sp [2]int) string {
	t := spanText(src, sp)
	if len(t) >= 2 && (t[0] == '"' || t[0] == '\'') && t[len(t)-1] == t[0] {
		t = t[1 : len(t)-1]
	}
	return t
}

// forEachCall visits the call nodes of the files of in.
func (s *Snapshot) forEachCall(in bitset, visit func(f int, ff *FileFacts, n *tree.Node)) {
	in.each(func(f int) {
		ff := s.facts[f]
		if ff.File == nil || ff.File.Root == nil {
			return
		}
		ff.File.Root.Walk(func(n *tree.Node) bool {
			if n.Kind == tree.KCall {
				visit(f, ff, n)
			}
			return true
		})
	})
}

// dynamicCallNodes returns the graph nodes of in that run code the call graph
// cannot name: calls through a variable or a parameter that no constant
// resolves, eval of such a string, dynamic sources, and `command` or `builtin`
// followed by an expansion.
func (s *Snapshot) dynamicCallNodes(in bitset) map[Node]bool {
	out := map[Node]bool{}
	in.each(func(f int) {
		for _, d := range s.dyn[f] {
			out[s.scopeNode(f, d.sc)] = true
		}
	})
	at := s.newNodeAtter()
	s.forEachCall(in, func(f int, ff *FileFacts, n *tree.Node) {
		words := wordSpans(n)
		if len(words) == 0 {
			return
		}
		cmd, _ := effectiveCall(ff.File.Src, words)
		if first := bareText(ff.File.Src, words[0]); (first == "command" || first == "builtin") && strings.ContainsAny(cmd, "$`") {
			out[at.at(f, n.Range.Start.Offset)] = true
		}
	})
	return out
}

// dynamicScopeRefusal refuses a rename of a variable that changes what
// dynamic scope makes of it. A function's local is seen by everything that
// function calls, so a use reached through calls from a function that declares
// a local of the name reads that local, not the global. Renaming only one side
// moves the use from one variable to the other. The cases:
//   - a global renamed, while a function with a local of the old name reaches
//     a use of the global;
//   - a global renamed onto the name of a local of a function that reaches one
//     of its uses (capture);
//   - a local renamed, while a function it calls (directly or through others)
//     reads or writes the old name, or the new one (capture);
//   - the same for a variable private to a subshell or command substitution,
//     where a call inside the subshell counts as a call from the function
//     around it;
//   - a function that declares a local of the name makes a call the graph
//     cannot follow;
//   - a prefix assignment of the name on a command that calls a function that
//     reads or writes it.
//
// A use is hidden from a callee only by a local declared before it that runs
// whenever the function does.
func (s *Snapshot) dynamicScopeRefusal(sym *Symbol, newName string, rs *renameSet, union bitset) *RenameRefusal {
	if sym.Kind != DeclVariable || len(rs.edits) == 0 {
		return nil
	}
	old := sym.Name
	site0 := rs.edits[0]
	var dyn map[Node]bool
	hazard := func(locals, privs []Node, targets map[Node]bool, sites []Location, code, reason string) *RenameRefusal {
		if len(locals)+len(privs) == 0 {
			return nil
		}
		if dyn == nil {
			dyn = s.dynamicCallNodes(union)
		}
		for _, n := range slices.Concat(locals, privs) {
			if dyn[n] && observer(targets, n) {
				if code == RefuseDynamicScope {
					reason = reasonDynamicCall
				}
				return s.refuse(code, reason, sites)
			}
		}
		for _, one := range []struct {
			starts    []Node
			inclusive bool
		}{{locals, true}, {privs, false}} {
			found, ok := s.reachesAny(one.starts, one.inclusive, targets)
			if found {
				return s.refuse(code, reason, sites)
			}
			if !ok {
				s.noteRenameCapAt(capReach, site0.Path, site0.Range.Start.Line)
				return s.refuse(RefuseDynamicScope, reasonReachCap, []Location{site0})
			}
		}
		return nil
	}
	accOld := map[Node]bool{}
	switch {
	case sym.File == "": // a global
		accOld = s.varAccessNodes(old, union, nil)
		locals, privs, sites := s.declarers(old, union)
		if ref := hazard(locals, privs, accOld, sites, RefuseDynamicScope, reasonDynamicScope); ref != nil {
			return ref
		}
		locals, privs, sites = s.declarers(newName, union)
		if len(locals)+len(privs) > 0 {
			editNodes := s.editNodes(s.newNodeAtter(), rs.edits)
			if ref := hazard(locals, privs, editNodes, sites, RefuseConflict, reasonDynamicCapture); ref != nil {
				return ref
			}
		}
	default: // a local, or a variable private to a subshell
		file := s.idx[sym.File]
		var start []Node // searched from its callees: its own uses are edited with it
		var skip func(string, int) bool
		if sym.Private != 0 {
			ps := privateScope(sym.Decls[0].Scope)
			if ps == nil {
				return nil
			}
			start = []Node{s.scopeNode(file, ps)}
			own := map[refKey]bool{}
			for _, e := range rs.edits {
				own[refKey{e.Path, e.Range.Start.Offset}] = true
			}
			skip = func(path string, off int) bool { return own[refKey{path, off}] }
		} else {
			fn := funcBody(sym.Decls[0].Scope)
			d := s.funcDeclOf(file, fn)
			if fn == nil || d == nil {
				return s.prefixAssignmentRefusal(old, s.varAccessNodes(old, union, nil), union, site0)
			}
			start = []Node{{Path: sym.File, Func: s.declSym[d]}}
		}
		accOld = s.varAccessNodes(old, union, skip)
		if ref := hazard(nil, start, accOld, s.symbolDecls(sym), RefuseDynamicScope, reasonDynamicScope); ref != nil {
			return ref
		}
		if ref := hazard(nil, start, s.varAccessNodes(newName, union, nil), s.symbolDecls(sym), RefuseConflict, reasonDynamicCapture); ref != nil {
			return ref
		}
	}
	return s.prefixAssignmentRefusal(old, accOld, union, site0)
}

// prefixAssignmentRefusal refuses when `name=v cmd` appears in view and cmd is
// a function that reaches an access of name: the assignment reaches that
// access through the environment of the call, and no rename of the symbol
// edits it. One backward search from the accesses answers for every
// assignment.
func (s *Snapshot) prefixAssignmentRefusal(name string, access map[Node]bool, union bitset, edit Location) *RenameRefusal {
	type assign struct {
		at     Location
		starts []Node
	}
	var pas []assign
	union.each(func(f int) {
		var callees map[*tree.Node][]Node
		for _, d := range s.facts[f].Decls {
			if !d.Temp || d.Name != name || d.Node == nil {
				continue
			}
			call := d.Node
			for call != nil && call.Kind != tree.KCall {
				call = call.Parent
			}
			if call == nil {
				continue
			}
			if callees == nil {
				callees = map[*tree.Node][]Node{}
				for _, b := range s.bindings[f] {
					if b.Use.Kind == UseCall && b.Sym != nil && b.Sym.Kind == DeclFunction {
						for _, fd := range b.Sym.Decls {
							callees[b.Use.Node] = append(callees[b.Use.Node], Node{Path: s.paths[s.declFile[fd]], Func: b.Sym})
						}
					}
				}
			}
			pas = append(pas, assign{s.declLoc(d), callees[call]})
		}
	})
	if len(pas) == 0 || len(access) == 0 {
		return nil
	}
	reaching, ok := s.reachingSet(access)
	if !ok {
		s.noteRenameCapAt(capReach, pas[0].at.Path, pas[0].at.Range.Start.Line)
		return s.refuse(RefuseDynamicScope, reasonReachCap, []Location{pas[0].at})
	}
	var sites []Location
	for _, p := range pas {
		for _, st := range p.starts {
			if reaching[st] {
				sites = append(sites, p.at)
				break
			}
		}
	}
	if len(sites) > 0 {
		return s.refuse(RefuseDynamicScope, reasonPrefixAssign, sites)
	}
	return nil
}

// refWordAt returns the offsets of word in text, a string the shell reads
// again (an eval or trap argument): a whole word where a dot, dash, slash or
// colon joins the words around it (a path, an option, a label), except where
// `$` or `${` makes it a variable, which ends at any of those.
func refWordAt(text []byte, word string) []int {
	out := wordAtIn(text, word, ".-/:")
	for _, at := range wordAt(text, word) {
		if at > 0 && (text[at-1] == '$' || text[at-1] == '{') && !slices.Contains(out, at) {
			out = append(out, at)
		}
	}
	slices.Sort(out)
	return out
}

// newNameInStringRefusal refuses when an eval or trap string that an edited
// function reaches (or holds) contains the new name as a word: the string is
// read again at run time, where the name would mean the renamed symbol.
func (s *Snapshot) newNameInStringRefusal(newName string, rs *renameSet, union bitset) *RenameRefusal {
	if len(rs.edits) == 0 {
		return nil
	}
	at := s.newNodeAtter()
	targets := map[Node]bool{}
	var sites []Location
	s.forEachCall(union, func(f int, ff *FileFacts, n *tree.Node) {
		src := ff.File.Src
		cmd, args := effectiveCall(src, wordSpans(n))
		if cmd != "eval" && cmd != "trap" {
			return
		}
		for _, a := range args {
			if t := spanText(src, a); !isOptionWord(t) && len(refWordAt([]byte(t), newName)) > 0 {
				targets[at.at(f, n.Range.Start.Offset)] = true
				sites = append(sites, Location{Path: s.paths[f], Range: tree.Range{Start: ff.File.PosAt(a[0]), End: ff.File.PosAt(a[1])}})
				break
			}
		}
	})
	if len(targets) == 0 {
		return nil
	}
	reaching, ok := s.reachingSet(targets)
	if !ok {
		s.noteRenameCapAt(capReach, rs.edits[0].Path, rs.edits[0].Range.Start.Line)
		return s.refuse(RefuseDynamicScope, reasonReachCap, []Location{rs.edits[0]})
	}
	for n := range s.editNodes(at, rs.edits) {
		if reaching[n] {
			return s.refuse(RefuseConflict, reasonNewNameInString, sites)
		}
	}
	return nil
}

// refKind says what a written name refers to.
type refKind uint8

const (
	refVar  refKind = 1 << iota // a variable
	refFunc                     // a function, alias or other command name
)

// referenceScope returns the filter of the name references that matter to a
// variable confined to a function or a subshell: those inside it and inside
// what it calls. nil for a symbol with no such confinement, or when the call
// graph around it is too large to tell.
func (s *Snapshot) referenceScope(sym *Symbol) func(f, off int) bool {
	if sym.Kind != DeclVariable || sym.File == "" {
		return nil
	}
	file, ok := s.idx[sym.File]
	if !ok {
		return nil
	}
	var region *tree.Node
	var start Node
	if sym.Private != 0 {
		ps := privateScope(sym.Decls[0].Scope)
		if ps == nil {
			return nil
		}
		region, start = ps.Node, s.scopeNode(file, ps)
	} else {
		fn := funcBody(sym.Decls[0].Scope)
		if fn == nil {
			return nil
		}
		region = fn
		start = Node{Path: sym.File}
		if d := s.funcDeclOf(file, fn); d != nil {
			start.Func = s.declSym[d]
		}
	}
	reach, ok := s.reachFrom([]Node{start}, true)
	if !ok {
		return nil
	}
	at := s.newNodeAtter()
	return func(f, off int) bool {
		if f == file && off >= region.Range.Start.Offset && off < region.Range.End.Offset {
			return true
		}
		return reach[at.at(f, off)]
	}
}

// nameReferenceRefusal refuses when the files in view write the name where the
// binder finds no use of it, and the rename would leave that text behind. By
// kind: for a variable, the arguments of unset (without -f), `test -v` and
// `[[ -v ]]`, the targets of declare, typeset, local, readonly and export with
// -n, an argument that is the bare name in a call of a function (a name passed
// to a function that reads it indirectly), and a literal value equal to the
// name in a file that expands indirectly (`${!ref}`) or declares namerefs; for
// a function, the arguments of unset -f, type, `command -v`, trap, the targets
// of declare, typeset and export with -f or -F, the function of `complete -F`
// and the value of PROMPT_COMMAND. The strings of trap and eval hold either.
// A confined variable is checked in its function and what that calls. A place
// that an edit or an exclusion covers is already accounted for.
func (s *Snapshot) nameReferenceRefusal(sym *Symbol, union bitset, rs *renameSet) *RenameRefusal {
	old := sym.Name
	want := refFunc
	if sym.Kind == DeclVariable {
		want = refVar
	}
	scoped := s.referenceScope(sym)
	var sites []Location
	union.each(func(f int) {
		ff := s.facts[f]
		if ff.File == nil || ff.File.Root == nil {
			return
		}
		src := ff.File.Src
		path := s.paths[f]
		note := func(k refKind, strs bool, from, to int) {
			if k&want == 0 || from < 0 || to > len(src) || from > to {
				return
			}
			find := wordAt
			if strs {
				find = refWordAt
			}
			for _, at := range find(src[from:to], old) {
				off := from + at
				if rs.covered(path, off) || scoped != nil && !scoped(f, off) {
					continue
				}
				sites = append(sites, Location{Path: path, Range: tree.Range{Start: ff.File.PosAt(off), End: ff.File.PosAt(off + len(old))}})
			}
		}
		var unknown map[*tree.Node]bool
		if want == refVar {
			unknown = s.unknownCommandCalls(f)
		}
		indirect := false
		ff.File.Root.Walk(func(n *tree.Node) bool {
			switch n.Kind {
			case tree.KParamExp:
				if a := n.Range.Start.Offset; a >= 0 && a+3 <= len(src) && string(src[a:a+3]) == "${!" {
					indirect = true
				}
			case tree.KTest:
				s.noteTestV(src, n, note)
			case tree.KCall:
				s.noteCallReferences(src, n, old, unknown[n], note)
			case tree.KDecl:
				s.noteDeclReferences(src, n, note)
			}
			return true
		})
		for _, d := range ff.Decls {
			if d.Name == "PROMPT_COMMAND" && d.Node != nil {
				note(refFunc, true, d.Node.Range.Start.Offset, d.Node.Range.End.Offset)
			}
		}
		if want == refVar {
			nameref := false
			for _, d := range ff.Decls {
				nameref = nameref || d.Attrs.Nameref
			}
			if indirect || nameref {
				for _, d := range ff.Decls {
					if d.Kind == DeclVariable && d.Literal == old && !rs.covered(path, d.NameRange.Start.Offset) &&
						(scoped == nil || scoped(f, d.NameRange.Start.Offset)) {
						sites = append(sites, Location{Path: path, Range: d.NameRange})
					}
				}
			}
		}
	})
	if len(sites) > 0 {
		return s.refuse(RefuseNameReference, reasonNameReference, sites)
	}
	return nil
}

// wordSpans returns the source offsets of the KWord children of n.
func wordSpans(n *tree.Node) [][2]int {
	var out [][2]int
	for _, c := range n.Children {
		if c.Kind == tree.KWord {
			out = append(out, [2]int{c.Range.Start.Offset, c.Range.End.Offset})
		}
	}
	return out
}

func spanText(src []byte, sp [2]int) string {
	if sp[0] < 0 || sp[1] > len(src) || sp[0] > sp[1] {
		return ""
	}
	return string(src[sp[0]:sp[1]])
}

// unknownCommandCalls returns the call nodes of file whose command is neither a
// function, an alias or a builtin of the workspace nor a command of the PATH
// profile: a function defined outside the workspace may be what runs. With no
// profile the snapshot has no command edges and none are known.
func (s *Snapshot) unknownCommandCalls(file int) map[*tree.Node]bool {
	var out map[*tree.Node]bool
	for _, r := range s.recs[s.recAt[file]:s.recAt[file+1]] {
		if r.cmd && r.res == Unresolved {
			if out == nil {
				out = map[*tree.Node]bool{}
			}
			out[s.facts[file].Uses[r.at].Node] = true
		}
	}
	return out
}

// noteCallReferences notes the name-bearing arguments of the commands whose
// arguments name functions or variables without being uses, and the arguments
// of a function (or of a command that may be one: unknown) that are the bare
// name of a variable.
func (s *Snapshot) noteCallReferences(src []byte, n *tree.Node, old string, unknown bool, note func(k refKind, strs bool, from, to int)) {
	words := wordSpans(n)
	if len(words) < 2 {
		return
	}
	if first := bareText(src, words[0]); len(s.funcSyms[first]) > 0 || unknown {
		for _, a := range words[1:] {
			if bareText(src, a) == old {
				note(refVar, false, a[0], a[1])
			}
		}
		return
	}
	cmd, args := effectiveCall(src, words)
	options := func() (opts []string, rest [][2]int) {
		for _, a := range args {
			if t := spanText(src, a); isOptionWord(t) {
				opts = append(opts, t)
			} else {
				rest = append(rest, a)
			}
		}
		return
	}
	opts, rest := options()
	hasOpt := func(letters string) bool {
		for _, o := range opts {
			if strings.ContainsAny(o[1:], letters) {
				return true
			}
		}
		return false
	}
	switch cmd {
	case "unset":
		k := refVar
		for _, o := range opts {
			switch {
			case strings.Contains(o, "f"):
				k = refFunc
			case strings.Contains(o, "v"):
				k = refVar
			}
		}
		for _, a := range rest {
			note(k, false, a[0], a[1])
		}
	case "type":
		if !hasOpt("Pp") {
			for _, a := range rest {
				note(refFunc, false, a[0], a[1])
			}
		}
	case "command": // only `command -v` and `command -V` get here
		for _, a := range rest {
			note(refFunc, false, a[0], a[1])
		}
	case "trap", "eval":
		for _, a := range rest {
			note(refVar|refFunc, true, a[0], a[1])
		}
	case "test", "[":
		if hasOpt("v") {
			for _, a := range rest {
				note(refVar, false, a[0], a[1])
			}
		}
	case "complete":
		for i, a := range args {
			if spanText(src, a) == "-F" && i+1 < len(args) {
				note(refFunc, false, args[i+1][0], args[i+1][1])
			}
		}
	}
}

// isOptionWord reports whether t is a bare option such as -f or -nr.
func isOptionWord(t string) bool {
	if len(t) < 2 || t[0] != '-' {
		return false
	}
	for _, c := range []byte(t[1:]) {
		if c < 'A' || c > 'z' || c > 'Z' && c < 'a' {
			return false
		}
	}
	return true
}

// noteDeclReferences notes the arguments of a declaration builtin that names
// namerefs (-n) or functions (-f, -F).
func (s *Snapshot) noteDeclReferences(src []byte, n *tree.Node, note func(k refKind, strs bool, from, to int)) {
	var nameref, function bool
	for _, c := range n.Children {
		if t := spanText(src, [2]int{c.Range.Start.Offset, c.Range.End.Offset}); strings.HasPrefix(t, "-") {
			nameref = nameref || strings.Contains(t[1:], "n")
			function = function || strings.ContainsAny(t[1:], "fF")
		}
	}
	for _, c := range n.Children {
		t := spanText(src, [2]int{c.Range.Start.Offset, c.Range.End.Offset})
		if strings.HasPrefix(t, "-") {
			continue
		}
		if nameref {
			note(refVar, false, c.Range.Start.Offset, c.Range.End.Offset)
		}
		if function {
			note(refFunc, false, c.Range.Start.Offset, c.Range.End.Offset)
		}
	}
}

// noteTestV notes the operand of -v in a `[[ ]]` test.
func (s *Snapshot) noteTestV(src []byte, n *tree.Node, note func(k refKind, strs bool, from, to int)) {
	a, b := n.Range.Start.Offset, n.Range.End.Offset
	if a < 0 || b > len(src) || a > b {
		return
	}
	text := src[a:b]
	var toks [][2]int
	for i := 0; i < len(text); {
		for i < len(text) && (text[i] == ' ' || text[i] == '\t' || text[i] == '\n') {
			i++
		}
		j := i
		for j < len(text) && text[j] != ' ' && text[j] != '\t' && text[j] != '\n' {
			j++
		}
		if j > i {
			toks = append(toks, [2]int{i, j})
		}
		i = j
	}
	for k := 0; k+1 < len(toks); k++ {
		if string(text[toks[k][0]:toks[k][1]]) == "-v" {
			note(refVar, false, a+toks[k+1][0], a+toks[k+1][1])
		}
	}
}

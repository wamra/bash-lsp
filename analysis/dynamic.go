package analysis

import (
	"slices"
	"strings"

	"github.com/wamra/bash-lsp/parse"
	"github.com/wamra/bash-lsp/tree"
)

// Reason strings of string, variable and dynamic-scope bindings and edges.
const (
	ReasonVariableConstant = "variable-constant"
	ReasonStringEval       = "string-eval"
	ReasonStringTrap       = "string-trap"
	ReasonStringShellC     = "string-shell-c"
	ReasonVariableUnknown  = "variable-unknown"
	ReasonExpansion        = "expansion"
	ReasonDynamicScope     = "dynamic-scope" // variable reads only; never an edge
	// ReasonUnsetInView: an edge to a function whose name some file in the
	// site's visibility closure removes with `unset -f`. Order
	// is not tracked, so such an edge is never resolved.
	ReasonUnsetInView = "unset-in-view"
	// ReasonCandidateCap: a Dynamic edge at a call site with more than
	// maxSiteTargets candidate definitions (a call by name) or targets (a call
	// through a variable); the edges to the first maxSiteTargets are listed.
	ReasonCandidateCap = "candidate-cap"
	// ReasonStringBudget: a Dynamic edge at a string body that was not parsed
	// because the file's string-parse budget (MaxFileBytes) is used up.
	ReasonStringBudget = "string-budget"
	// ReasonDynamicScopeCap: a binding reason, never an edge and not in
	// reasonNames: a variable read is bound to the first maxDynScopeLocals
	// caller locals of its name only (Snapshot.LimitsHit says how many reads).
	ReasonDynamicScopeCap = "dynamic-scope-cap"
)

// maxSiteTargets is the most targets one call site lists: candidate
// definitions of a name, or functions a variable call may reach. A site with
// more has the first maxSiteTargets and one dynamic/candidate-cap edge.
const maxSiteTargets = 16

// maxDynScopeLocals is the most caller locals one variable read binds to
// through dynamic scope.
const maxDynScopeLocals = 16

// Via says how a UseCall names its target when it is not a plain command
// word (Use.ViaAlias, the target inside an alias definition, is separate).
type Via uint8

const (
	ViaNone Via = iota
	// ViaEval, ViaTrap, ViaShellC: a command inside the string argument of
	// eval, of trap, or of bash/sh/dash/ksh -c. The range is the command
	// name inside the string, mapped back to the file, so its text is the name.
	ViaEval
	ViaTrap
	ViaShellC
	// ViaVariable: a command word that is exactly one parameter expansion
	// (`"$cmd"`, `$cmd`, `${cmd}`, `"${cmd}"`). Use.Name is the parameter,
	// not a function, and the range text does not spell the target's name:
	// a rename must not rewrite it.
	ViaVariable
	// ViaValue: made by Build, never by Bind. The literal value of an
	// assignment (`cmd=build_release`) through which a ViaVariable use
	// resolved; the range text is the function's name. No edge has it as site.
	ViaValue
)

// NamesTarget reports whether the text of u's range is exactly u.Name: false
// for a ViaVariable word (`"$cmd"`), and for a name written with quotes,
// escapes or line continuations (`eval \f`, `eval f\<newline>oo`).
func (u *Use) NamesTarget() bool { return u.Via != ViaVariable && !u.inexact }

// reason is the binding and edge reason of a call made this way, "" for ViaNone.
func (v Via) reason() string {
	switch v {
	case ViaEval:
		return ReasonStringEval
	case ViaTrap:
		return ReasonStringTrap
	case ViaShellC:
		return ReasonStringShellC
	case ViaVariable, ViaValue:
		return ReasonVariableConstant
	}
	return ""
}

// DynamicRef is a command whose name is an expansion with no known target:
// `$(pick) x`, `${a}_${b}`, the string of `eval "$code"`, or a string nested
// deeper than maxStringDepth. Range is the expression (in the file); Node is
// the command in the file that holds it.
type DynamicRef struct {
	Range  tree.Range
	Scope  *Scope
	Node   *tree.Node
	Budget bool // a string body not parsed because the file's string-parse budget is used up, not because of its depth
}

// InvokeRef is the execution of a file by literal path: a command word with
// a `/` (`./x.sh`), or the script operand of bash/sh/dash/ksh (`bash x.sh`).
type InvokeRef struct {
	Node  *tree.Node
	Word  *tree.Node
	Range tree.Range
	Scope *Scope
}

// maxStringDepth bounds strings inside strings: a string at this depth is
// parsed, one inside it is a dynamic/expansion site.
const maxStringDepth = 3

// shells are the commands whose -c string is code and whose first operand
// otherwise is a script.
var shells = map[string]bool{"bash": true, "sh": true, "dash": true, "ksh": true}

// fragMap maps a string body back to the file: byte i of src starts at file
// offset offs[i], and a range of src ending before byte i ends at ends[i]
// (ends has len(src)+1 entries). A byte that stands for several file bytes
// (an escape `\"`, or the placeholder for an expansion) spans all of them.
// offs nil means src is the file itself.
type fragMap struct {
	src  []byte
	offs []int
	ends []int
}

func (m *fragMap) off(i int) int {
	i = max(0, min(i, len(m.src)))
	if m.offs == nil {
		return i
	}
	if i == len(m.src) {
		return m.ends[i]
	}
	return m.offs[i]
}

func (m *fragMap) end(i int) int {
	i = max(0, min(i, len(m.src)))
	if m.offs == nil {
		return i
	}
	return m.ends[i]
}

// fileRange maps the range [a, e) of m's tree to a range in the file.
func (b *binder) fileRange(m *fragMap, a, e int) tree.Range {
	s, t := m.off(a), m.end(e)
	if e <= a {
		t = s
	}
	return tree.Range{Start: b.ff.File.PosAt(s), End: b.ff.File.PosAt(max(s, t))}
}

// dynamicName reports whether a command of this name needs dynamicCall:
// eval, trap, a shell, or a path.
func dynamicName(name string) bool {
	return name == "eval" || name == "trap" || shells[name] || strings.IndexByte(name, '/') >= 0
}

// isShell reports whether name is one of shells, by basename when it is a
// path (`/bin/sh`, `/usr/bin/env` is not).
func isShell(name string) bool {
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	return shells[name]
}

// dynamicCall looks at a command whose name is the literal name: eval, trap
// and shells get their string parsed; a path or a shell's script operand is
// an invocation (file level only). depth is the string depth of m (0: file).
func (b *binder) dynamicCall(n *tree.Node, name string, args []*tree.Node, sc *Scope, m *fragMap, depth int) {
	switch {
	case name == "eval":
		if len(args) > 0 {
			if t, ok := literalText(args[0]); ok && t == "--" { // bash 3.2 runs `eval -- echo hi`
				args = args[1:]
			}
		}
		if len(args) > 0 {
			b.fragment(ViaEval, args, n, sc, m, depth)
		}
	case name == "trap":
		if a := trapAction(args); a != nil {
			b.fragment(ViaTrap, []*tree.Node{a}, n, sc, m, depth)
		}
	case isShell(name):
		w, cmd := shellOperand(args)
		switch {
		case w == nil:
		case cmd:
			b.fragment(ViaShellC, []*tree.Node{w}, n, sc, m, depth)
		case depth == 0:
			if t, ok := literalText(w); ok && t != "" {
				b.ff.Invokes = append(b.ff.Invokes, InvokeRef{Node: n, Word: w, Range: w.Range, Scope: sc})
			}
		}
	case depth == 0 && strings.Contains(name, "/"):
		for _, c := range n.Children { // the command word itself
			if c.Kind == tree.KWord {
				b.ff.Invokes = append(b.ff.Invokes, InvokeRef{Node: n, Word: c, Range: c.Range, Scope: sc})
				break
			}
		}
	}
}

// trapAction returns the action argument of trap, nil for the forms that
// install nothing. `help trap` (bash 3.2): trap [-lp] [arg signal_spec ...].
// -l and -p list; one argument alone is a signal to reset; an action `-` resets
// and an empty one ignores; a numeric first argument is a signal (checked:
// `trap 0 INT` resets EXIT on /bin/bash 3.2). `--` ends the options.
func trapAction(args []*tree.Node) *tree.Node {
	i := 0
	if i < len(args) {
		if t, ok := literalText(args[i]); ok && len(t) > 1 && t[0] == '-' {
			if t != "--" {
				return nil
			}
			i++
		}
	}
	rest := args[i:]
	if len(rest) < 2 {
		return nil
	}
	if t, ok := literalText(rest[0]); ok && (t == "" || t == "-" || allDigits(t)) {
		return nil
	}
	return rest[0]
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// shellOperand returns the first operand of a shell command line and whether
// -c was given (then it is the command string, else a script path). From
// `man bash` OPTIONS: -c reads commands from the first non-option argument
// (verified on /bin/bash 3.2: -ec, -ce, `-c -e`, `-o pipefail -c`); -o/+o
// and -O/+O take an argument, so do --rcfile and --init-file; `--` or `-`
// ends the options. -R file is ksh's (`man ksh` SYNOPSIS). -s (read stdin)
// means no script.
func shellOperand(args []*tree.Node) (w *tree.Node, cmd bool) {
	for i := 0; i < len(args); i++ {
		t, ok := literalText(args[i])
		if !ok {
			return args[i], cmd
		}
		switch {
		case t == "--" || t == "-":
			if i+1 < len(args) {
				return args[i+1], cmd
			}
			return nil, cmd
		case strings.HasPrefix(t, "--"):
			if t == "--rcfile" || t == "--init-file" {
				i++
			}
		case len(t) > 1 && (t[0] == '-' || t[0] == '+'):
			if t[0] == '-' && strings.IndexByte(t, 'c') > 0 {
				cmd = true
			}
			if t[0] == '-' && !cmd && strings.IndexByte(t, 's') > 0 {
				return nil, false
			}
			if strings.IndexByte("oOR", t[len(t)-1]) >= 0 {
				i++
			}
		default:
			return args[i], cmd
		}
	}
	return nil, cmd
}

// fragment parses the words args of m (joined with spaces, as eval does)
// as shell and records each command inside: a call use marked via, a
// dynamic site for a command name that is an expansion, and the strings
// inside it, one level deeper.
func (b *binder) fragment(via Via, args []*tree.Node, n *tree.Node, sc *Scope, m *fragMap, depth int) {
	if depth >= maxStringDepth {
		b.ff.Dynamic = append(b.ff.Dynamic, DynamicRef{Range: b.fileRange(m, args[0].Range.Start.Offset, args[len(args)-1].Range.End.Offset), Scope: sc, Node: n})
		return
	}
	est := fragmentSize(args, m.src)
	if est > b.fragLeft {
		b.ff.Dynamic = append(b.ff.Dynamic, DynamicRef{Range: b.fileRange(m, args[0].Range.Start.Offset, args[len(args)-1].Range.End.Offset), Scope: sc, Node: n, Budget: true})
		return
	}
	b.fragLeft -= est
	fm := buildFragment(args, m)
	f := parse.Fragment(fm.src, b.ff.File.Dialect)
	if f.Root == nil {
		return
	}
	f.Root.Walk(func(c *tree.Node) bool {
		if c.Kind != tree.KCall {
			return true
		}
		var words []*tree.Node
		for _, w := range c.Children {
			if w.Kind == tree.KWord {
				words = append(words, w)
			}
		}
		if len(words) == 0 {
			return true
		}
		name, ok, escaped := commandName(words[0])
		switch {
		case ok && name != "":
			if name == "source" || name == "." || name == "alias" {
				return true // not resolved inside strings
			}
			a, e := words[0].Range.Start.Offset, words[0].Range.End.Offset
			u := b.use(UseCall, name, b.fileRange(fm, a, e), sc, n)
			u.Via, u.noAlias = via, escaped
			r := u.Range
			u.inexact = !fm.verbatim(a, e, len(name)) || string(b.src[r.Start.Offset:r.End.Offset]) != name
			b.dynamicCall(n, name, words[1:], sc, fm, depth+1)
		case !ok && hasExpansion(words[0]):
			b.ff.Dynamic = append(b.ff.Dynamic, DynamicRef{Range: b.fileRange(fm, words[0].Range.Start.Offset, words[0].Range.End.Offset), Scope: sc, Node: n})
		}
		return true
	})
}

// fragmentSize is an upper bound on the length of the text buildFragment
// makes for words: literal bytes count once, every other part counts as a
// placeholder, plus the joining spaces. Bind compares it with what is left
// of the per-file budget (MaxFileBytes in all, across every string and
// nesting level) before building anything.
func fragmentSize(words []*tree.Node, src []byte) int {
	n := len(words)
	for _, w := range words {
		for _, p := range w.Children {
			s, e := p.Range.Start.Offset, p.Range.End.Offset
			switch p.Kind {
			case tree.KLit, tree.KSglQuoted:
				n += max(0, e-s)
			case tree.KDblQuoted:
				for _, q := range p.Children {
					if q.Kind == tree.KLit {
						n += max(0, q.Range.End.Offset-q.Range.Start.Offset)
					} else {
						n += len(placeholder)
					}
				}
			default:
				n += len(placeholder)
			}
		}
	}
	return n
}

// verbatim reports whether the n bytes of m at [a, e) are copied 1:1 from
// consecutive file bytes, so the file text of the mapped range is the same
// as m's text there.
func (m *fragMap) verbatim(a, e, n int) bool {
	if e-a != n || a < 0 || e > len(m.src) || a >= e {
		return false
	}
	start := m.off(a)
	for i := a; i < e; i++ {
		if m.off(i) != start+(i-a) {
			return false
		}
	}
	return m.end(e) == start+n
}

// crlfAt reports whether src has CR LF at i.
func crlfAt(src []byte, i int) bool { return i+1 < len(src) && src[i] == '\r' && src[i+1] == '\n' }

// placeholder stands for an expansion of the enclosing level inside a string
// body: its value is known only when that level runs, so it is one opaque
// parameter expansion (self-delimiting, so a following name does not join it).
const placeholder = "${__wb}"

// buildFragment makes the text the shell runs for words of m: quotes
// removed, the escapes of each quoting context applied, and every expansion
// of m's level replaced by the placeholder; with the map back to the file.
func buildFragment(words []*tree.Node, m *fragMap) *fragMap {
	fm := &fragMap{ends: []int{m.off(words[0].Range.Start.Offset)}}
	add := func(c byte, start, end int) {
		fm.src = append(fm.src, c)
		fm.offs = append(fm.offs, start)
		fm.ends = append(fm.ends, end)
	}
	src := m.src
	clamp := func(i int) int { return max(0, min(i, len(src))) }
	prev := fm.ends[0]      // file offset where the text so far ends
	one := func(i, w int) { // byte i of m's src, standing for w bytes of it
		add(src[i+w-1], m.off(i), m.end(i+w))
		prev = m.end(i + w)
	}
	crlf := func(i int) { // the CR LF at i, which the parser left out of both Lits
		one(i, 1)
		one(i+1, 1)
	}
	opaque := func(s, e int) {
		for k := 0; k < len(placeholder)-1; k++ {
			add(placeholder[k], m.off(s), m.off(s))
		}
		add(placeholder[len(placeholder)-1], m.off(s), m.end(e))
		prev = m.end(e)
	}
	for wi, w := range words {
		if wi > 0 {
			add(' ', prev, prev)
		}
		for _, p := range w.Children {
			s, e := clamp(p.Range.Start.Offset), clamp(p.Range.End.Offset)
			switch {
			case p.Kind == tree.KLit && !p.Recovered:
				for i := s; i < e; i++ { // unquoted: a backslash quotes the next byte
					if src[i] == '\\' && i+1 == e && crlfAt(src, e) {
						one(i, 1)
						crlf(e)
						continue
					}
					if src[i] == '\\' && i+1 == e && e < len(src) && src[e] == '\n' {
						continue // a line continuation the parser left between two Lits
					}
					if src[i] == '\\' && i+1 < e {
						if src[i+1] != '\n' {
							one(i, 2)
						}
						i++
						continue
					}
					one(i, 1)
				}
			case p.Kind == tree.KSglQuoted && s < e && src[s] != '$' && e-s >= 2:
				for i := s + 1; i < e-1; i++ {
					one(i, 1)
				}
			case p.Kind == tree.KDblQuoted:
				for _, q := range p.Children {
					qs, qe := clamp(q.Range.Start.Offset), clamp(q.Range.End.Offset)
					if q.Kind != tree.KLit || q.Recovered {
						opaque(qs, qe)
						continue
					}
					for i := qs; i < qe; i++ { // double quotes: \ quotes only $ ` " \ and newline
						if src[i] == '\\' && i+1 == qe && crlfAt(src, qe) {
							one(i, 1) // bash: `\<CR>` is no continuation, the parser's split is undone
							crlf(qe)
							continue
						}
						if src[i] == '\\' && i+1 == qe && qe < len(src) && src[qe] == '\n' {
							continue // the parser splits the Lit at a line continuation
						}
						if src[i] == '\\' && i+1 < qe && strings.IndexByte("$`\"\\\n", src[i+1]) >= 0 {
							if src[i+1] != '\n' {
								one(i, 2)
							}
							i++
							continue
						}
						one(i, 1)
					}
				}
			default:
				opaque(s, e)
			}
		}
	}
	return fm
}

// hasExpansion reports whether word w has a part that expands (anything
// but literals and quoted literals), so a non-literal word that is merely
// recovered from a syntax error does not count.
func hasExpansion(w *tree.Node) bool {
	for _, p := range w.Children {
		switch p.Kind {
		case tree.KLit, tree.KSglQuoted:
		case tree.KDblQuoted:
			for _, q := range p.Children {
				if q.Kind != tree.KLit {
					return true
				}
			}
		default:
			return true
		}
	}
	return false
}

// expansionCommand records a file-level command word that is not literal:
// exactly one parameter expansion is a ViaVariable use, resolved by Build;
// anything else is a dynamic/expansion site.
func (b *binder) expansionCommand(n, w *tree.Node, sc *Scope) {
	if !hasExpansion(w) {
		return
	}
	if name := soleParam(w, b.src); name != "" {
		u := b.use(UseCall, name, w.Range, sc, n)
		u.Via = ViaVariable
		return
	}
	b.ff.Dynamic = append(b.ff.Dynamic, DynamicRef{Range: w.Range, Scope: sc, Node: n})
}

// soleParam returns the parameter of a word that is exactly `$p`, `${p}`,
// `"$p"` or `"${p}"`, "" otherwise.
func soleParam(w *tree.Node, src []byte) string {
	if len(w.Children) != 1 {
		return ""
	}
	p := w.Children[0]
	if p.Kind == tree.KDblQuoted {
		if len(p.Children) != 1 || p.Range.End.Offset-p.Range.Start.Offset != p.Children[0].Range.End.Offset-p.Children[0].Range.Start.Offset+2 {
			return ""
		}
		p = p.Children[0]
	}
	if p.Kind != tree.KParamExp || p.Name == "" || len(p.Children) != 0 {
		return ""
	}
	s, e := p.Range.Start.Offset, p.Range.End.Offset
	if s < 0 || e > len(src) || s > e {
		return ""
	}
	if t := string(src[s:e]); t != "$"+p.Name && t != "${"+p.Name+"}" {
		return ""
	}
	return p.Name
}

// unsetArgs records the literal names of `unset -f NAME...` (options may be
// combined, `-fv`; `--` ends them). Plain `unset NAME` is not recorded.
func (b *binder) unsetArgs(args []*tree.Node) {
	funcs := false
	for i, w := range args {
		t, ok := literalText(w)
		if !ok {
			continue
		}
		if t == "--" {
			args = args[i+1:]
			break
		}
		if len(t) > 1 && t[0] == '-' {
			funcs = funcs || strings.IndexByte(t, 'f') > 0
			continue
		}
		args = args[i:]
		break
	}
	if !funcs {
		return
	}
	for _, w := range args {
		if t, ok := literalText(w); ok && t != "" && t[0] != '-' {
			b.ff.Unsets = append(b.ff.Unsets, t)
		}
	}
}

// --- Build side ---

// dynSite is a dynamic edge's site: a DynamicRef of the facts
// (dynamic/expansion) or a ViaVariable use with no constant value
// (dynamic/variable-unknown).
type dynSite struct {
	rng    tree.Range
	sc     *Scope
	reason uint8
}

// invokeSite is one resolved target of an InvokeRef.
type invokeSite struct {
	ref    *InvokeRef
	target string
}

// valueKey is one literal-value binding: the assignment and the function.
type valueKey struct {
	d   *Decl
	sym *Symbol
}

// unsetInView reports whether some file visible from file has `unset -f name`.
func (s *Snapshot) unsetInView(file int, name string) bool {
	for _, f := range s.unsetBy[name] {
		if s.visible[file].has(f) {
			return true
		}
	}
	return false
}

// applyUnset relabels the edges of recs (all with sites in file) whose
// target function is named by an `unset -f` in view:
// candidate/unset-in-view, whatever they were.
func (s *Snapshot) applyUnset(recs []edgeRec, file int) {
	if len(s.unsetBy) == 0 {
		return
	}
	for k := range recs {
		if d := recs[k].to; d != nil && d.Kind == DeclFunction && s.unsetInView(file, d.Name) {
			recs[k].res, recs[k].reason = Candidate, codeUnsetInView
		}
	}
}

// tgtDynamic marks an edgeRec of rel RelCalls whose at indexes the file's
// dynSites instead of its bindings.
const tgtDynamic = -2

var (
	codeVariableUnknown = reasonCode(ReasonVariableUnknown)
	codeExpansion       = reasonCode(ReasonExpansion)
	codeUnsetInView     = reasonCode(ReasonUnsetInView)
	codeCandidateCap    = reasonCode(ReasonCandidateCap)
	codeStringBudget    = reasonCode(ReasonStringBudget)
)

// prepareDynamic starts the dynamic sites of every file from its facts.
func (s *Snapshot) prepareDynamic() {
	s.dyn = make([][]dynSite, len(s.paths))
	s.valueSeen = map[valueKey]bool{}
	s.valueUse = map[*Decl]*Use{}
	s.varVals = map[varValuesKey]*varValues{}
	s.unsetBy = map[string][]int{}
	for i, ff := range s.facts {
		for _, name := range ff.Unsets {
			if !slices.Contains(s.unsetBy[name], i) {
				s.unsetBy[name] = append(s.unsetBy[name], i)
			}
		}
	}
	for i, ff := range s.facts {
		for _, d := range ff.Dynamic {
			reason := codeExpansion
			if d.Budget {
				reason = codeStringBudget
			}
			s.dyn[i] = append(s.dyn[i], dynSite{rng: d.Range, sc: d.Scope, reason: reason})
		}
	}
}

// bindVariableCall binds a ViaVariable use: when every declaration of the
// variable in view (by the variable binding rules) assigns a literal and
// each literal names a function in view, one Candidate/variable-constant
// binding per distinct function; otherwise none, and a
// dynamic/variable-unknown site.
//
// A declaration in view that assigns nothing (`local cmd`, `export cmd`)
// is not counted; one that is not a plain literal assignment (`read`, a for
// loop, `+=`, an array, a nameref or integer, an expansion) makes the value
// unknown, as does a literal that is no function in view (`cmd=echo`).
//
// priv is the private symbol of the variable at the call (privateOf), nil
// when none.
func (s *Snapshot) bindVariableCall(out []Binding, file int, u *Use, priv *Symbol) []Binding {
	unknown := func() []Binding {
		s.dyn[file] = append(s.dyn[file], dynSite{rng: u.Range, sc: u.Scope, reason: codeVariableUnknown})
		return out
	}
	if !identRe.MatchString(u.Name) {
		return unknown()
	}
	vars := s.bindVar(nil, file, &Use{Kind: UseVarRead, Name: u.Name, Range: u.Range, Scope: u.Scope}, priv)
	var targets []*Symbol
	vals := make([]*varValues, 0, len(vars))
	for _, vb := range vars {
		if vb.Res != Resolved || vb.Sym == nil {
			return unknown()
		}
		v := s.valuesOf(vb.Sym, file)
		if v.unknown {
			return unknown()
		}
		vals = append(vals, v)
		for _, f := range v.targets {
			if len(targets) > maxSiteTargets {
				break // one past the cap says there are more; the rest is not collected
			}
			if !slices.Contains(targets, f) {
				targets = append(targets, f)
			}
		}
	}
	if len(targets) > maxSiteTargets {
		targets = targets[:maxSiteTargets]
		if s.varCap == nil {
			s.varCap = map[*Use]bool{}
		}
		s.varCap[u] = true
	}
	if len(targets) == 0 {
		return unknown()
	}
	b := Binding{Use: u, File: s.paths[file], Res: Candidate, Reason: ReasonVariableConstant}
	for _, t := range targets {
		b.Sym = t
		out = append(out, b)
	}
	for _, v := range vals {
		if !v.valuesBound {
			v.valuesBound = true
			s.bindValues(v.lits)
		}
	}
	return out
}

// varValues is what the declarations in view of one variable symbol from
// one file hold, for "$cmd" calls (bindVariableCall): unknown, or the
// literal assignments with the functions each names, and those functions in
// order of first appearance. Computed once per (symbol, file) in a Build: a
// scan per call cost O(calls x assignments), 9.3 s for 128 KiB of
// `cmd=f; "$cmd";`.
type varValues struct {
	unknown     bool
	lits        []varLit
	targets     []*Symbol
	valuesBound bool // bindValues has run on lits
}

type varLit struct {
	d    *Decl
	syms []*Symbol
}

type varValuesKey struct {
	sym  *Symbol
	file int
}

// valuesOf returns the varValues of sym seen from file.
func (s *Snapshot) valuesOf(sym *Symbol, file int) *varValues {
	k := varValuesKey{sym, file}
	if v := s.varVals[k]; v != nil {
		return v
	}
	v := &varValues{}
	s.varVals[k] = v
	for _, d := range sym.Decls {
		if !s.visible[file].has(s.declFile[d]) || d.Temp {
			continue
		}
		if d.Implicit == "" && d.Node != nil && d.Node.Kind == tree.KAssign && !s.assigns(d) {
			continue // `local cmd`: declares, assigns nothing
		}
		if !s.plainLiteral(d) {
			v.unknown = true
			return v
		}
		fs := s.pickAll(s.funcSyms[d.Literal], file)
		if len(fs) == 0 {
			v.unknown = true
			return v
		}
		v.lits = append(v.lits, varLit{d, fs})
		for _, f := range fs {
			if !slices.Contains(v.targets, f) {
				v.targets = append(v.targets, f)
			}
		}
	}
	return v
}

// bindValues adds the literal-value bindings of lits: each literal is a
// Candidate/variable-constant use of the functions it names.
func (s *Snapshot) bindValues(lits []varLit) {
	for _, l := range lits {
		df := s.declFile[l.d]
		vu, ok := s.valueUse[l.d]
		if !ok {
			f := s.facts[df].File
			if vr, ok := valueRange(l.d, f); ok {
				vu = &Use{Kind: UseCall, Via: ViaValue, Name: l.d.Literal, Range: vr, Scope: l.d.Scope, Node: l.d.Node,
					inexact: string(f.Src[vr.Start.Offset:vr.End.Offset]) != l.d.Literal}
			}
			s.valueUse[l.d] = vu
		}
		if vu == nil {
			continue
		}
		for _, f := range l.syms { // per (declaration, symbol): the same literal can reach different symbols from different closures
			if k := (valueKey{l.d, f}); !s.valueSeen[k] {
				s.valueSeen[k] = true
				s.extra = append(s.extra, Binding{Use: vu, File: s.paths[df], Sym: f, Res: Candidate, Reason: ReasonVariableConstant})
			}
		}
	}
}

// assigns reports whether the assignment node of d has `=` after the name.
func (s *Snapshot) assigns(d *Decl) bool {
	src := s.facts[s.declFile[d]].File.Src
	i := d.NameRange.End.Offset
	return i < len(src) && (src[i] == '=' || src[i] == '+' || src[i] == '[')
}

// plainLiteral reports whether d assigns exactly its literal value.
func (s *Snapshot) plainLiteral(d *Decl) bool {
	if d.Kind != DeclVariable || d.Implicit != "" || d.Literal == "" || d.Node == nil || d.Node.Kind != tree.KAssign {
		return false
	}
	if a := d.Attrs; a.Nameref || a.Integer || a.Array || a.Assoc {
		return false
	}
	src := s.facts[s.declFile[d]].File.Src
	i := d.NameRange.End.Offset
	return i < len(src) && src[i] == '='
}

// valueRange is the range of the literal value text of assignment d: the
// word, or the inside of a quoted word.
func valueRange(d *Decl, f *tree.File) (tree.Range, bool) {
	var w *tree.Node
	for _, c := range d.Node.Children {
		if c.Kind == tree.KWord {
			w = c
		}
	}
	if w == nil || len(w.Children) != 1 {
		return tree.Range{}, false
	}
	p := w.Children[0]
	switch p.Kind {
	case tree.KLit:
		return p.Range, true
	case tree.KSglQuoted:
		s, e := p.Range.Start.Offset, p.Range.End.Offset
		if e-s < 2 || s < 0 || e > len(f.Src) || f.Src[s] == '$' {
			return tree.Range{}, false
		}
		return tree.Range{Start: f.PosAt(s + 1), End: f.PosAt(e - 1)}, true
	case tree.KDblQuoted:
		if len(p.Children) == 1 && p.Children[0].Kind == tree.KLit {
			return p.Children[0].Range, true
		}
	}
	return tree.Range{}, false
}

// finishDynamic runs after bindAll: it adds the literal-value bindings of
// variable-constant calls, binds dynamic-scope reads and resolves invocations.
func (s *Snapshot) finishDynamic(r func(file int) *resolver) {
	added := map[int][]Binding{}
	replaced := map[*Use]bool{}
	for _, b := range s.extra {
		f := s.idx[b.File]
		added[f] = append(added[f], b)
	}
	s.dynamicScope(added, replaced)
	if len(added) > 0 {
		touched := map[*Symbol]bool{}
		for f, bs := range added {
			list := s.bindings[f]
			if len(replaced) > 0 {
				list = slices.DeleteFunc(slices.Clone(list), func(b Binding) bool {
					if replaced[b.Use] {
						touched[b.Sym] = true
						return true
					}
					return false
				})
			}
			list = append(list, bs...)
			slices.SortStableFunc(list, func(a, b Binding) int { return a.Use.Range.Start.Offset - b.Use.Range.Start.Offset })
			s.bindings[f] = list
			for _, b := range bs {
				touched[b.Sym] = true
			}
		}
		bySym := map[*Symbol][]Binding{}
		for _, bs := range added {
			for _, b := range bs {
				bySym[b.Sym] = append(bySym[b.Sym], b)
			}
		}
		for sym := range touched {
			list := slices.DeleteFunc(slices.Clone(s.bySym[sym]), func(b Binding) bool { return replaced[b.Use] })
			list = append(list, bySym[sym]...)
			slices.SortStableFunc(list, func(a, b Binding) int {
				if a.File != b.File {
					return strings.Compare(a.File, b.File)
				}
				return a.Use.Range.Start.Offset - b.Use.Range.Start.Offset
			})
			if len(list) == 0 {
				delete(s.bySym, sym)
			} else {
				s.bySym[sym] = list
			}
		}
	}
	s.extra, s.valueSeen, s.valueUse, s.varVals = nil, nil, nil, nil
	s.invokes = make([][]invokeSite, len(s.paths))
	for i, ff := range s.facts {
		if len(ff.Invokes) == 0 {
			continue
		}
		res := r(i)
		for k := range ff.Invokes {
			ref := &ff.Invokes[k]
			e := res.resolve(SourceRef{Node: ref.Node, Word: ref.Word, Range: ref.Range})
			if e.Status != SourceResolved {
				continue
			}
			for _, t := range e.Targets {
				s.invokes[i] = append(s.invokes[i], invokeSite{ref: ref, target: t})
			}
		}
	}
}

// dynamicScope rebinds a read inside function g that no declaration in view
// makes (unresolved/environment) to the `local` of that name in each caller
// of g, when some caller declares one: Candidate/dynamic-scope, instead of the
// environment binding. Callers are the functions around resolved RelCalls
// edges to the definition of g that holds the read (plain calls and calls
// through an alias; not strings, variables or alias definitions), depth 1.
//
// Cost: callers are kept once per (file, function), and for each g the
// caller locals of the names g reads unresolved are indexed once, looking at
// the smaller of those names and each caller's locals. A scan of every call
// of g per read cost O(reads x calls): 56 s for 1 MiB.
func (s *Snapshot) dynamicScope(added map[int][]Binding, replaced map[*Use]bool) {
	type callerLocal struct {
		file int
		fn   *tree.Node
	}
	// The unresolved reads inside a function, with that function.
	type read struct {
		file, k int
		g       *Decl
	}
	var reads []read
	names := map[*Decl]map[string]bool{} // per g, the names it reads unresolved
	for f, bs := range s.bindings {
		for k := range bs {
			b := &bs[k]
			u := b.Use
			if u.Kind != UseVarRead || b.Res != Unresolved || u.Scope == nil || u.Scope.Func == "" {
				continue
			}
			g := s.callerOfScope(f, u.Scope)
			if g == nil {
				continue
			}
			reads = append(reads, read{f, k, g})
			if names[g] == nil {
				names[g] = map[string]bool{}
			}
			names[g][u.Name] = true
		}
	}
	if len(reads) == 0 {
		return
	}
	// callersOf maps a definition to the distinct functions around the
	// resolved plain calls that reach it, in order of their first call.
	// Filled per symbol in one pass over its calls (each call's targets
	// listed once): per definition, the pass cost O(definitions^2) for one
	// call of a name defined many times.
	callersOf := map[*Decl][]callerLocal{}
	seenCaller := map[*Decl]map[callerLocal]bool{}
	swept := map[*Symbol]bool{}
	var scratch []edgeRec
	sweep := func(gs *Symbol) {
		for _, cb := range s.bySym[gs] {
			cu := cb.Use
			if cu.Kind != UseCall || cu.Via != ViaNone || cu.ViaAlias || cb.Res != Resolved {
				continue
			}
			cf := s.idx[cb.File]
			scratch, _ = s.appendTargets(scratch[:0], cf, edgeRec{}, &cb) // a capped list is all candidates: no Resolved record in it
			var d *Decl
			for k, r := range scratch {
				if r.res != Resolved || r.to == nil || s.declSym[r.to] != gs ||
					slices.ContainsFunc(scratch[:k], func(q edgeRec) bool { return q.res == Resolved && q.to == r.to }) {
					continue
				}
				if d == nil {
					if d = s.callerOfScope(cf, cu.Scope); d == nil {
						break
					}
				}
				c := callerLocal{cf, d.Node}
				if seenCaller[r.to] == nil {
					seenCaller[r.to] = map[callerLocal]bool{}
				}
				if !seenCaller[r.to][c] {
					seenCaller[r.to][c] = true
					callersOf[r.to] = append(callersOf[r.to], c)
				}
			}
		}
	}
	// localsOf[g][name] is the caller-local symbols of name, in caller order,
	// each once: what each read of name in g binds to.
	localsOf := map[*Decl]map[string][]*Symbol{}
	index := func(g *Decl) map[string][]*Symbol {
		if m, ok := localsOf[g]; ok {
			return m
		}
		m := map[string][]*Symbol{}
		want := names[g]
		// each symbol once per name, checked with a set: a list scan cost
		// O(callers^2) for a name local in many callers
		type nameSym struct {
			name string
			sym  *Symbol
		}
		have := map[nameSym]bool{}
		add := func(c callerLocal, name string) {
			if sym := s.localSyms[localKey{c.file, c.fn, name}]; sym != nil && !have[nameSym{name, sym}] {
				have[nameSym{name, sym}] = true
				m[name] = append(m[name], sym)
			}
		}
		for _, c := range callersOf[g] {
			ls := s.locals[c.file][c.fn]
			if len(ls) < len(want) {
				for name := range ls {
					if want[name] {
						add(c, name)
					}
				}
				continue
			}
			for name := range want {
				if ls[name] {
					add(c, name)
				}
			}
		}
		localsOf[g] = m
		return m
	}
	for _, rd := range reads {
		b := &s.bindings[rd.file][rd.k]
		u := b.Use
		if gs := s.declSym[rd.g]; !swept[gs] {
			swept[gs] = true
			sweep(gs)
		}
		syms := index(rd.g)[u.Name]
		if len(syms) == 0 {
			continue
		}
		replaced[u] = true
		if len(syms) > maxDynScopeLocals {
			s.lim.dynScopeCap(rd.file, u, len(syms))
			syms = syms[:maxDynScopeLocals]
		}
		for _, sym := range syms {
			added[rd.file] = append(added[rd.file], Binding{Use: u, File: b.File, Sym: sym, Res: Candidate, Reason: ReasonDynamicScope})
		}
	}
}

// appendDynamicEdges adds the dynamic edges and the invocations of file.
func (s *Snapshot) appendDynamicEdges(out []edgeRec, file int) []edgeRec {
	for k, d := range s.dyn[file] {
		out = append(out, edgeRec{at: int32(k), tgt: tgtDynamic, rel: RelCalls, res: Dynamic, reason: d.reason})
	}
	for k := range s.invokes[file] {
		out = append(out, edgeRec{at: int32(k), rel: RelInvokes, res: Resolved, reason: codeDirect})
	}
	return out
}

// appendViaTargets appends the edges of a string or variable call binding:
// one candidate edge per definition of the symbol in view of file, with the
// binding's reason.
//
// At most maxSiteTargets definitions are listed; capped reports that the
// symbol has more in view.
func (s *Snapshot) appendViaTargets(out []edgeRec, file int, r edgeRec, b *Binding) (_ []edgeRec, capped bool) {
	r.res, r.reason = Candidate, reasonCode(b.Reason)
	n := 0
	for _, d := range b.Sym.Decls {
		if s.visible[file].has(s.declFile[d]) {
			if n == maxSiteTargets {
				return out, true
			}
			n++
			r.to = d
			out = append(out, r)
		}
	}
	return out, false
}

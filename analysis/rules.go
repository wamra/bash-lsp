package analysis

import (
	"bytes"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wamra/bash-lsp/tree"
)

// rules is the native rule table (FDS-015). Severities are LSP
// DiagnosticSeverity values; ShellCheck lists the codes that report the same
// problem, so a ShellCheck finding with one of them replaces the native one
// (Merge) and `# shellcheck disable=` with one of them silences it.
var rules = []RuleInfo{
	{"WB1001", "syntax-error", SevError, nil, "a syntax error, or code in an error gap that was not analysed"},
	{"WB1002", "file-skipped", SevInfo, nil, "the file was not parsed (too large, or a dialect that is not supported)"},
	{"WB1003", "internal-error", SevInfo, nil, "the parser or lowerer failed on this region; it is incomplete"},
	{"WB1005", "resync-budget", SevInfo, nil, "error recovery used up its budget: the rest of the file is one error gap"},
	{"WB1007", "findings-cap", SevInfo, nil, "more findings than are shown for one file"},
	{"WB1008", "unknown-directive-rule", SevInfo, nil, "a `# bash-lsp disable=` directive names a rule id no rule has"},
	{"WB1101", "unresolved-source", SevWarning, []string{"SC1090", "SC1091"}, "a `source` whose file is not in the workspace or was skipped for its size"},
	{"WB1103", "duplicate-function", SevInfo, nil, "a function defined twice at the top level of one file"},
	{"WB1104", "builtin-name", SevInfo, nil, "a function named like a builtin replaces it"},
	{"WB1105", "local-outside-function", SevError, []string{"SC2168"}, "`local` outside a function fails"},
	{"WB1106", "lost-assignment", SevWarning, []string{"SC2030", "SC2031"}, "a variable set in a subshell, pipeline stage or substitution, read after it"},
	{"WB1201", "unused-function", SevHint, []string{"SC2329"}, "a function nothing calls, in a script nothing sources"},
	{"WB1202", "unused-variable", SevHint, []string{"SC2034"}, "a variable that is assigned but never read"},
	{"WB1301", "bash-version", SevWarning, nil, "a feature newer than the oldest bash version the target range names"},
	{"WB1302", "bash-only-in-sh", SevWarning, []string{"SC3*", "SC2113"}, "a bash-only feature in a file that is an sh script"},
	{"WB1401", "unquoted-file-argument", SevWarning, []string{"SC2086", "SC2068"}, "an unquoted expansion in an argument of a command that changes files"},
}

var ruleByID = func() map[string]RuleInfo {
	m := map[string]RuleInfo{}
	for _, r := range rules {
		m[r.ID] = r
	}
	return m
}()

// reservedRules are the ids of rules the plan names that are not built yet:
// settings and directives naming them are not mistakes.
var reservedRules = map[string]bool{"WB1004": true, "WB1006": true, "WB1102": true}

// KnownRule reports whether id names a native rule, built or reserved.
func KnownRule(id string) bool { _, ok := ruleByID[id]; return ok || reservedRules[id] }

// Rules lists the native rules.
func Rules() []RuleInfo {
	out := make([]RuleInfo, len(rules))
	for i, r := range rules {
		r.ShellCheck = append([]string(nil), r.ShellCheck...)
		out[i] = r
	}
	return out
}

// --- WB1001, WB1003, WB1005: syntax ---

// lineEnd returns the position of the end of the line holding p, before its
// newline (and a carriage return before it).
func lineEnd(f *tree.File, p tree.Pos) tree.Pos {
	src := f.Src
	o := min(max(p.Offset, 0), len(src))
	i := bytes.IndexByte(src[o:], '\n')
	if i < 0 {
		return f.PosAt(len(src))
	}
	e := o + i
	if e > o && src[e-1] == '\r' {
		e--
	}
	return f.PosAt(e)
}

// syntax reports the file's syntax errors (WB1001), recovered internal
// errors (WB1003), error gaps that hold no error (WB1001) and resync limits
// (WB1005). An error inside a gap is ranged from the gap's start to the end
// of the error's line: the code from there was not analysed.
func (c *diagCtx) syntax() {
	f := c.f
	var gaps []*tree.Node
	for _, n := range f.Root.Children {
		if n.Kind == tree.KErrorGap {
			gaps = append(gaps, n)
		}
	}
	covered := make([]bool, len(gaps))
	gapAt := func(off int) int {
		i := sort.Search(len(gaps), func(i int) bool { return gaps[i].Range.End.Offset > off })
		if i < len(gaps) && gaps[i].Range.Start.Offset <= off {
			return i
		}
		// an error at the very end of the file belongs to a gap that ends there
		if n := len(gaps); n > 0 && off == len(f.Src) && gaps[n-1].Range.End.Offset == off {
			return n - 1
		}
		return -1
	}
	for _, e := range f.Errors {
		rule := "WB1001"
		if e.Internal {
			rule = "WB1003"
		}
		r := e.Range
		if g := gapAt(e.Range.Start.Offset); g >= 0 {
			covered[g] = true
			r.Start = gaps[g].Range.Start
			if end := lineEnd(f, e.Range.Start); end.Offset > r.End.Offset {
				r.End = end
			}
		}
		msg := e.Msg
		if e.Internal { // the panic text is for the log (the server writes it), not the user
			msg = fmt.Sprintf("internal parser error; this file is analysed up to line %d", e.Range.Start.Line)
		}
		c.add(rule, r, msg)
	}
	for i, g := range gaps {
		if !covered[i] {
			c.add("WB1001", g.Range, "code not analysed (error gap)")
		}
	}
	for _, l := range f.Limits {
		r := tree.Range{Start: l.Range.Start, End: lineEnd(f, l.Range.Start)}
		c.add("WB1005", r, "resync budget used up: the rest of the file from this line is one error gap and is not analysed ("+l.Detail+")")
	}
}

// --- WB1008: unknown rule id in a directive ---

func (c *diagCtx) directiveIDs() {
	for _, cm := range c.f.Comments {
		d, ok := parseDirective(cm.Value)
		if !ok || d.wb == nil || !placed(c.f.Src, cm) {
			continue
		}
		var unknown []string
		for id := range d.wb {
			if !KnownRule(id) {
				unknown = append(unknown, id)
			}
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			c.add("WB1008", cm.Range, "unknown rule id in a bash-lsp directive: "+strings.Join(unknown, ", "))
		}
	}
}

// --- WB1101: unresolved source ---

func (c *diagCtx) sources() {
	src := c.f.Src
	for _, e := range c.s.edges[c.file] {
		if e.Status != SourceUnresolved || e.Ref.Word == nil || notAFile(e.Ref.Word) {
			continue
		}
		r := e.Ref.Word.Range
		text := ""
		if a, b := r.Start.Offset, r.End.Offset; a >= 0 && a <= b && b <= len(src) {
			text = string(src[a:b])
		}
		lit, literal := literalText(e.Ref.Word)
		named := ""
		if e.Ref.Directive != "" { // `# shellcheck source=PATH` names the file to follow
			lit, literal, named = e.Ref.Directive, true, " (named by a shellcheck source= directive)"
			text = lit
		}
		switch {
		case e.Skipped != "":
			d := c.add("WB1101", r, fmt.Sprintf("source file is over the %d MiB analysis limit and was skipped: %s (unresolved/%s)",
				MaxFileBytes>>20, c.s.rel(e.Skipped), ReasonSkippedForSize))
			d.codes = []string{"SC1091"}
		case literal:
			d := c.add("WB1101", r, fmt.Sprintf("source file not found in the workspace: %s%s (unresolved/%s)", text, named, ReasonNotFound))
			d.codes = []string{"SC1091"} // ShellCheck's code for a file it cannot follow
			if c.outsideWorkspace(lit, e.Ref) || named == "" && guardedSource(e.Ref, lit, src) {
				d.Severity = SevInfo
			}
		default:
			d := c.add("WB1101", r, fmt.Sprintf("source path is computed and could not be resolved: %s (unresolved/%s)", text, ReasonNotFound))
			d.codes = []string{"SC1090"} // ShellCheck's code for a path it cannot compute
			d.Severity = SevInfo
		}
	}
}

// notAFile reports a `source` operand that names no file to resolve: a
// device or /proc path (`source /dev/stdin`, `/dev/null`) or a process
// substitution (`source <(cmd)`).
func notAFile(w *tree.Node) bool {
	if len(w.Children) > 0 && w.Children[0].Kind == tree.KProcSubst {
		return true
	}
	lit, ok := literalText(w)
	return ok && (strings.HasPrefix(lit, "/dev/") || strings.HasPrefix(lit, "/proc/"))
}

// outsideWorkspace reports a literal path that cannot be a workspace file:
// absolute outside every workspace folder, under the home directory (`~/`),
// or a bare name that bash looks up in PATH (not for bats `load`, which
// looks beside the test file only).
func (c *diagCtx) outsideWorkspace(lit string, ref SourceRef) bool {
	switch {
	case strings.HasPrefix(lit, "~"):
		return true
	case !strings.Contains(lit, "/"):
		return !c.batsLoad(ref)
	case filepath.IsAbs(lit):
		return !c.inWorkspace(lit)
	}
	return false
}

// batsLoad reports a bats `load` command.
func (c *diagCtx) batsLoad(ref SourceRef) bool {
	return (&resolver{ff: c.ff}).batsLoad(ref)
}

// fileTestOps are the test operators that check a file exists or is readable.
var fileTestOps = map[string]bool{"-f": true, "-r": true, "-e": true, "-s": true}

// guardedSource reports a `source` of lit behind a test on the same path:
// `[ -f P ] && . P`, `if [[ -r P ]]; then . P; fi`, `test -e P && source P`.
func guardedSource(ref SourceRef, lit string, src []byte) bool {
	child := ref.Node
	for n, depth := ref.Node.Parent, 0; n != nil && depth < 8; n, depth = n.Parent, depth+1 {
		switch n.Kind {
		case tree.KFuncDecl, tree.KFile:
			return false
		case tree.KAndOr:
			// `T && . P`: the operator just before the source's operand is
			// `&&` and the test before it is not negated.
			for k, op := range n.Children {
				if op != child || k == 0 {
					continue
				}
				prev := n.Children[k-1]
				a, b := prev.Range.End.Offset, child.Range.Start.Offset
				if a < 0 || b > len(src) || a > b || !strings.Contains(string(src[a:b]), "&&") {
					return false
				}
				return testsPath(prev, lit, src)
			}
		case tree.KIf:
			// the `then` part of `if T` (not an else or elif branch)
			if len(n.Children) > 1 && n.Children[1] == child && testsPath(n.Children[0], lit, src) {
				return true
			}
			return false
		}
		child = n
	}
	return false
}

// testsPath reports whether n holds a test (`[[ ]]`, `[ ]`, `test`) with a
// file operator whose operand is lit, not negated.
func testsPath(n *tree.Node, lit string, src []byte) bool {
	if a, b := n.Range.Start.Offset, n.Range.End.Offset; a >= 0 && a <= b && b <= len(src) {
		for _, f := range strings.Fields(string(src[a:b])) {
			if f == "!" { // a negated test guards the other case
				return false
			}
		}
	}
	found := false
	n.Walk(func(k *tree.Node) bool {
		if found {
			return false
		}
		if k.Kind != tree.KTest && k.Kind != tree.KCall {
			return true
		}
		a, b := k.Range.Start.Offset, k.Range.End.Offset
		if a < 0 || b > len(src) || a >= b {
			return false
		}
		fields := strings.Fields(string(src[a:b]))
		if k.Kind == tree.KCall && (len(fields) == 0 || fields[0] != "[" && fields[0] != "test") {
			return true
		}
		op := false
		for _, f := range fields {
			op = op || fileTestOps[f]
		}
		if !op {
			return false
		}
		for _, w := range k.Children {
			if t, ok := literalText(w); ok && t == lit {
				found = true
			}
		}
		return false
	})
	return found
}

// --- WB1103, WB1104: function definitions ---

func (c *diagCtx) functions() {
	first := make(map[string]*tree.Node, len(c.scan.topFuncs))
	for _, fn := range c.scan.topFuncs {
		if p, ok := first[fn.Name]; ok {
			c.add("WB1103", fn.NameRange, fmt.Sprintf("function %s is already defined at line %d; this definition replaces it", fn.Name, p.NameRange.Start.Line))
			continue
		}
		first[fn.Name] = fn
	}
	for _, fn := range c.scan.funcs {
		if IsBuiltin(fn.Name) && !c.scan.wrappers[fn] && !interactiveBuiltins[fn.Name] {
			c.add("WB1104", fn.NameRange, fmt.Sprintf("function %s has the name of a builtin and replaces it", fn.Name))
		}
	}
}

// interactiveBuiltins are builtins a script does not use: a script that
// defines `help` (a usage message, the common case) or `history` loses
// nothing.
var interactiveBuiltins = map[string]bool{"help": true, "history": true, "fc": true, "bind": true, "logout": true, "suspend": true}

// --- WB1105: local outside a function ---

// localOutside reports `local` at file level in a script: a file with a
// shebang that no file sources (a sourced file's top level may run inside a
// function of the file that sources it).
func (c *diagCtx) localOutside() {
	if !c.scan.shebang || c.s.sourced[c.file] {
		return
	}
	src := c.f.Src
	for _, n := range c.scan.locals {
		a := n.Range.Start.Offset
		if a < 0 || a+5 > len(src) || string(src[a:a+5]) != "local" {
			continue
		}
		c.add("WB1105", tree.Range{Start: n.Range.Start, End: c.f.PosAt(a + 5)}, "`local` outside a function: bash refuses it")
	}
}

// --- WB1106: assignment lost in a subshell ---

// shellVariables are variables the shell itself reads or sets (the WB1202
// list), and envVariables ones a script usually gets from its environment.
var shellVariables = map[string]bool{"IFS": true, "PS1": true, "PS2": true, "PS3": true, "PS4": true, "PATH": true,
	"CDPATH": true, "HISTFILE": true, "HISTSIZE": true, "HISTCONTROL": true, "PROMPT_COMMAND": true, "TMOUT": true,
	"OPTIND": true, "OPTERR": true, "TIMEFORMAT": true, "GLOBIGNORE": true, "BASH_ENV": true, "LANG": true}

var envVariables = map[string]bool{"HOME": true, "USER": true, "LOGNAME": true, "PWD": true, "OLDPWD": true,
	"SHELL": true, "TERM": true, "TMPDIR": true, "EDITOR": true, "PAGER": true}

func shellVariable(name string) bool { return shellVariables[name] || strings.HasPrefix(name, "LC_") }

var privateKind = map[ScopeKind]string{ScopeSubshell: "a subshell", ScopeCmdSubst: "a command substitution",
	ScopePipeline: "a pipeline stage", ScopeProcSubst: "a process substitution", ScopeCoproc: "a coproc"}

// lostAssignments reports a read, after a private scope (subshell, pipeline
// stage, substitution, coproc) that assigns its name, that binds to no
// declaration at all: the assignment did not survive the scope. Only reads
// in the same function (or at file level) as the scope, outside the shell's
// own and the usual environment variables, in a file whose graph is
// complete. With `shopt -s lastpipe` in the file a pipeline stage may run in
// the shell itself, so pipeline stages are not counted. The first such read
// per name and function is reported.
func (c *diagCtx) lostAssignments() {
	s := c.s
	ents := s.privByFile[c.file]
	if len(ents) == 0 || s.incomplete[c.file] {
		return
	}
	lastpipe := c.scan.lastpipe
	type key struct {
		name string
		fn   *tree.Node
	}
	type lost struct {
		end      int
		sc       *Scope
		reported bool
	}
	first := map[key]*lost{}
	for _, e := range ents {
		if e.sc.Node == nil || lastpipe && e.sc.Kind == ScopePipeline {
			continue
		}
		k := key{e.sym.Name, funcBody(e.sc)}
		if l := first[k]; l == nil || e.sc.Node.Range.End.Offset < l.end {
			first[k] = &lost{end: e.sc.Node.Range.End.Offset, sc: e.sc}
		}
	}
	bs := s.bindings[c.file]
	for i := 0; i < len(bs); {
		j := i + 1
		for j < len(bs) && bs[j].Use == bs[i].Use {
			j++
		}
		u := bs[i].Use
		env := u.Kind == UseVarRead
		for _, b := range bs[i:j] {
			env = env && b.Res == Unresolved && b.Reason == ReasonEnvironment
		}
		i = j
		if !env || shellVariable(u.Name) || envVariables[u.Name] {
			continue
		}
		l := first[key{u.Name, funcBody(u.Scope)}]
		if l == nil || l.reported || u.Range.Start.Offset < l.end {
			continue
		}
		l.reported = true
		r := u.Range
		if u.Node != nil && u.Node.Kind == tree.KParamExp {
			r = u.Node.Range
		}
		c.add("WB1106", r, fmt.Sprintf("%s was set in %s at line %d; that assignment is lost after it and does not reach this read",
			u.Name, privateKind[l.sc.Kind], l.sc.Node.Range.Start.Line))
	}
}

// --- WB1201, WB1202: unused ---

// scriptConditions are the file conditions for an unused function and an
// unused global: a script (a shebang and a top-level command, no top-level
// `return` or dual-use guard) that nothing sources, not bats, whose graph is
// complete, where no visible file has a dynamic call or code that was not
// analysed, and no file of the workspace loads scripts through a loop
// variable it could not resolve.
func (c *diagCtx) scriptConditions() bool {
	sc := c.scan
	if !sc.shebang || !sc.topCommand || c.s.sourced[c.file] || c.s.incomplete[c.file] || c.f.Dialect == tree.Bats || c.loopLoaded() {
		return false
	}
	u := c.visible()
	return !u.dynamic && !u.gap
}

// hookNames are functions bash itself calls.
var hookNames = map[string]bool{"command_not_found_handle": true}

// unusedFunctions reports a function with no incoming binding of any
// resolution (call, alias, string, variable value) whose name no visible
// file mentions as a literal word or inside a string (`retry 3 f`,
// `cb=f`, `export -f f`, `complete -F f`, `su -c 'f'`).
func (c *diagCtx) unusedFunctions() {
	s := c.s
	ok := -1
	for _, d := range s.funcDecls[c.file] {
		sym := s.declSym[d]
		if sym == nil || len(s.bySym[sym]) > 0 || hookNames[d.Name] {
			continue
		}
		if ok < 0 {
			ok = 0
			if c.scriptConditions() {
				ok = 1
			}
		}
		if ok == 0 {
			return
		}
		if c.visibleWords()[d.Name] {
			continue
		}
		dg := c.add("WB1201", d.NameRange, fmt.Sprintf("function %s is never called", d.Name))
		dg.Unnecessary = true
	}
}

// unusedVariables reports a variable that is assigned and never read: a
// local or a subshell's variable (in a library only when its function calls
// nothing that resolves to nothing in view), a global only under
// scriptConditions, when not exported and not a value returned to a
// caller's local; never an accumulator (`+=`). Never `_`, a variable the shell
// itself uses, an upper-case name (an environment variable by convention),
// a nameref, or one set implicitly (`read`, `for`, `getopts`, ...). A name
// the visibility may read by name (readByName: dynamic scope, `"$name"`, a
// mention inside a string) counts as read; so does every name when a
// visible file uses `${!x}`, a nameref or a dynamic call.
func (c *diagCtx) unusedVariables() {
	s := c.s
	var seen map[*Symbol]bool // symbols with several declarations, looked at already
	var pending []unusedCand
	script := -1
	for _, d := range c.ff.Decls {
		if d.Kind != DeclVariable || d.Temp || d.Implicit != "" || d.Name == "_" || shellVariable(d.Name) || upperName(d.Name) {
			continue
		}
		sym := s.declSym[d]
		if sym == nil {
			continue
		}
		if len(sym.Decls) > 1 {
			if seen[sym] {
				continue
			}
			if seen == nil {
				seen = map[*Symbol]bool{}
			}
			seen[sym] = true
		}
		if symFlags(sym, func(a VarAttrs) bool { return a.Exported || a.Nameref }) || readIn(s.bySym[sym]) || c.appended(sym) {
			continue
		}
		global := sym.File == ""
		if global && c.returnValue(sym) {
			continue
		}
		if !global && !c.scriptShaped() && c.unresolvedCallIn(d) {
			continue
		}
		if global {
			if script < 0 {
				script = 0
				if c.scriptConditions() {
					script = 1
				}
			}
			if script == 0 {
				continue
			}
		}
		u := c.visible()
		if u.indirect || u.nameref || u.dynamic {
			continue
		}
		pending = append(pending, unusedCand{d, global})
	}
	if len(pending) == 0 {
		return
	}
	// Names read by name are looked up only for the candidates left.
	c.wantReads = make(map[string]bool, len(pending))
	for _, p := range pending {
		c.wantReads[p.d.Name] = true
	}
	for _, p := range pending {
		d := p.d
		if c.readByName(d.Name) || p.global && c.allExport() {
			continue
		}
		what := "variable"
		if d.Attrs.Local {
			what = "local"
		}
		dg := c.add("WB1202", d.NameRange, fmt.Sprintf("%s %s is assigned but never read", what, d.Name))
		dg.Unnecessary = true
	}
}

type unusedCand struct {
	d      *Decl
	global bool
}

// appended reports whether some declaration of sym appends (`xs+=(1)`,
// `s+=x`): an accumulator, which ShellCheck 0.11.0 does not report either.
func (c *diagCtx) appended(sym *Symbol) bool {
	for _, d := range sym.Decls {
		src := c.s.src(d)
		i := d.NameRange.End.Offset
		if i >= 0 && i < len(src) && src[i] == '[' {
			if j := bytes.IndexByte(src[i:], ']'); j > 0 {
				i += j + 1
			}
		}
		if i >= 0 && i+1 < len(src) && src[i] == '+' && src[i+1] == '=' {
			return true
		}
	}
	return false
}

// returnValue reports whether global sym is assigned inside a function and
// some function in view declares a local of its name: the assignment may
// write that caller's local through dynamic scope (a value returned in a
// variable, `set_it() { result=5; }`).
func (c *diagCtx) returnValue(sym *Symbol) bool {
	inFunc := false
	for _, d := range sym.Decls {
		if funcBody(d.Scope) != nil {
			inFunc = true
			break
		}
	}
	if !inFunc {
		return false
	}
	found := false
	c.s.visible[c.file].each(func(j int) {
		if !found && c.s.facts[j] != nil {
			found = c.s.scanOf(j).localNames[sym.Name]
		}
	})
	return found
}

// scriptShaped reports a file with a shebang and a top-level command (and
// no top-level return or dual-use guard): a script, not a library.
func (c *diagCtx) scriptShaped() bool { return c.scan.shebang && c.scan.topCommand }

// unresolvedCallIn reports whether the function holding local (or private)
// variable d calls a name with no resolved target in view (none, or only a
// name match in a file outside the closure) that some file of the workspace
// defines as a function: in a library
// that callee may be a function of a file that sources it, reading d
// through dynamic scope.
func (c *diagCtx) unresolvedCallIn(d *Decl) bool {
	fn := funcBody(d.Scope)
	if fn == nil {
		return true
	}
	if c.unresolvedFns == nil {
		c.unresolvedFns = map[*tree.Node]bool{}
		bound := map[*Use]bool{} // calls with a resolved target; a name match outside the closure may be the reader
		for k := range c.s.bindings[c.file] {
			if b := &c.s.bindings[c.file][k]; b.Res == Resolved {
				bound[b.Use] = true
			}
		}
		for _, u := range c.ff.Uses {
			// R73: a name no file of the workspace defines is an external
			// command, which cannot read the local.
			if u.Kind == UseCall && u.Via == ViaNone && !u.ViaAlias && !bound[u] && !IsBuiltin(u.Name) && len(c.s.funcSyms[u.Name]) > 0 {
				if f := funcBody(u.Scope); f != nil {
					c.unresolvedFns[f] = true
				}
			}
		}
	}
	return c.unresolvedFns[fn]
}

// upperName reports a name with no lower-case letter: by convention an
// environment or shell variable (TZ, TMPDIR, BASH_XTRACEFD), which the shell
// or a child process may read when it came in exported.
func upperName(name string) bool {
	for i := 0; i < len(name); i++ {
		if c := name[i]; 'a' <= c && c <= 'z' {
			return false
		}
	}
	return true
}

func symFlags(sym *Symbol, f func(VarAttrs) bool) bool {
	for _, d := range sym.Decls {
		if f(d.Attrs) {
			return true
		}
	}
	return false
}

func readIn(bs []Binding) bool {
	for _, b := range bs {
		if b.Use.Kind == UseVarRead {
			return true
		}
	}
	return false
}

// --- WB1401: unquoted expansion in an argument of a file-changing command ---

func (c *diagCtx) quoting() {
	src := c.f.Src
	bs := c.s.bindings[c.file]
	for _, p := range c.scan.quotes {
		a, b := p.Range.Start.Offset, p.Range.End.Offset
		text := string(src[a:b])
		if (text == "$"+p.Name || text == "${"+p.Name+"}") && c.oneWordAt(bs, p) {
			continue
		}
		d := c.add("WB1401", p.Range, fmt.Sprintf("%s is not quoted: its value is split into words and expanded as a glob; write \"%s\"", text, text))
		// Never safe: a script may split on purpose (`set -f; IFS=,; rm $list`).
		d.Fixes = []Fix{{
			Title: fmt.Sprintf("Quote %s", text),
			Edits: []TextEdit{{Range: p.Range, NewText: "\"" + text + "\""}},
		}}
	}
}

// oneWordAt reports whether the read of p binds only to variables whose
// every assignment in view is provably one word without glob characters
// (oneWord): then the unquoted expansion cannot split or glob.
func (c *diagCtx) oneWordAt(bs []Binding, p *tree.Node) bool {
	off := p.NameRange.Start.Offset
	i := sort.Search(len(bs), func(i int) bool { return bs[i].Use.Range.Start.Offset >= off })
	found := false
	for ; i < len(bs) && bs[i].Use.Range.Start.Offset == off; i++ {
		b := bs[i]
		if b.Use.Node != p {
			continue
		}
		if b.Sym == nil || b.Res == Unresolved || !c.oneWord(b.Sym) || !c.setBefore(b.Sym, p) {
			return false
		}
		if b.Sym.File == "" && funcBody(b.Use.Scope) != nil && c.localInView(p.Name) {
			return false // a caller's local of that name may be what the read sees (dynamic scope)
		}
		found = true
	}
	return found && !c.visibleFlag(func(sc *fileScan) bool { return sc.setsIFS || sc.unsetVars[p.Name] })
}

// visibleFlag reports whether f holds for the scan of some visible file.
func (c *diagCtx) visibleFlag(f func(*fileScan) bool) bool {
	found := false
	c.s.visible[c.file].each(func(j int) {
		if !found && c.s.facts[j] != nil {
			found = f(c.s.scanOf(j))
		}
	})
	return found
}

// localInView reports whether some function in view declares name local.
func (c *diagCtx) localInView(name string) bool {
	return c.visibleFlag(func(sc *fileScan) bool { return sc.localNames[name] })
}

// setBefore reports whether sym is surely set where p reads it: some
// assignment in view runs unconditionally (no if, &&, ||, case or loop
// around it in its function or at file level) and comes before the read,
// is in another file, or is at file level while the read is in a function.
// Otherwise the value may come from the environment.
func (c *diagCtx) setBefore(sym *Symbol, p *tree.Node) bool {
	s := c.s
	inFunc := funcBody(scopeAt(c, p)) != nil
	for _, d := range sym.Decls {
		if d.Temp || !s.visible[c.file].has(s.declFile[d]) || d.Node == nil || !unconditional(d.Node) {
			continue
		}
		if s.declFile[d] != c.file || d.NameRange.Start.Offset < p.Range.Start.Offset || inFunc && funcBody(d.Scope) == nil {
			return true
		}
	}
	return false
}

// scopeAt returns the scope of the read of p (the use Bind recorded).
func scopeAt(c *diagCtx, p *tree.Node) *Scope {
	bs := c.s.bindings[c.file]
	off := p.NameRange.Start.Offset
	i := sort.Search(len(bs), func(i int) bool { return bs[i].Use.Range.Start.Offset >= off })
	for ; i < len(bs) && bs[i].Use.Range.Start.Offset == off; i++ {
		if bs[i].Use.Node == p {
			return bs[i].Use.Scope
		}
	}
	return nil
}

// conditionals are the nodes whose parts may not run.
var conditionals = map[tree.Kind]bool{tree.KIf: true, tree.KAndOr: true, tree.KCase: true, tree.KWhile: true,
	tree.KCStyleFor: true, tree.KSubshell: true, tree.KPipeline: true}

// unconditional reports whether n runs whenever its function (or the file)
// does: no conditional construct around it below the function. A `for` is
// the declaration of its own variable, not a condition on it.
func unconditional(n *tree.Node) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		switch {
		case p.Kind == tree.KFuncDecl || p.Kind == tree.KFile:
			return true
		case conditionals[p.Kind], p.Kind == tree.KFor && p != n:
			return false
		}
	}
	return true
}

// oneWord reports whether every assignment of sym in view is one word
// without whitespace or glob characters: a literal, an arithmetic
// expansion, an arithmetic write, a `for` over such literals, or any value
// of an integer variable. Cached per symbol.
func (c *diagCtx) oneWord(sym *Symbol) bool {
	if v, ok := c.oneWords[sym]; ok {
		return v
	}
	ok := c.oneWordUncached(sym)
	if c.oneWords == nil {
		c.oneWords = map[*Symbol]bool{}
	}
	c.oneWords[sym] = ok
	return ok
}

func (c *diagCtx) oneWordUncached(sym *Symbol) bool {
	s := c.s
	if symFlags(sym, func(a VarAttrs) bool { return a.Integer }) {
		return true
	}
	assigned := false
	for _, d := range sym.Decls {
		if d.Temp || !s.visible[c.file].has(s.declFile[d]) {
			continue
		}
		switch {
		case d.Implicit == "arith":
		case d.Implicit == "for":
			if !forOverWords(d.Node) {
				return false
			}
		case d.Implicit == "" && d.Node != nil && d.Node.Kind == tree.KAssign:
			if !s.assigns(d) {
				continue // `local x`: declares, assigns nothing
			}
			if !s.plainAssign(d) || !oneWordValue(d.Node, s.src(d)) {
				return false
			}
		default:
			return false
		}
		assigned = true
	}
	return assigned
}

// plainAssign reports `name=value` (no `+=`, subscript or array).
func (s *Snapshot) plainAssign(d *Decl) bool {
	if a := d.Attrs; a.Nameref || a.Array || a.Assoc {
		return false
	}
	src := s.src(d)
	i := d.NameRange.End.Offset
	return i+1 <= len(src) && i < len(src) && src[i] == '=' && (i+1 == len(src) || src[i+1] != '(')
}

// oneWordValue reports whether the value of assignment n is a non-empty
// literal without whitespace or glob characters, or one arithmetic
// expansion.
func oneWordValue(n *tree.Node, src []byte) bool {
	var w *tree.Node
	for _, k := range n.Children {
		if k.Kind == tree.KWord {
			w = k
		}
	}
	if w == nil {
		return false
	}
	if len(w.Children) == 1 && w.Children[0].Kind == tree.KArithExp {
		return true
	}
	for _, p := range w.Children {
		// `$'a\tb'` is stored undecoded: its value may hold any byte
		if a := p.Range.Start.Offset; p.Kind == tree.KSglQuoted && a >= 0 && a+1 < len(src) && src[a] == '$' {
			return false
		}
	}
	t, ok := literalText(w)
	return ok && safeWord(t) && !strings.Contains(t, "\\")
}

// forOverWords reports a `for` loop over literal words, each safeWord.
func forOverWords(n *tree.Node) bool {
	if n == nil || n.Kind != tree.KFor {
		return false
	}
	words := 0
	for _, k := range n.Children {
		if k.Kind != tree.KWord {
			continue
		}
		t, ok := literalText(k)
		if !ok || !safeWord(t) {
			return false
		}
		words++
	}
	return words > 0
}

func safeWord(t string) bool { return t != "" && !strings.ContainsAny(t, " \t\n*?[") }

// integerAt reports whether the read of p binds to a variable with the
// integer attribute.
func (c *diagCtx) integerAt(bs []Binding, p *tree.Node) bool {
	off := p.NameRange.Start.Offset
	i := sort.Search(len(bs), func(i int) bool { return bs[i].Use.Range.Start.Offset >= off })
	for ; i < len(bs) && bs[i].Use.Range.Start.Offset == off; i++ {
		if b := bs[i]; b.Use.Node == p && b.Sym != nil && symFlags(b.Sym, func(a VarAttrs) bool { return a.Integer }) {
			return true
		}
	}
	return false
}

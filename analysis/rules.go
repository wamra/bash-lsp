package analysis

import (
	"bytes"
	"fmt"
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
	{"WB1101", "unresolved-source", SevWarning, nil, "a `source` whose file is not in the workspace or was skipped for its size"},
	{"WB1103", "duplicate-function", SevInfo, nil, "a function defined twice at the top level of one file"},
	{"WB1104", "builtin-name", SevInfo, nil, "a function named like a builtin replaces it"},
	{"WB1105", "local-outside-function", SevError, []string{"SC2168"}, "`local` outside a function fails"},
	{"WB1106", "lost-assignment", SevWarning, []string{"SC2030", "SC2031"}, "a variable set in a subshell, pipeline stage or substitution, read after it"},
	{"WB1201", "unused-function", SevHint, []string{"SC2329"}, "a function nothing calls, in a script nothing sources"},
	{"WB1202", "unused-variable", SevHint, []string{"SC2034"}, "a variable that is assigned but never read"},
	{"WB1401", "unquoted-file-argument", SevWarning, []string{"SC2086"}, "an unquoted expansion in an argument of a command that changes files"},
}

var ruleByID = func() map[string]RuleInfo {
	m := map[string]RuleInfo{}
	for _, r := range rules {
		m[r.ID] = r
	}
	return m
}()

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
		c.add(rule, r, e.Msg)
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

// --- WB1101: unresolved source ---

func (c *diagCtx) sources() {
	src := c.f.Src
	for _, e := range c.s.edges[c.file] {
		if e.Status != SourceUnresolved || e.Ref.Word == nil {
			continue
		}
		r := e.Ref.Word.Range
		text := ""
		if a, b := r.Start.Offset, r.End.Offset; a >= 0 && a <= b && b <= len(src) {
			text = string(src[a:b])
		}
		_, literal := literalText(e.Ref.Word)
		switch {
		case e.Skipped != "":
			c.add("WB1101", r, fmt.Sprintf("source file is over the %d MiB analysis limit and was skipped: %s (unresolved/%s)",
				MaxFileBytes>>20, c.s.rel(e.Skipped), ReasonSkippedForSize))
		case literal:
			c.add("WB1101", r, fmt.Sprintf("source file not found in the workspace: %s (unresolved/%s)", text, ReasonNotFound))
		default:
			d := c.add("WB1101", r, fmt.Sprintf("source path is computed and could not be resolved: %s (unresolved/%s)", text, ReasonNotFound))
			d.Severity = SevInfo
		}
	}
}

// --- WB1103, WB1104: function definitions ---

func (c *diagCtx) functions() {
	first := map[string]*tree.Node{}
	for _, fn := range c.scan.topFuncs {
		if p, ok := first[fn.Name]; ok {
			c.add("WB1103", fn.NameRange, fmt.Sprintf("function %s is already defined at line %d; this definition replaces it", fn.Name, p.NameRange.Start.Line))
			continue
		}
		first[fn.Name] = fn
	}
	for _, fn := range c.scan.funcs {
		if IsBuiltin(fn.Name) && !c.scan.wrappers[fn] {
			c.add("WB1104", fn.NameRange, fmt.Sprintf("function %s has the name of a builtin and replaces it", fn.Name))
		}
	}
}

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
// complete. The first such read per name and function is reported.
func (c *diagCtx) lostAssignments() {
	s := c.s
	ents := s.privByFile[c.file]
	if len(ents) == 0 || s.incomplete[c.file] {
		return
	}
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
		if e.sc.Node == nil {
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
		c.add("WB1106", r, fmt.Sprintf("%s was set in %s at line %d; that assignment is lost after it, and this read sees no value",
			u.Name, privateKind[l.sc.Kind], l.sc.Node.Range.Start.Line))
	}
}

// --- WB1201, WB1202: unused ---

// scriptConditions are the file conditions for an unused function and an
// unused global: a script (a shebang and a top-level command) that nothing
// sources, not bats, whose graph is complete, and where no visible file has
// a dynamic call or code that was not analysed.
func (c *diagCtx) scriptConditions() bool {
	sc := c.scan
	if !sc.shebang || !sc.topCommand || c.s.sourced[c.file] || c.s.incomplete[c.file] || c.f.Dialect == tree.Bats {
		return false
	}
	u := c.visibleNames()
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
		if c.visibleNames().words[d.Name] {
			continue
		}
		dg := c.add("WB1201", d.NameRange, fmt.Sprintf("function %s is never called", d.Name))
		dg.Unnecessary = true
	}
}

// unusedVariables reports a variable that is assigned and never read: a
// local or a subshell's variable always, a global only under
// scriptConditions and when not exported. Never `_`, a variable the shell
// itself uses, a nameref, or one set implicitly (`read`, `for`, `getopts`,
// ...). A name that any visible file reads unbound (dynamic scope), calls
// as `"$name"`, or mentions as `$name` inside a string counts as read; so
// does every name when a visible file uses `${!x}` or a nameref, and a local
// when a dynamic call sits in its function.
func (c *diagCtx) unusedVariables() {
	s := c.s
	var seen map[*Symbol]bool // symbols with several declarations, looked at already
	script := -1
	for _, d := range c.ff.Decls {
		if d.Kind != DeclVariable || d.Temp || d.Implicit != "" || d.Name == "_" || shellVariable(d.Name) {
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
		if symFlags(sym, func(a VarAttrs) bool { return a.Exported || a.Nameref }) || readIn(s.bySym[sym]) {
			continue
		}
		global := sym.File == ""
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
		u := c.visibleNames()
		if u.indirect || u.nameref || u.reads[d.Name] || global && u.export {
			continue
		}
		if !global && c.dynamicIn(d) {
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

// dynamicIn reports whether a dynamic call site lies in the scope that
// holds local or private variable d: the called code may read it.
func (c *diagCtx) dynamicIn(d *Decl) bool {
	sites := c.dynSites()
	if len(sites) == 0 {
		return false
	}
	var n *tree.Node
	if ps := privateScope(d.Scope); ps != nil {
		n = ps.Node
	} else {
		n = funcBody(d.Scope)
	}
	if n == nil {
		return true
	}
	i := sort.SearchInts(sites, n.Range.Start.Offset)
	return i < len(sites) && sites[i] < n.Range.End.Offset
}

// dynSites returns the start offsets of the file's dynamic call sites, sorted.
func (c *diagCtx) dynSites() []int {
	if c.sites == nil {
		c.sites = make([]int, 0, len(c.s.dyn[c.file]))
		for _, site := range c.s.dyn[c.file] {
			c.sites = append(c.sites, site.rng.Start.Offset)
		}
		sort.Ints(c.sites)
	}
	return c.sites
}

// --- WB1401: unquoted expansion in an argument of a file-changing command ---

func (c *diagCtx) quoting() {
	src := c.f.Src
	bs := c.s.bindings[c.file]
	for _, p := range c.scan.quotes {
		if c.integerAt(bs, p) {
			continue
		}
		a, b := p.Range.Start.Offset, p.Range.End.Offset
		text := string(src[a:b])
		d := c.add("WB1401", p.Range, fmt.Sprintf("%s is not quoted: its value is split into words and expanded as a glob; write \"%s\"", text, text))
		d.Fixes = []Fix{{
			Title: fmt.Sprintf("Quote %s", text),
			Edits: []TextEdit{{Range: p.Range, NewText: "\"" + text + "\""}},
			Safe:  !strings.ContainsAny(text, "\"'`\\"),
		}}
	}
}

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

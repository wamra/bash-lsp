package analysis

import (
	"bytes"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/wamra/bash-lsp/tree"
)

const (
	maxCallSites    = 5
	maxGlobalsShown = 8
	maxDeclText     = 120
)

// rel shows p relative to the workspace root, with forward slashes.
func (s *Snapshot) rel(p string) string {
	if s.root != "" {
		if r, err := filepath.Rel(s.root, p); err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			return filepath.ToSlash(r)
		}
	}
	return filepath.ToSlash(p)
}

// spot is "path:line" for a declaration's name.
func (s *Snapshot) spot(d *Decl) string {
	return fmt.Sprintf("`%s:%d`", s.rel(s.paths[s.declFile[d]]), d.NameRange.Start.Line)
}

func (s *Snapshot) src(d *Decl) []byte { return s.facts[s.declFile[d]].File.Src }

// Hover describes the symbol under the cursor in Markdown. rng is the range
// of the hovered token (the whole definition when the cursor is on a function's name).
// On the path word of a `source` that could not be resolved it says why, in
// one line; a resolved `source` has no hover.
func (s *Snapshot) Hover(path string, offset int) (markdown string, rng tree.Range, ok bool) {
	h, file, found := s.at(path, offset)
	if !found {
		return "", tree.Range{}, false
	}
	if e := h.edge; e != nil {
		if e.Status != SourceUnresolved || e.Ref.Word == nil {
			return "", tree.Range{}, false
		}
		if e.Skipped != "" {
			return fmt.Sprintf("`source` not resolved (unresolved/%s): %s", ReasonSkippedForSize, s.rel(e.Skipped)), e.Ref.Word.Range, true
		}
		return fmt.Sprintf("`source` not resolved (unresolved/%s)", ReasonNotFound), e.Ref.Word.Range, true
	}
	var sym *Symbol
	switch {
	case h.decl != nil:
		sym = s.declSym[h.decl]
		rng = h.decl.NameRange
		if h.decl.Kind == DeclFunction {
			rng = h.decl.Node.Range
		}
	case len(h.binds) > 0 && h.binds[0].Reason == ReasonDynamicScope:
		return s.hoverDynamicScope(h.binds), h.binds[0].Use.Range, true
	case len(h.binds) > 0:
		sym = h.binds[0].Sym
		rng = h.binds[0].Use.Range
	}
	if sym == nil {
		return "", tree.Range{}, false
	}
	switch sym.Kind {
	case DeclFunction:
		// How a call reaches the function (through an alias, a variable, a
		// string) is said by the label lines.
		md := s.hoverFunction(sym)
		if len(h.binds) > 0 {
			for _, l := range s.siteLabels(file, h.binds) {
				md += l + "\n"
			}
		}
		return md, rng, true
	case DeclAlias:
		return s.hoverAlias(sym), rng, true
	}
	return s.hoverVariable(sym), rng, true
}

// siteLabels are the label lines of a call site: how the use of binds
// reaches each function, with the resolution and reason of the edge, e.g.
// "calls `build_release` through alias `br` (resolved/alias)". One line per
// distinct way, from the edges at the site (they carry conditional-definition
// and unset-in-view, which bindings do not); a plain resolved/direct call has
// none. A literal value (`cmd=f`) has no edge: its line comes from its
// bindings.
func (s *Snapshot) siteLabels(file int, binds []Binding) []string {
	u := binds[0].Use
	if u.Kind != UseCall {
		return nil
	}
	var out []string
	add := func(l string) {
		if !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	if u.Via == ViaValue {
		for _, b := range binds {
			if b.Sym != nil && b.Sym.Kind == DeclFunction && u.Node != nil {
				add(fmt.Sprintf("value of `%s` names `%s` (%s/%s)", u.Node.Name, b.Sym.Name, b.Res, b.Reason))
			}
		}
		return out
	}
	for _, r := range s.edgesAt(file, u) {
		name := s.declSym[r.to].Name
		if r.rel == RelCalls && r.res == Resolved && r.reason == codeDirect {
			continue
		}
		var how string
		switch {
		case r.rel == RelAliases:
			alias := "?"
			if d := s.aliasDeclOf(file, u); d != nil {
				alias = d.Name
			}
			how = fmt.Sprintf("alias `%s` names `%s`", alias, name)
		case u.Via == ViaVariable:
			how = fmt.Sprintf("calls `%s` through variable `%s`", name, u.Name)
		case u.Via == ViaEval:
			how = fmt.Sprintf("calls `%s` through an `eval` string", name)
		case u.Via == ViaTrap:
			how = fmt.Sprintf("calls `%s` through a `trap` string", name)
		case u.Via == ViaShellC:
			how = fmt.Sprintf("calls `%s` through a shell `-c` string", name)
		case u.Name != name: // the command word is an alias of the function
			how = fmt.Sprintf("calls `%s` through alias `%s`", name, u.Name)
		default:
			how = fmt.Sprintf("calls `%s`", name)
		}
		add(fmt.Sprintf("%s (%s/%s)", how, r.res, reasonNames[r.reason]))
	}
	return out
}

func fence(line string) string { return "```bash\n" + line + "\n```\n" }

func (s *Snapshot) hoverFunction(sym *Symbol) string {
	var sb strings.Builder
	sb.WriteString(fence(s.signature(sym.Decls[0])))
	if len(sym.Decls) > 1 {
		fmt.Fprintf(&sb, "%d definitions\n", len(sym.Decls))
	}
	for _, d := range sym.Decls {
		if d.Doc != "" {
			sb.WriteString(d.Doc + "\n")
			break
		}
	}
	sb.WriteString("\n")
	var defs []string
	for _, d := range sym.Decls {
		defs = append(defs, s.spot(d))
	}
	sb.WriteString("Defined in " + strings.Join(defs, ", ") + "\n")
	if sites := s.callSites(sym); len(sites) > 0 {
		noun := "sites"
		if len(sites) == 1 {
			noun = "site"
		}
		shown := sites
		more := ""
		if len(shown) > maxCallSites {
			more = fmt.Sprintf(", and %d more", len(shown)-maxCallSites)
			shown = shown[:maxCallSites]
		}
		fmt.Fprintf(&sb, "Called from %d %s: %s%s\n", len(sites), noun, strings.Join(shown, ", "), more)
	}
	reads, writes, exits := s.bodyEffects(sym)
	if len(reads) > 0 {
		sb.WriteString("Reads globals: " + quoteList(reads, maxGlobalsShown) + "\n")
	}
	if len(writes) > 0 {
		sb.WriteString("Writes globals: " + quoteList(writes, maxGlobalsShown) + "\n")
	}
	if exits {
		sb.WriteString("Calls `exit`\n")
	}
	return sb.String()
}

func quoteList(names []string, max int) string {
	if len(names) > max {
		names = names[:max]
	}
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = "`" + n + "`"
	}
	return strings.Join(q, ", ")
}

// callSites lists every call bound to sym, in (path, offset) order. Calls
// through an alias count; the target named inside an alias definition does not.
func (s *Snapshot) callSites(sym *Symbol) []string {
	type site struct {
		path   string
		offset int
		text   string
	}
	var sites []site
	for _, b := range s.BindingsOf(sym) {
		if b.Use.Kind != UseCall || b.Use.ViaAlias || b.Use.Via == ViaValue {
			continue
		}
		t := fmt.Sprintf("`%s:%d`", s.rel(b.File), b.Use.Range.Start.Line)
		if b.Use.Scope != nil && b.Use.Scope.Func != "" {
			t += " (" + b.Use.Scope.Func + ")"
		}
		sites = append(sites, site{b.File, b.Use.Range.Start.Offset, t})
	}
	sort.SliceStable(sites, func(a, b int) bool {
		if sites[a].path != sites[b].path {
			return sites[a].path < sites[b].path
		}
		return sites[a].offset < sites[b].offset
	})
	out := make([]string, len(sites))
	for i, x := range sites {
		out[i] = x.text
	}
	return out
}

// bodyEffects collects, over every definition of sym, the non-local variables
// the body reads and writes (sorted, no duplicates) and whether it calls exit.
func (s *Snapshot) bodyEffects(sym *Symbol) (reads, writes []string, exits bool) {
	rs, ws := map[string]bool{}, map[string]bool{}
	// One pass over each file's uses and bindings, against the definitions'
	// spans merged and sorted, instead of one pass per definition (O(definitions
	// x uses): 0.46 s for one hover at 20,000 definitions of one name).
	spans := map[int][][2]int{}
	for _, d := range sym.Decls {
		f := s.declFile[d]
		spans[f] = append(spans[f], [2]int{d.Node.Range.Start.Offset, d.Node.Range.End.Offset})
	}
	for f, sp := range spans {
		sp = mergeSpans(sp)
		in := func(r tree.Range) bool {
			off := r.Start.Offset
			i := sort.Search(len(sp), func(i int) bool { return sp[i][1] > off })
			return i < len(sp) && sp[i][0] <= off
		}
		for _, u := range s.facts[f].Uses {
			// a `bash -c` body runs in a child and a trap action later: neither exits from the function
			if u.Kind == UseCall && u.Via != ViaVariable && u.Via != ViaShellC && u.Via != ViaTrap && u.Name == "exit" && in(u.Range) {
				exits = true
			}
		}
		for _, b := range s.bindings[f] {
			if b.Use.Kind == UseCall || !in(b.Use.Range) || b.Sym == nil || b.Sym.Func != "" {
				continue
			}
			if len(b.Sym.Decls) > 0 && b.Sym.Decls[0].Temp {
				continue
			}
			if b.Use.Kind == UseVarRead {
				rs[b.Use.Name] = true
			} else {
				ws[b.Use.Name] = true
			}
		}
	}
	return sortedKeys(rs), sortedKeys(ws), exits
}

// mergeSpans sorts the half-open spans sp by start and merges those that
// overlap, so that an offset lies in one of sp exactly when it lies in one of
// the result.
func mergeSpans(sp [][2]int) [][2]int {
	sort.Slice(sp, func(i, j int) bool { return sp[i][0] < sp[j][0] })
	out := sp[:0]
	for _, x := range sp {
		if n := len(out); n > 0 && x[0] < out[n-1][1] {
			out[n-1][1] = max(out[n-1][1], x[1])
			continue
		}
		out = append(out, x)
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// signature renders a function as its name followed by its positional parameters.
func (s *Snapshot) signature(d *Decl) string {
	names, max, args := positionalParams(d.Node, s.src(d))
	var sb strings.Builder
	sb.WriteString(d.Name)
	for i := 1; i <= max; i++ {
		if n := names[i]; n != "" {
			sb.WriteString(" <" + n + ">")
		} else {
			sb.WriteString(" <$" + strconv.Itoa(i) + ">")
		}
	}
	if args {
		sb.WriteString(" [args...]")
	}
	return sb.String()
}

// positionalParams reads the parameter expansions in a function body: the
// highest positional index read, a name for each index that is copied into a
// variable (`NAME="$i"`, `NAME=$i`, `NAME="${i}"`, with or without `:-default`),
// and whether "$@" or "$*" is read. Nested function bodies are skipped.
func positionalParams(fn *tree.Node, src []byte) (names map[int]string, max int, args bool) {
	names = map[int]string{}
	fn.Walk(func(n *tree.Node) bool {
		if n != fn && n.Kind == tree.KFuncDecl {
			return false
		}
		switch n.Kind {
		case tree.KParamExp:
			if i := positionalIndex(n.Name); i > 0 {
				if i > max {
					max = i
				}
			} else if n.Name == "@" || n.Name == "*" {
				args = true
			}
		case tree.KAssign:
			if i := copiedParam(n, src); i > 0 && names[i] == "" {
				names[i] = n.Name
			}
		}
		return true
	})
	return names, max, args
}

func positionalIndex(name string) int {
	if name == "" || name == "0" {
		return 0
	}
	i, err := strconv.Atoi(name)
	if err != nil || i < 0 {
		return 0
	}
	return i
}

// copiedParam returns i when the assignment is NAME=$i in one of the accepted forms, else 0.
func copiedParam(a *tree.Node, src []byte) int {
	if a.Name == "" || len(a.Children) == 0 {
		return 0
	}
	e := a.NameRange.End.Offset
	if e+1 >= len(src) || src[e] != '=' || src[e+1] == '(' { // `+=`, `[i]=` and arrays are not copies
		return 0
	}
	w := a.Children[len(a.Children)-1]
	if w.Kind != tree.KWord || len(w.Children) != 1 {
		return 0
	}
	p := w.Children[0]
	if p.Kind == tree.KDblQuoted && len(p.Children) == 1 {
		p = p.Children[0]
	}
	if p.Kind != tree.KParamExp {
		return 0
	}
	i := positionalIndex(p.Name)
	if i == 0 {
		return 0
	}
	if o := p.Range.Start.Offset; o+1 < len(src) && src[o+1] == '{' {
		rest := src[p.NameRange.End.Offset:]
		if !(len(rest) > 0 && rest[0] == '}') && !bytes.HasPrefix(rest, []byte(":-")) && !bytes.HasPrefix(rest, []byte("-")) {
			return 0
		}
	}
	return i
}

func (s *Snapshot) hoverAlias(sym *Symbol) string {
	d := sym.Decls[0]
	var sb strings.Builder
	sb.WriteString(fence(declSource(d, s.src(d))))
	if d.AliasTo != "" {
		sb.WriteString("Alias of `" + d.AliasTo + "`\n")
	}
	sb.WriteString("Defined in " + s.spot(d) + "\n")
	return sb.String()
}

func (s *Snapshot) hoverVariable(sym *Symbol) string {
	var sb strings.Builder
	if len(sym.Decls) == 0 {
		sb.WriteString(fence(sym.Name))
		sb.WriteString("Not assigned in this workspace; read from the environment.\n")
		return sb.String()
	}
	d := sym.Decls[0]
	sb.WriteString(fence(declSource(d, s.src(d))))
	switch {
	case d.Temp:
		sb.WriteString("Set for one command only, at " + s.spot(d) + "\n")
	case sym.Private != 0:
		fmt.Fprintf(&sb, "Visible only inside its %s in `%s`\n", privateName(sym.Private), s.rel(sym.File))
	case sym.Func != "":
		fmt.Fprintf(&sb, "Local to `%s` in `%s`\n", sym.Func, s.rel(sym.File))
	default:
		var at []string
		for _, x := range sym.Decls {
			at = append(at, s.spot(x))
		}
		more := ""
		if len(at) > maxCallSites {
			more = fmt.Sprintf(", and %d more", len(at)-maxCallSites)
			at = at[:maxCallSites]
		}
		sb.WriteString("Global, declared in " + strings.Join(at, ", ") + more + "\n")
	}
	var attrs VarAttrs
	for _, x := range sym.Decls {
		a := x.Attrs
		attrs.Exported = attrs.Exported || a.Exported
		attrs.Readonly = attrs.Readonly || a.Readonly
		attrs.Integer = attrs.Integer || a.Integer
		attrs.Array = attrs.Array || a.Array
		attrs.Assoc = attrs.Assoc || a.Assoc
		attrs.Nameref = attrs.Nameref || a.Nameref
	}
	if names := attrNames(attrs); len(names) > 0 {
		sb.WriteString("Attributes: " + strings.Join(names, ", ") + "\n")
	}
	if len(sym.Decls) == 1 && d.Literal != "" {
		sb.WriteString("Value: `" + d.Literal + "`\n")
	}
	return sb.String()
}

// hoverDynamicScope describes a variable read that no declaration in view
// explains, bound only to callers' locals through dynamic scope (FDS-006):
// labelled candidate/dynamic-scope, with every candidate local listed (in
// path and offset order, the first maxCallSites shown and the rest counted).
func (s *Snapshot) hoverDynamicScope(binds []Binding) string {
	var sb strings.Builder
	sb.WriteString(fence(binds[0].Use.Name))
	sb.WriteString("No declaration in view; at run time the value may come from a caller's `local` (" +
		Candidate.String() + "/" + ReasonDynamicScope + ").\n")
	var ds []*Decl
	for _, b := range binds {
		if b.Sym != nil && len(b.Sym.Decls) > 0 {
			ds = append(ds, b.Sym.Decls[0])
		}
	}
	sort.SliceStable(ds, func(i, j int) bool {
		pi, pj := s.paths[s.declFile[ds[i]]], s.paths[s.declFile[ds[j]]]
		if pi != pj {
			return pi < pj
		}
		return ds[i].NameRange.Start.Offset < ds[j].NameRange.Start.Offset
	})
	noun := "locals"
	if len(ds) == 1 {
		noun = "local"
	}
	fmt.Fprintf(&sb, "%d candidate caller %s:\n", len(ds), noun)
	for i, d := range ds {
		if i == maxCallSites {
			fmt.Fprintf(&sb, "- and %d more\n", len(ds)-maxCallSites)
			break
		}
		fmt.Fprintf(&sb, "- `%s` in `%s` at %s\n", declSource(d, s.src(d)), s.declSym[d].Func, s.spot(d))
	}
	return sb.String()
}

// privateName names a private scope kind for hover.
func privateName(k ScopeKind) string {
	switch k {
	case ScopeCmdSubst:
		return "command substitution"
	case ScopePipeline:
		return "pipeline stage"
	case ScopeProcSubst:
		return "process substitution"
	case ScopeCoproc:
		return "coproc"
	}
	return "subshell"
}

// attrNames lists the attributes that are set, excluding scope.
func attrNames(a VarAttrs) []string {
	var out []string
	for _, x := range []struct {
		on   bool
		name string
	}{{a.Exported, "exported"}, {a.Readonly, "readonly"}, {a.Integer, "integer"},
		{a.Array, "array"}, {a.Assoc, "associative"}, {a.Nameref, "nameref"}} {
		if x.on {
			out = append(out, x.name)
		}
	}
	return out
}

// declSource is the first line of a declaration's source text, cut at 120 characters.
// A declaration inside `local`/`declare`/`export` shows the whole clause.
func declSource(d *Decl, src []byte) string {
	n := d.Node
	if n.Kind == tree.KAssign && n.Parent != nil && n.Parent.Kind == tree.KDecl {
		n = n.Parent
	}
	a, b := n.Range.Start.Offset, n.Range.End.Offset
	if a < 0 || b > len(src) || a >= b {
		return d.Name
	}
	line := string(src[a:b])
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	if r := []rune(line); len(r) > maxDeclText {
		line = string(r[:maxDeclText])
	}
	return line
}

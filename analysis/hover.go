package analysis

import (
	"bytes"
	"fmt"
	"path/filepath"
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
func (s *Snapshot) Hover(path string, offset int) (markdown string, rng tree.Range, ok bool) {
	h, _, found := s.at(path, offset)
	if !found || h.edge != nil {
		return "", tree.Range{}, false
	}
	var sym *Symbol
	via := ""
	switch {
	case h.decl != nil:
		sym = s.declSym[h.decl]
		rng = h.decl.NameRange
		if h.decl.Kind == DeclFunction {
			rng = h.decl.Node.Range
		}
	case len(h.binds) > 0:
		sym = h.binds[0].Sym
		rng = h.binds[0].Use.Range
		for _, b := range h.binds {
			if b.Sym == sym && b.Reason == "alias" {
				via = b.Use.Name
			}
		}
	}
	if sym == nil {
		return "", tree.Range{}, false
	}
	switch sym.Kind {
	case DeclFunction:
		return s.hoverFunction(sym, via), rng, true
	case DeclAlias:
		return s.hoverAlias(sym), rng, true
	}
	return s.hoverVariable(sym), rng, true
}

func fence(line string) string { return "```bash\n" + line + "\n```\n" }

func (s *Snapshot) hoverFunction(sym *Symbol, via string) string {
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
	if via != "" {
		sb.WriteString("Reached through alias `" + via + "`\n")
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
		if b.Use.Kind != UseCall || b.Use.ViaAlias {
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
	for _, d := range sym.Decls {
		f := s.declFile[d]
		in := func(r tree.Range) bool {
			return r.Start.Offset >= d.Node.Range.Start.Offset && r.Start.Offset < d.Node.Range.End.Offset
		}
		for _, u := range s.facts[f].Uses {
			if u.Kind == UseCall && u.Name == "exit" && in(u.Range) {
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
		sb.WriteString("Environment variable (not assigned in this workspace)\n")
		return sb.String()
	}
	d := sym.Decls[0]
	sb.WriteString(fence(declSource(d, s.src(d))))
	switch {
	case d.Temp:
		sb.WriteString("Set for one command only, at " + s.spot(d) + "\n")
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

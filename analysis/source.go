package analysis

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/wamra/bash-lsp/tree"
)

// SourceStatus is the outcome of resolving one SourceRef.
type SourceStatus uint8

const (
	SourceResolved SourceStatus = iota + 1
	SourceUnresolved
	SourceIgnored // directive source=/dev/null
)

// SourceEdge is one resolved (or not) `source` command. Targets are clean
// absolute paths that exist in the caller's file set. How is "directive",
// "literal", "script-dir" or "loop-glob"; empty when unresolved.
type SourceEdge struct {
	Ref     SourceRef
	Targets []string
	Status  SourceStatus
	How     string
	// Skipped, for an unresolved source, is the clean absolute path of the
	// existing file it names that Options.Oversize reports skipped for its
	// size; "" otherwise.
	Skipped string
}

// scriptDirForms are the spellings of "the directory of this script", with
// double quotes and extra spaces removed before comparison.
var scriptDirForms = map[string]bool{
	"$(dirname $0)":                true,
	"$(dirname ${BASH_SOURCE[0]})": true,
	"$(dirname $BASH_SOURCE)":      true,
	"${BASH_SOURCE[0]%/*}":         true,
	"${BASH_SOURCE%/*}":            true,
	"${0%/*}":                      true,
}

// ResolveSources resolves every source command of ff. exists reports whether
// an absolute path is a known workspace file; glob expands an absolute pattern
// against known workspace files. searchPaths are extra absolute directories.
func ResolveSources(ff *FileFacts, root string, searchPaths []string, exists func(string) bool, glob func(string) []string) []SourceEdge {
	if ff == nil {
		return nil
	}
	r := &resolver{ff: ff, root: root, search: searchPaths, exists: exists, glob: glob, dir: filepath.Dir(ff.Path)}
	out := make([]SourceEdge, 0, len(ff.Sources))
	for _, ref := range ff.Sources {
		out = append(out, r.resolve(ref))
	}
	return out
}

// markOversize sets Skipped on each unresolved edge of es whose path
// resolves, by the same rules, to a file oversize reports.
func markOversize(es []SourceEdge, ff *FileFacts, root string, searchPaths []string, oversize func(string) bool) {
	var r *resolver
	for i := range es {
		if es[i].Status != SourceUnresolved {
			continue
		}
		if r == nil {
			r = &resolver{ff: ff, root: root, search: searchPaths, exists: oversize, glob: func(string) []string { return nil }, dir: filepath.Dir(ff.Path)}
		}
		if e := r.resolve(es[i].Ref); e.Status == SourceResolved && len(e.Targets) > 0 {
			es[i].Skipped = e.Targets[0]
		}
	}
}

type resolver struct {
	ff     *FileFacts
	root   string
	search []string
	dir    string
	exists func(string) bool
	glob   func(string) []string

	// only maps each variable name to its single non-temporary declaration,
	// nil for a name declared more than once; filled on first use, so that
	// varIsScriptDir does not scan every declaration per reference (O(sources
	// x declarations): 63 s for 900 KB).
	only map[string]*Decl
}

func (r *resolver) resolve(ref SourceRef) SourceEdge {
	e := SourceEdge{Ref: ref, Status: SourceUnresolved}
	hit := func(how string, targets ...string) SourceEdge {
		e.Status, e.How, e.Targets = SourceResolved, how, targets
		return e
	}
	if ref.Directive != "" {
		if ref.Directive == "/dev/null" {
			e.Status, e.How = SourceIgnored, "directive"
			return e
		}
		if t := r.firstExisting(ref.Directive, r.dir, r.search, []string{r.root}); t != "" {
			return hit("directive", t)
		}
	}
	if ref.Word == nil {
		return e
	}
	if lit, ok := literalText(ref.Word); ok && lit != "" {
		if r.batsLoad(ref) {
			// bats `load NAME`: NAME.bash, else NAME, beside the test file
			for _, p := range []string{lit + ".bash", lit} {
				if t := r.firstExisting(p, r.dir); t != "" {
					return hit("literal", t)
				}
			}
			return e
		}
		if t := r.firstExisting(lit, r.dir, []string{r.root}, r.search); t != "" {
			return hit("literal", t)
		}
	}
	if t := r.scriptDir(ref.Word); t != "" {
		return hit("script-dir", t)
	}
	if ts := r.loopGlob(ref); len(ts) > 0 {
		return hit("loop-glob", ts...)
	}
	return e
}

// batsLoad reports a bats `load` command (Bind records `load` as a source
// only in a bats file).
func (r *resolver) batsLoad(ref SourceRef) bool {
	if r.ff.File == nil || r.ff.File.Dialect != tree.Bats || ref.Node == nil {
		return false
	}
	for _, w := range ref.Node.Children {
		if w.Kind == tree.KWord {
			name, ok, _ := commandName(w)
			return ok && name == "load"
		}
	}
	return false
}

// firstExisting tries p as-is when absolute, else against each directory in
// the given groups in order.
func (r *resolver) firstExisting(p string, groups ...any) string {
	if filepath.IsAbs(p) {
		if c := filepath.Clean(p); r.exists(c) {
			return c
		}
		return ""
	}
	for _, g := range groups {
		var dirs []string
		switch v := g.(type) {
		case string:
			dirs = []string{v}
		case []string:
			dirs = v
		}
		for _, d := range dirs {
			if d == "" {
				continue
			}
			if c := filepath.Join(d, p); r.exists(c) {
				return c
			}
		}
	}
	return ""
}

// wordParts flattens a word into its parts, unwrapping double quotes.
func wordParts(w *tree.Node) []*tree.Node {
	var parts []*tree.Node
	for _, p := range w.Children {
		if p.Kind == tree.KDblQuoted {
			parts = append(parts, p.Children...)
		} else {
			parts = append(parts, p)
		}
	}
	return parts
}

func (r *resolver) text(n *tree.Node) string {
	s, e := n.Range.Start.Offset, n.Range.End.Offset
	src := r.ff.File.Src
	if s < 0 || e > len(src) || s > e {
		return ""
	}
	return string(src[s:e])
}

// norm drops double quotes and collapses whitespace so quoting variants compare equal.
func norm(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, `"`, "")), " ")
}

// isScriptDirNode reports whether n is one of the "directory of this script" forms.
func (r *resolver) isScriptDirNode(n *tree.Node) bool {
	if n.Kind != tree.KCmdSubst && n.Kind != tree.KParamExp {
		return false
	}
	t := norm(r.text(n))
	if scriptDirForms[t] {
		return true
	}
	const pre, post = "$(cd ", " && pwd)"
	if n.Kind == tree.KCmdSubst && strings.HasPrefix(t, pre) && strings.HasSuffix(t, post) {
		return scriptDirForms[t[len(pre):len(t)-len(post)]]
	}
	return false
}

// scriptDir resolves `<script-dir form><literal rest>` words, directly or via a
// variable assigned exactly once from such a form.
func (r *resolver) scriptDir(w *tree.Node) string {
	parts := wordParts(w)
	if len(parts) == 0 {
		return ""
	}
	first := parts[0]
	ok := r.isScriptDirNode(first)
	if !ok && first.Kind == tree.KParamExp && first.Name != "" && len(first.Children) == 0 {
		ok = r.varIsScriptDir(first.Name)
	}
	if !ok {
		return ""
	}
	var rest strings.Builder
	for _, p := range parts[1:] {
		if p.Kind != tree.KLit || p.Recovered {
			return ""
		}
		rest.WriteString(p.Value)
	}
	if rest.Len() == 0 {
		return ""
	}
	t := filepath.Join(r.dir, filepath.FromSlash(rest.String()))
	if r.exists(t) {
		return t
	}
	return ""
}

func (r *resolver) varIsScriptDir(name string) bool {
	if r.only == nil {
		r.only = map[string]*Decl{}
		for _, c := range r.ff.Decls {
			if c.Kind == DeclVariable && !c.Temp {
				if _, seen := r.only[c.Name]; seen {
					r.only[c.Name] = nil
				} else {
					r.only[c.Name] = c
				}
			}
		}
	}
	d := r.only[name]
	if d == nil || d.Implicit != "" || d.Node == nil || d.Node.Kind != tree.KAssign {
		return false
	}
	var val *tree.Node
	for i := len(d.Node.Children) - 1; i >= 0; i-- {
		if c := d.Node.Children[i]; c.Kind == tree.KWord {
			val = c
			break
		}
	}
	if val == nil {
		return false
	}
	parts := wordParts(val)
	return len(parts) == 1 && r.isScriptDirNode(parts[0])
}

// loopGlob resolves `$f` inside `for f in <literal words>`.
func (r *resolver) loopGlob(ref SourceRef) []string {
	parts := wordParts(ref.Word)
	if len(parts) != 1 || parts[0].Kind != tree.KParamExp || len(parts[0].Children) != 0 {
		return nil
	}
	name := parts[0].Name
	if t := norm(r.text(parts[0])); t != "$"+name && t != "${"+name+"}" {
		return nil
	}
	loop := ref.loop // Bind tracked the loops: no walk up the tree per reference
	if !ref.loopKnown {
		for p := ref.Node.Parent; p != nil; p = p.Parent {
			if p.Kind == tree.KFor && p.Name == name {
				loop = p
				break
			}
		}
	}
	if loop == nil {
		return nil
	}
	var pats []string
	for _, c := range loop.Children {
		if c.Kind != tree.KWord {
			continue
		}
		s, ok := literalText(c)
		if !ok || s == "" {
			return nil
		}
		pats = append(pats, s)
	}
	if len(pats) == 0 {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, p := range pats {
		for _, t := range r.expand(p) {
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	return out
}

// expand globs one pattern against the file's directory, then root.
func (r *resolver) expand(p string) []string {
	bases := []string{r.dir, r.root}
	if filepath.IsAbs(p) {
		bases = []string{""}
	}
	for _, b := range bases {
		pat := p
		if b != "" {
			pat = filepath.Join(b, filepath.FromSlash(p))
		}
		pat = filepath.Clean(pat)
		var hits []string
		if strings.ContainsAny(p, "*?[") {
			for _, h := range r.glob(pat) {
				if h = filepath.Clean(h); r.exists(h) {
					hits = append(hits, h)
				}
			}
		} else if r.exists(pat) {
			hits = []string{pat}
		}
		if len(hits) > 0 {
			sort.Strings(hits)
			return hits
		}
	}
	return nil
}

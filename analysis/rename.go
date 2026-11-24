package analysis

import (
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/wamra/bash-lsp/tree"
)

// RenameEdit replaces one name with NewText.
type RenameEdit struct {
	Path    string
	Range   tree.Range
	NewText string
}

// RenameExclusion is a site that mentions the symbol but is never edited:
// the rename cannot prove that the text there names it.
type RenameExclusion struct {
	Loc          Location
	Res          Resolution
	Reason, Text string
}

// RenamePlan is the outcome of a rename that was not refused.
type RenamePlan struct {
	Symbol   *Symbol
	Old, New string
	Edits    []RenameEdit      // sorted (Path, Range.Start.Offset), no duplicates
	Excluded []RenameExclusion // never edited
	Others   []Location        // declarations of same-named, different symbols
	Warnings []string
}

// RenameRefusal is the error of a rename that cannot be done safely.
type RenameRefusal struct {
	Code, Reason string
	Sites        []Location
}

func (r *RenameRefusal) Error() string { return "rename refused (" + r.Code + "): " + r.Reason }

// Refusal codes.
const (
	RefuseNotSymbol     = "not-a-symbol"
	RefuseCandidateSite = "candidate-site"
	RefuseAmbiguous     = "ambiguous-symbol"
	RefuseInvalidName   = "invalid-name"
	RefuseReserved      = "reserved-name"
	RefuseConflict      = "name-conflict"
	RefuseRecovered     = "recovered-region"
	RefuseSkippedFile   = "skipped-file"
	RefuseNoDecl        = "no-declaration"
	RefuseTooMany       = "too-many-sites"
)

const (
	maxRenameSites  = 100000 // edits plus exclusions; above it the plan is refused with RefuseTooMany
	maxRenameOthers = 1000   // declarations listed in RenamePlan.Others
	maxRefusalSites = 100    // sites listed in one refusal
	maxExcludedText = 120    // bytes of source text kept in an exclusion

	reasonInexactName = "inexact-name" // the text of the site is not exactly the name
)

// refuse makes a refusal, listing the sites in order, each place once, at
// most maxRefusalSites of them.
func refuse(code, reason string, sites []Location) *RenameRefusal {
	slices.SortStableFunc(sites, locCmp)
	sites = slices.CompactFunc(sites, func(a, b Location) bool {
		return a.Path == b.Path && a.Range.Start.Offset == b.Range.Start.Offset
	})
	if len(sites) > maxRefusalSites {
		reason += fmt.Sprintf(" (first %d of %d sites)", maxRefusalSites, len(sites))
		sites = sites[:maxRefusalSites]
	}
	return &RenameRefusal{Code: code, Reason: reason, Sites: sites}
}

// renameSite is the place under a cursor that names the symbol to rename.
type renameSite struct {
	key refKey
	rng tree.Range
	sym *Symbol
}

// siteText is the source text of rng in file, cut to maxExcludedText bytes.
func (s *Snapshot) siteText(file int, rng tree.Range) string {
	src := s.facts[file].File.Src
	a, b := rng.Start.Offset, rng.End.Offset
	if a < 0 || a > b || b > len(src) {
		return ""
	}
	if b-a > maxExcludedText {
		b = a + maxExcludedText
		for b > a && !utf8.RuneStart(src[b]) {
			b--
		}
	}
	return string(src[a:b])
}

// renameCandidates lists the symbols the text under offset names: a
// declaration name or a bound use whose symbol is called what the text says.
// A call through an alias names the alias, not the function it reaches. When
// nothing is under offset it looks one byte to the left, as a cursor just
// after a name does.
func (s *Snapshot) renameCandidates(path string, offset int) []renameSite {
	file, ok := s.idx[path]
	if !ok {
		return nil
	}
	for _, o := range []int{offset, offset - 1} {
		if o < 0 {
			continue
		}
		var out []renameSite
		add := func(rng tree.Range, sym *Symbol) {
			if sym == nil || slices.ContainsFunc(out, func(c renameSite) bool { return c.sym == sym }) {
				return
			}
			out = append(out, renameSite{key: refKey{path, rng.Start.Offset}, rng: rng, sym: sym})
		}
		for _, d := range s.facts[file].Decls {
			if sym := s.declSym[d]; sym != nil && d.NameRange.Contains(o) && s.siteText(file, d.NameRange) == sym.Name {
				add(d.NameRange, sym)
			}
		}
		for _, b := range s.bindings[file] {
			u := b.Use
			if b.Res == Unresolved || b.Sym == nil || !u.Range.Contains(o) {
				continue
			}
			text := s.siteText(file, u.Range)
			if b.Sym.Name == text {
				add(u.Range, b.Sym)
			}
			if b.Reason == ReasonAlias && u.Name == text {
				add(u.Range, s.pick(s.aliasSyms[text], file))
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// cursorSymbol resolves the cursor to one symbol or refuses.
func (s *Snapshot) cursorSymbol(path string, offset int) (*renameSite, error) {
	cands := s.renameCandidates(path, offset)
	switch len(cands) {
	case 0:
		return nil, refuse(RefuseNotSymbol, "the text at the cursor is not the name of a function, alias or variable of the workspace", nil)
	case 1:
		return &cands[0], nil
	}
	var sites []Location
	for _, c := range cands {
		sites = append(sites, s.symbolDecls(c.sym)...)
	}
	return nil, refuse(RefuseAmbiguous, fmt.Sprintf("the name at the cursor is bound to %d symbols", len(cands)), sites)
}

func (s *Snapshot) symbolDecls(sym *Symbol) []Location {
	out := make([]Location, 0, len(sym.Decls))
	for _, d := range sym.Decls {
		out = append(out, s.declLoc(d))
	}
	return out
}

func candidateSiteRefusal(name string, site *renameSite) *RenameRefusal {
	return refuse(RefuseCandidateSite, "this site may or may not name "+name+" and is not renamed",
		[]Location{{Path: site.key.path, Range: site.rng}})
}

// PrepareRename returns the range of the name at the cursor and the text to
// show as its placeholder, or a *RenameRefusal when no rename can start there.
func (s *Snapshot) PrepareRename(path string, offset int) (tree.Range, string, error) {
	site, err := s.cursorSymbol(path, offset)
	if err != nil {
		return tree.Range{}, "", err
	}
	rs, err := s.renameSites(site.sym)
	if err != nil {
		return tree.Range{}, "", err
	}
	if rs.isExcluded(site.key) {
		return tree.Range{}, "", candidateSiteRefusal(site.sym.Name, site)
	}
	return site.rng, site.sym.Name, nil
}

// PlanRename plans the rename of the symbol under the cursor to newName.
func (s *Snapshot) PlanRename(path string, offset int, newName string) (*RenamePlan, error) {
	site, err := s.cursorSymbol(path, offset)
	if err != nil {
		return nil, err
	}
	return s.planSymbol(site.sym, newName, site)
}

// PlanRenameByName plans the rename of the function called name. When several
// symbols have that name, the one whose declaring files are visible from the
// most files wins; a tie is refused. The others are listed in Others.
func (s *Snapshot) PlanRenameByName(name, newName string) (*RenamePlan, error) {
	syms := s.funcSyms[name]
	if len(syms) == 0 {
		return nil, refuse(RefuseNotSymbol, "no function named "+name, nil)
	}
	var best []*Symbol
	bestN := -1
	for _, sym := range syms {
		if n := s.viewCount(sym); n > bestN {
			best, bestN = []*Symbol{sym}, n
		} else if n == bestN {
			best = append(best, sym)
		}
	}
	if len(best) > 1 {
		var sites []Location
		for _, sym := range best {
			sites = append(sites, s.symbolDecls(sym)...)
		}
		return nil, refuse(RefuseAmbiguous, fmt.Sprintf("%d functions named %s are equally widely visible", len(best), name), sites)
	}
	return s.planSymbol(best[0], newName, nil)
}

// declFiles returns the distinct files that declare sym.
func (s *Snapshot) declFiles(sym *Symbol) []int {
	var out []int
	for _, d := range sym.Decls {
		if f := s.declFile[d]; !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

// sees reports whether file can see a declaration in one of dfs.
func (s *Snapshot) sees(file int, dfs []int) bool {
	for _, df := range dfs {
		if s.visible[file].has(df) {
			return true
		}
	}
	return false
}

// viewCount is the number of files from which a declaration of sym is visible.
func (s *Snapshot) viewCount(sym *Symbol) int {
	dfs := s.declFiles(sym)
	n := 0
	for f := range s.paths {
		if s.sees(f, dfs) {
			n++
		}
	}
	return n
}

// renameSet is the sites of one symbol: edits and exclusions, sorted by place.
type renameSet struct {
	edits []Location
	excl  []RenameExclusion
	seen  map[refKey]bool // every place already claimed, as an edit, an exclusion or a skipped alias call
	n     int             // edits plus exclusions
	excls map[refKey]bool // the exclusions' places, made on first use
}

func (r *renameSet) isExcluded(k refKey) bool {
	if r.excls == nil {
		r.excls = make(map[refKey]bool, len(r.excl))
		for _, x := range r.excl {
			r.excls[refKey{x.Loc.Path, x.Loc.Range.Start.Offset}] = true
		}
	}
	return r.excls[k]
}

// claim marks the place of rng in file as taken; false when it already was.
func (s *Snapshot) claim(r *renameSet, file int, rng tree.Range) (bool, error) {
	k := refKey{s.paths[file], rng.Start.Offset}
	if r.seen[k] {
		return false, nil
	}
	if r.n++; r.n > maxRenameSites {
		return false, refuse(RefuseTooMany, fmt.Sprintf("more than %d sites to edit or confirm by hand", maxRenameSites), nil)
	}
	r.seen[k] = true
	return true, nil
}

// addEdit records an edit at rng of file when the text there is exactly old,
// and an exclusion otherwise. A place claimed before is left alone.
func (s *Snapshot) addEdit(r *renameSet, file int, rng tree.Range, old string) error {
	if ok, err := s.claim(r, file, rng); !ok || err != nil {
		return err
	}
	loc := Location{Path: s.paths[file], Range: rng}
	if text := s.siteText(file, rng); text != old {
		r.excl = append(r.excl, RenameExclusion{Loc: loc, Res: Resolved, Reason: reasonInexactName, Text: text})
		return nil
	}
	r.edits = append(r.edits, loc)
	return nil
}

func (s *Snapshot) addExclusion(r *renameSet, file int, rng tree.Range, res Resolution, reason string) error {
	if ok, err := s.claim(r, file, rng); !ok || err != nil {
		return err
	}
	r.excl = append(r.excl, RenameExclusion{Loc: Location{Path: s.paths[file], Range: rng}, Res: res, Reason: reason, Text: s.siteText(file, rng)})
	return nil
}

// renameSites sorts every site that mentions sym into an edit or an
// exclusion, in one pass over its bindings and one over its edges.
//
// An edit is a declaration name, or a resolved use whose text is the name and
// whose edges to sym are all resolved. A call through an alias is neither: it
// keeps working because the alias value word is renamed. Everything else that
// mentions sym is an exclusion.
func (s *Snapshot) renameSites(sym *Symbol) (*renameSet, error) {
	r := &renameSet{seen: map[refKey]bool{}}
	for _, d := range sym.Decls {
		if err := s.addEdit(r, s.declFile[d], d.NameRange, sym.Name); err != nil {
			return nil, err
		}
	}
	var err error
	if sym.Kind == DeclAlias {
		err = s.aliasCallSites(r, sym)
	} else {
		err = s.symbolUseSites(r, sym)
	}
	if err != nil {
		return nil, err
	}
	slices.SortFunc(r.edits, locCmp)
	slices.SortFunc(r.excl, func(a, b RenameExclusion) int { return locCmp(a.Loc, b.Loc) })
	return r, nil
}

func (s *Snapshot) symbolUseSites(r *renameSet, sym *Symbol) error {
	old := sym.Name
	byKey := map[refKey][]Edge{} // the edges to sym, by site
	for _, e := range s.EdgesTo(sym) {
		k := refKey{e.Site.Path, e.Site.Range.Start.Offset}
		byKey[k] = append(byKey[k], e)
	}
	for _, b := range s.bySym[sym] {
		u := b.Use
		k := refKey{b.File, u.Range.Start.Offset}
		if r.seen[k] {
			continue
		}
		file := s.idx[b.File]
		var err error
		switch {
		case u.Via == ViaVariable:
			err = s.addExclusion(r, file, u.Range, b.Res, b.Reason)
		case u.Name != old && b.Reason == ReasonAlias:
			r.seen[k] = true // a call through an alias: the alias value word is the edit
		case u.Name != old || b.Res != Resolved || u.Via != ViaNone:
			err = s.addExclusion(r, file, u.Range, b.Res, b.Reason)
		case !u.NamesTarget():
			err = s.addExclusion(r, file, u.Range, b.Res, reasonInexactName)
		default:
			if e, bad := firstUnresolved(byKey[k]); bad {
				err = s.addExclusion(r, file, u.Range, e.Res, e.Reason)
			} else {
				err = s.addEdit(r, file, u.Range, old)
			}
		}
		if err != nil {
			return err
		}
	}
	for k, es := range byKey { // edge sites that no binding of sym explains
		if r.seen[k] {
			continue
		}
		e := es[0]
		if err := s.addExclusion(r, s.idx[e.Site.Path], e.Site.Range, e.Res, e.Reason); err != nil {
			return err
		}
	}
	return nil
}

func firstUnresolved(es []Edge) (Edge, bool) {
	for _, e := range es {
		if e.Res != Resolved {
			return e, true
		}
	}
	return Edge{}, false
}

// aliasCallSites finds the calls that bash expands through the alias sym: a
// call word with its name, in a file that sees the alias, written without an
// escape. Their bindings go to the function the alias leads to (or to the
// alias when it leads nowhere), so they are found by name.
func (s *Snapshot) aliasCallSites(r *renameSet, sym *Symbol) error {
	old := sym.Name
	dfs := s.declFiles(sym)
	for f := range s.paths {
		if !s.sees(f, dfs) || s.pick(s.aliasSyms[old], f) != sym {
			continue
		}
		for _, b := range s.bindings[f] {
			u := b.Use
			if u.Kind != UseCall || u.Name != old || u.noAlias || u.Via == ViaVariable {
				continue
			}
			var err error
			switch {
			case b.Res != Resolved || u.Via != ViaNone:
				err = s.addExclusion(r, f, u.Range, b.Res, b.Reason)
			case !u.NamesTarget():
				err = s.addExclusion(r, f, u.Range, b.Res, reasonInexactName)
			default:
				err = s.addEdit(r, f, u.Range, old)
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// planSymbol plans the rename of sym; cursor, when not nil, is the site the
// request came from, which must not be an exclusion.
func (s *Snapshot) planSymbol(sym *Symbol, newName string, cursor *renameSite) (*RenamePlan, error) {
	if len(sym.Decls) == 0 {
		return nil, refuse(RefuseNoDecl, sym.Name+" has no declaration in the workspace", nil)
	}
	rs, err := s.renameSites(sym)
	if err != nil {
		return nil, err
	}
	if cursor != nil && rs.isExcluded(cursor.key) {
		return nil, candidateSiteRefusal(sym.Name, cursor)
	}
	if !identRe.MatchString(newName) {
		return nil, refuse(RefuseInvalidName, fmt.Sprintf("%q is not a name of the form [A-Za-z_][A-Za-z0-9_]*", newName), nil)
	}
	if IsBuiltin(newName) {
		return nil, refuse(RefuseReserved, newName+" is a shell builtin or keyword", nil)
	}
	var files []int // the edited files, in path order
	for _, e := range rs.edits {
		if f := s.idx[e.Path]; len(files) == 0 || files[len(files)-1] != f {
			files = append(files, f)
		}
	}
	union := newBitset(len(s.paths)) // the files visible from any edited file
	for _, f := range files {
		union.or(s.visible[f])
	}
	if sites := s.renameConflicts(sym, newName, files, union, rs.edits); len(sites) > 0 {
		return nil, refuse(RefuseConflict, newName+" is already a name in the scope of this rename", sites)
	}
	if sites := s.recoveredSites(rs.edits); len(sites) > 0 {
		return nil, refuse(RefuseRecovered, "an edit site is in a region the parser had to repair", sites)
	}
	var skipped []Location
	union.each(func(f int) {
		for _, se := range s.edges[f] {
			if se.Skipped != "" {
				skipped = append(skipped, Location{Path: s.paths[f], Range: se.Ref.Range})
			}
		}
	})
	if len(skipped) > 0 {
		return nil, refuse(RefuseSkippedFile, "a file in view was skipped for its size and may use "+sym.Name, skipped)
	}

	plan := &RenamePlan{Symbol: sym, Old: sym.Name, New: newName, Excluded: rs.excl}
	plan.Edits = make([]RenameEdit, len(rs.edits))
	for i, e := range rs.edits {
		plan.Edits[i] = RenameEdit{Path: e.Path, Range: e.Range, NewText: newName}
	}
	for _, f := range files {
		if s.incomplete[f] {
			plan.Warnings = append(plan.Warnings, "graph incomplete: a dynamic source may call "+sym.Name)
			break
		}
	}
	for _, d := range sym.Decls {
		if d.Attrs.Exported {
			plan.Warnings = append(plan.Warnings, "exported: processes started from these scripts may read "+sym.Name)
			break
		}
	}
	var truncated bool
	plan.Others, truncated = s.sameNamed(sym)
	if truncated {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("others: only the first %d declarations of other symbols named %s are listed", maxRenameOthers, sym.Name))
	}
	return plan, nil
}

// sameNamed lists the declarations of the other symbols of sym's kind and
// name, sorted, at most maxRenameOthers of them (truncated reports a cut).
func (s *Snapshot) sameNamed(sym *Symbol) (out []Location, truncated bool) {
	var all []*Symbol
	switch sym.Kind {
	case DeclFunction:
		all = s.funcSyms[sym.Name]
	case DeclAlias:
		all = s.aliasSyms[sym.Name]
	case DeclVariable:
		all = s.varSyms[sym.Name]
	}
collect:
	for _, o := range all {
		if o == sym {
			continue
		}
		for _, d := range o.Decls {
			if len(out) == maxRenameOthers {
				truncated = true
				break collect
			}
			out = append(out, s.declLoc(d))
		}
	}
	slices.SortStableFunc(out, locCmp)
	return out, truncated
}

// renameConflicts lists the places where a symbol named newName would be
// confused with sym once renamed: a function or alias of that name in view
// (commands share one namespace), a variable of that name in view or read from
// the environment, a local of that name in a function that holds an edit of a
// global, and any other use of that name where a local is confined. edits is
// sorted.
func (s *Snapshot) renameConflicts(sym *Symbol, newName string, files []int, union bitset, edits []Location) []Location {
	var out []Location
	inView := func(syms []*Symbol) {
		for _, o := range syms {
			if o == sym {
				continue
			}
			for _, d := range o.Decls {
				if union.has(s.declFile[d]) {
					out = append(out, s.declLoc(d))
				}
			}
		}
	}
	if sym.Kind != DeclVariable {
		inView(s.funcSyms[newName])
		inView(s.aliasSyms[newName])
		return out
	}
	if sym.File != "" { // a local, or a variable private to a subshell
		return s.scopedVariableConflicts(sym, newName)
	}
	inView(s.varSyms[newName])
	if env := s.envSyms[newName]; env != nil { // reads of the environment would become reads of sym
		for _, b := range s.bySym[env] {
			if f, ok := s.idx[b.File]; ok && union.has(f) {
				out = append(out, Location{Path: b.File, Range: b.Use.Range})
			}
		}
	}
	for _, f := range files {
		for fn, names := range s.locals[f] {
			if !names[newName] {
				continue
			}
			from := Location{Path: s.paths[f], Range: tree.Range{Start: fn.Range.Start}}
			i, _ := slices.BinarySearchFunc(edits, from, locCmp)
			if i < len(edits) && edits[i].Path == from.Path && edits[i].Range.Start.Offset < fn.Range.End.Offset {
				if ls := s.localSyms[localKey{f, fn, newName}]; ls != nil {
					out = append(out, s.symbolDecls(ls)...)
				}
			}
		}
	}
	return out
}

// scopedVariableConflicts finds the uses of newName in the function (or, at
// file level, the file) that holds the variable sym is confined to: another
// local of that name, a global it would hide, or an environment read.
func (s *Snapshot) scopedVariableConflicts(sym *Symbol, newName string) []Location {
	file, ok := s.idx[sym.File]
	if !ok {
		return nil
	}
	region := funcBody(sym.Decls[0].Scope)
	var out []Location
	if region != nil {
		if ls := s.localSyms[localKey{file, region, newName}]; ls != nil && ls != sym {
			out = append(out, s.symbolDecls(ls)...)
		}
	}
	for _, u := range s.facts[file].Uses {
		if u.Kind != UseCall && u.Name == newName && funcBody(u.Scope) == region {
			out = append(out, Location{Path: sym.File, Range: u.Range})
		}
	}
	return out
}

// recoveredSites lists the edits that lie in or under a node the parser
// recovered, or an error gap. edits is sorted, so each file needs one Locator.
func (s *Snapshot) recoveredSites(edits []Location) []Location {
	var out []Location
	var loc *tree.Locator
	curPath := ""
	for _, e := range edits {
		if loc == nil || e.Path != curPath {
			f := s.facts[s.idx[e.Path]].File
			if f == nil || f.Root == nil {
				loc = nil
				continue
			}
			curPath, loc = e.Path, f.NewLocator()
		}
		for n := loc.At(e.Range.Start.Offset); n != nil; n = n.Parent {
			if n.Recovered || n.Kind == tree.KErrorGap {
				out = append(out, e)
				break
			}
		}
	}
	return out
}

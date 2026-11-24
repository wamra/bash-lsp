package analysis

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
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
	RefuseTooMany       = "too-many-sites"
	RefuseDynamicScope  = "dynamic-scope"
	RefuseNameReference = "name-reference"
	RefuseNoOp          = "no-op"
)

// Reasons of the refusals that have more than one, or that other code tests.
const (
	reasonGapAfterEdit    = "an edit site lies at or after a region the parser could not read"
	reasonGapMentionsName = "a region the parser could not read, in view of an edit, mentions the name"
	reasonRecovered       = "an edit site is in a region the parser had to repair"
	reasonDynamicScope    = "dynamic scope: a function that declares a local of this name reaches a use of the name, so the rename would change which variable the use reads"
	reasonDynamicCapture  = "dynamic scope: the new name would join the renamed variable to a local, or to a use, that a calling function sees under that name"
	reasonPrefixAssign    = "dynamic scope: a command with a prefix assignment of this name calls a function that reads or writes the name"
	reasonReachCap        = "dynamic scope: the call graph around the name is too large to check"
	reasonNameReference   = "the name is also written where the analysis finds no use of it (unset, type, declare -n, export -f, test -v, trap or eval strings, indirection)"
	reasonSpecialVar      = "the new name is a special shell variable"
	reasonCalledCommand   = "the new name is called as a command in view, so those calls would reach the renamed function"
	reasonSameName        = "the new name is the old name"
	reasonInexactName     = "inexact-name" // the text of a site is not exactly the name (an exclusion reason)
)

const (
	maxRenameSites  = 100000 // edits plus exclusions; above it the plan is refused with RefuseTooMany
	maxRenameOthers = 1000   // declarations listed in RenamePlan.Others
	maxRenameByName = 256    // same-named functions a rename by name tells apart
	maxRenameReach  = 20000  // call-graph nodes a dynamic-scope check visits
	maxRefusalSites = 100    // sites listed in one refusal
	// maxExcludedText is the most bytes of source text an exclusion keeps, cut on
	// a rune boundary. The text is raw: the encoder that sends it outward
	// redacts first and cuts after, so that a secret cut in half does not slip
	// past the redaction patterns.
	maxExcludedText = 4096
)

// renameCap names a bound of rename that a LimitsHit line reports.
type renameCap uint8

const (
	capOthers renameCap = iota
	capSites
	capText
	capByName
	capReach
)

// renameLims counts, per file, the rename bounds met on a snapshot.
type renameLims [capReach + 1]limCount

var renameCapFormats = [...]string{
	capOthers: "rename others cap: %d renames listed only the first %d declarations of other symbols with the same name",
	capSites:  "rename sites cap: %d refusals listed only the first %d sites",
	capText:   "rename text cap: %d excluded sites kept only the first %d bytes of their text",
	capByName: "rename by-name cap: %d renames by name met more than %d functions of one name and were refused",
	capReach:  "rename reach cap: %d dynamic-scope checks visited more than %d call-graph nodes and refused",
}

var renameCapBounds = [...]int{capOthers: maxRenameOthers, capSites: maxRefusalSites, capText: maxExcludedText,
	capByName: maxRenameByName, capReach: maxRenameReach}

// noteRenameCap records that a rename met bound c at line of file.
func (s *Snapshot) noteRenameCap(c renameCap, file, line int) {
	s.capMu.Lock()
	defer s.capMu.Unlock()
	if s.renLim == nil {
		s.renLim = map[int]*renameLims{}
	}
	l := s.renLim[file]
	if l == nil {
		l = &renameLims{}
		s.renLim[file] = l
	}
	l[c] = l[c].also(line)
}

// noteRenameCapAt is noteRenameCap for a place named by path.
func (s *Snapshot) noteRenameCapAt(c renameCap, path string, line int) {
	if f, ok := s.idx[path]; ok {
		s.noteRenameCap(c, f, line)
	}
}

// refuse makes a refusal, listing the sites in order, each place once, at
// most maxRefusalSites of them.
func (s *Snapshot) refuse(code, reason string, sites []Location) *RenameRefusal {
	slices.SortStableFunc(sites, locCmp)
	sites = slices.CompactFunc(sites, func(a, b Location) bool {
		return a.Path == b.Path && a.Range.Start.Offset == b.Range.Start.Offset
	})
	if len(sites) > maxRefusalSites {
		reason += fmt.Sprintf(" (first %d of %d sites)", maxRefusalSites, len(sites))
		s.noteRenameCapAt(capSites, sites[0].Path, sites[0].Range.Start.Line)
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

// siteText is the source text of rng in file.
func (s *Snapshot) siteText(file int, rng tree.Range) string {
	src := s.facts[file].File.Src
	a, b := rng.Start.Offset, rng.End.Offset
	if a < 0 || a > b || b > len(src) {
		return ""
	}
	return string(src[a:b])
}

// excludedText is siteText cut to maxExcludedText bytes on a rune boundary.
func (s *Snapshot) excludedText(file int, rng tree.Range) string {
	text := s.siteText(file, rng)
	if len(text) <= maxExcludedText {
		return text
	}
	s.noteRenameCap(capText, file, rng.Start.Line)
	cut := maxExcludedText
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// exactRange returns rng narrowed to old when the text there is old followed
// by the carriage return of a CRLF line ending; false when the text is not
// the name.
func (s *Snapshot) exactRange(file int, rng tree.Range, old string) (tree.Range, bool) {
	text := s.siteText(file, rng)
	switch {
	case text == old:
		return rng, true
	case text == old+"\r":
		return tree.Range{Start: rng.Start, End: s.facts[file].File.PosAt(rng.End.Offset - 1)}, true
	}
	return rng, false
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
			rng, exact := s.exactRange(file, u.Range, u.Name)
			text := s.siteText(file, rng)
			if exact && b.Sym.Name == text {
				add(rng, b.Sym)
			}
			if exact && b.Reason == ReasonAlias && u.Name == text {
				add(rng, s.pick(s.aliasSyms[text], file))
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
		return nil, s.refuse(RefuseNotSymbol, "the text at the cursor is not the name of a function, alias or variable of the workspace", nil)
	case 1:
		return &cands[0], nil
	}
	var sites []Location
	for _, c := range cands {
		sites = append(sites, s.symbolDecls(c.sym)...)
	}
	return nil, s.refuse(RefuseAmbiguous, fmt.Sprintf("the name at the cursor is bound to %d symbols", len(cands)), sites)
}

func (s *Snapshot) symbolDecls(sym *Symbol) []Location {
	out := make([]Location, 0, len(sym.Decls))
	for _, d := range sym.Decls {
		out = append(out, s.declLoc(d))
	}
	return out
}

func (s *Snapshot) candidateSiteRefusal(name string, site *renameSite) *RenameRefusal {
	return s.refuse(RefuseCandidateSite, "this site may or may not name "+name+" and is not renamed",
		[]Location{{Path: site.key.path, Range: site.rng}})
}

// PrepareRename returns the range of the name at the cursor and the text to
// show as its placeholder, or a *RenameRefusal when no rename can start there.
// It refuses what the cursor alone decides (not a symbol, ambiguous, a
// candidate site, too many sites); the checks that need the new name are
// PlanRename's.
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
		return tree.Range{}, "", s.candidateSiteRefusal(site.sym.Name, site)
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
// most files wins; a tie is refused, and so are more than maxRenameByName of
// them (told apart by position instead). The others are listed in Others.
func (s *Snapshot) PlanRenameByName(name, newName string) (*RenamePlan, error) {
	syms := s.funcSyms[name]
	if len(syms) == 0 {
		return nil, s.refuse(RefuseNotSymbol, "no function named "+name, nil)
	}
	if len(syms) > maxRenameByName {
		var sites []Location
		for _, sym := range syms {
			sites = append(sites, s.symbolDecls(sym)...)
		}
		first := sites[0]
		s.noteRenameCapAt(capByName, first.Path, first.Range.Start.Line)
		return nil, s.refuse(RefuseAmbiguous, fmt.Sprintf("%d functions are named %s, more than %d: rename one by its position", len(syms), name, maxRenameByName), sites)
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
		return nil, s.refuse(RefuseAmbiguous, fmt.Sprintf("%d functions named %s are equally widely visible", len(best), name), sites)
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
	seen  map[refKey]bool     // every place already claimed, as an edit, an exclusion or a skipped alias call
	n     int                 // edits plus exclusions
	excls map[refKey]bool     // the exclusions' places, made on first use
	spans map[string][][2]int // per path, the byte spans of edits and exclusions, made on first use
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
		return false, s.refuse(RefuseTooMany, fmt.Sprintf("more than %d sites to edit or confirm by hand", maxRenameSites), nil)
	}
	r.seen[k] = true
	return true, nil
}

// addEdit records an edit at rng of file when the text there is exactly old
// (up to a CRLF line end), and an exclusion otherwise. A place claimed before
// is left alone.
func (s *Snapshot) addEdit(r *renameSet, file int, rng tree.Range, old string) error {
	if ok, err := s.claim(r, file, rng); !ok || err != nil {
		return err
	}
	exact, ok := s.exactRange(file, rng, old)
	if !ok {
		r.excl = append(r.excl, RenameExclusion{Loc: Location{Path: s.paths[file], Range: rng}, Res: Resolved,
			Reason: reasonInexactName, Text: s.excludedText(file, rng)})
		return nil
	}
	r.edits = append(r.edits, Location{Path: s.paths[file], Range: exact})
	return nil
}

func (s *Snapshot) addExclusion(r *renameSet, file int, rng tree.Range, res Resolution, reason string) error {
	if ok, err := s.claim(r, file, rng); !ok || err != nil {
		return err
	}
	r.excl = append(r.excl, RenameExclusion{Loc: Location{Path: s.paths[file], Range: rng}, Res: res, Reason: reason, Text: s.excludedText(file, rng)})
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
			if b.Res != Resolved || u.Via != ViaNone {
				err = s.addExclusion(r, f, u.Range, b.Res, b.Reason)
			} else {
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
	rs, err := s.renameSites(sym)
	if err != nil {
		return nil, err
	}
	if cursor != nil && rs.isExcluded(cursor.key) {
		return nil, s.candidateSiteRefusal(sym.Name, cursor)
	}
	if !identRe.MatchString(newName) {
		return nil, s.refuse(RefuseInvalidName, fmt.Sprintf("%q is not a name of the form [A-Za-z_][A-Za-z0-9_]*", newName), nil)
	}
	if IsBuiltin(newName) {
		return nil, s.refuse(RefuseReserved, newName+" is a shell builtin or keyword", nil)
	}
	if newName == sym.Name {
		return nil, s.refuse(RefuseNoOp, reasonSameName, nil)
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
	if ref := s.renameConflicts(sym, newName, files, union, rs.edits); ref != nil {
		return nil, ref
	}
	if ref := s.dynamicScopeRefusal(sym, newName, rs, union); ref != nil {
		return nil, ref
	}
	if ref := s.unreadableRefusal(sym.Name, files, union, rs.edits); ref != nil {
		return nil, ref
	}
	if ref := s.nameReferenceRefusal(sym, union, rs); ref != nil {
		return nil, ref
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
		return nil, s.refuse(RefuseSkippedFile, "a file in view was skipped for its size and may use "+sym.Name, skipped)
	}

	plan := &RenamePlan{Symbol: sym, Old: sym.Name, New: newName, Excluded: rs.excl}
	plan.Edits = make([]RenameEdit, len(rs.edits))
	for i, e := range rs.edits {
		plan.Edits[i] = RenameEdit{Path: e.Path, Range: e.Range, NewText: newName}
	}
	plan.Warnings = s.incompleteWarnings(sym.Name, files, union)
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
		s.noteRenameCap(capOthers, s.declFile[sym.Decls[0]], sym.Decls[0].NameRange.Start.Line)
	}
	return plan, nil
}

// incompleteWarnings says why the graph around the edited files is incomplete
// when it is: a source with a dynamic path may reach any file, a source of a
// static path that was not found may name a file the workspace does not have.
func (s *Snapshot) incompleteWarnings(old string, files []int, union bitset) []string {
	incomplete := false
	for _, f := range files {
		if s.incomplete[f] {
			incomplete = true
			break
		}
	}
	if !incomplete {
		return nil
	}
	dynamic := false
	var missing []string
	union.each(func(f int) {
		for _, se := range s.edges[f] {
			if se.Status != SourceUnresolved || se.Skipped != "" {
				continue
			}
			text := ""
			if se.Ref.Word != nil {
				text = s.siteText(f, se.Ref.Word.Range)
			}
			if text == "" || strings.ContainsAny(text, "$`") {
				dynamic = true
			} else if !slices.Contains(missing, text) {
				missing = append(missing, text)
			}
		}
	})
	var out []string
	if dynamic || len(missing) == 0 {
		out = append(out, "graph incomplete: a dynamic source may call "+old)
	}
	slices.Sort(missing)
	switch {
	case len(missing) == 1:
		out = append(out, fmt.Sprintf("graph incomplete: the sourced file %s was not found and may call %s", missing[0], old))
	case len(missing) > 1:
		shown := missing[:min(len(missing), 3)]
		more := ""
		if len(missing) > len(shown) {
			more = ", ..."
		}
		out = append(out, fmt.Sprintf("graph incomplete: %d sourced files were not found (%s%s) and may call %s", len(missing), strings.Join(shown, ", "), more, old))
	}
	return out
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

// specialVariables are variables the shell itself reads or sets, beyond
// shellVariable: the version and clock variables the version rules know, the
// last-argument parameter, and RANDOM.
var specialVariables = map[string]bool{"_": true, "RANDOM": true, "BASH_VERSION": true, "BASH_VERSINFO": true,
	"EPOCHSECONDS": true, "EPOCHREALTIME": true}

func specialVariable(name string) bool {
	return shellVariable(name) || envVariables[name] || specialVariables[name]
}

// renameConflicts refuses when a symbol named newName would be confused with
// sym once renamed: a function or alias of that name in view (commands share
// one namespace), a command of that name called in view (a function renamed
// onto it would take over those calls), a variable of that name in view or
// read from the environment, a special shell variable, a local of that name
// in a function that holds an edit of a global, any other use of that name
// where a local is confined, and a local of that name, or a use of it, that
// dynamic scope would join to the renamed variable. edits is sorted.
func (s *Snapshot) renameConflicts(sym *Symbol, newName string, files []int, union bitset, edits []Location) *RenameRefusal {
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
	conflict := func(reason string) *RenameRefusal {
		return s.refuse(RefuseConflict, reason, out)
	}
	if sym.Kind != DeclVariable {
		inView(s.funcSyms[newName])
		inView(s.aliasSyms[newName])
		union.each(func(f int) {
			for _, u := range s.facts[f].Uses {
				if u.Kind == UseCall && u.Name == newName && u.Via == ViaNone && !u.ViaAlias {
					out = append(out, Location{Path: s.paths[f], Range: u.Range})
				}
			}
		})
		if len(out) == 0 {
			return nil
		}
		return conflict(newName + " is already a name in the scope of this rename, or a command called in view")
	}
	if specialVariable(newName) {
		return conflict(reasonSpecialVar)
	}
	if sym.File != "" { // a local, or a variable private to a subshell
		out = s.scopedVariableConflicts(sym, newName)
		if len(out) > 0 {
			return conflict(newName + " is already a name in the scope of this rename")
		}
		return nil
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
	if len(out) > 0 {
		return conflict(newName + " is already a name in the scope of this rename")
	}
	return nil
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

// scopeNode is the graph node whose code runs where scope sc is: the named
// function around it, else the file's top level.
func (s *Snapshot) scopeNode(file int, sc *Scope) Node {
	if d := s.callerOfScope(file, sc); d != nil {
		return Node{Path: s.paths[file], Func: s.declSym[d]}
	}
	return Node{Path: s.paths[file]}
}

// varAccessNodes returns the graph nodes that read or write the variable name
// in the files of in, where it is not shadowed by a local of that name in the
// same function: the nodes whose accesses dynamic scope may join to a local of
// a function that calls them.
func (s *Snapshot) varAccessNodes(name string, in bitset) map[Node]bool {
	out := map[Node]bool{}
	in.each(func(f int) {
		ff := s.facts[f]
		note := func(sc *Scope) {
			if fn := funcBody(sc); fn != nil && s.locals[f][fn][name] {
				return
			}
			out[s.scopeNode(f, sc)] = true
		}
		for _, u := range ff.Uses {
			if u.Kind != UseCall && u.Name == name {
				note(u.Scope)
			}
		}
	})
	return out
}

// localDeclarers returns, with the declaration places, the function nodes of
// in that declare name local.
func (s *Snapshot) localDeclarers(name string, in bitset) ([]Node, []Location) {
	var nodes []Node
	var sites []Location
	in.each(func(f int) {
		for fn, names := range s.locals[f] {
			if !names[name] {
				continue
			}
			d := s.funcDeclOf(f, fn)
			if d == nil {
				continue
			}
			nodes = append(nodes, Node{Path: s.paths[f], Func: s.declSym[d]})
			if ls := s.localSyms[localKey{f, fn, name}]; ls != nil {
				sites = append(sites, s.symbolDecls(ls)...)
			}
		}
	})
	return nodes, sites
}

// reachFrom returns every node the call graph reaches from starts (starts
// included), by resolved and candidate calls and sources; ok is false when
// the search met more than maxRenameReach nodes.
func (s *Snapshot) reachFrom(starts []Node) (map[Node]bool, bool) {
	g := s.adjacency()
	seen := make(map[Node]bool, len(starts))
	queue := append([]Node(nil), starts...)
	for _, n := range starts {
		seen[n] = true
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
func (s *Snapshot) reachesAny(starts []Node, targets map[Node]bool) (found, ok bool) {
	if len(starts) == 0 || len(targets) == 0 {
		return false, true
	}
	r, ok := s.reachFrom(starts)
	for n := range targets {
		if r[n] {
			return true, ok
		}
	}
	return false, ok
}

// dynamicScopeRefusal refuses a rename of a variable that changes what
// dynamic scope makes of it. A function's local is seen by everything that
// function calls, so a use reached through calls from a function that declares
// a local of the name reads that local, not the global. Renaming only one side
// moves the use from one variable to the other. Four cases:
//   - a global renamed, while a function with a local of the old name reaches
//     a use of the global;
//   - a global renamed onto the name of a local of a function that reaches one
//     of its uses (capture);
//   - a local renamed, while a function it calls (directly or through others)
//     reads or writes the old name, or the new one (capture);
//   - a prefix assignment of the name on a command that calls a function that
//     reads or writes it.
//
// Variables private to a subshell are not checked.
func (s *Snapshot) dynamicScopeRefusal(sym *Symbol, newName string, rs *renameSet, union bitset) *RenameRefusal {
	if sym.Kind != DeclVariable || (sym.File != "" && sym.Private != 0) {
		return nil
	}
	old := sym.Name
	accOld := s.varAccessNodes(old, union)
	cut := func(site Location) *RenameRefusal {
		s.noteRenameCapAt(capReach, site.Path, site.Range.Start.Line)
		return s.refuse(RefuseDynamicScope, reasonReachCap, []Location{site})
	}
	site0 := rs.edits[0]
	if sym.File == "" { // a global
		starts, sites := s.localDeclarers(old, union)
		if found, ok := s.reachesAny(starts, accOld); found {
			return s.refuse(RefuseDynamicScope, reasonDynamicScope, sites)
		} else if !ok {
			return cut(site0)
		}
		starts, sites = s.localDeclarers(newName, union)
		editNodes := map[Node]bool{}
		for _, e := range rs.edits {
			if f, ok := s.idx[e.Path]; ok {
				editNodes[s.nodeAt(f, e.Range.Start.Offset)] = true
			}
		}
		if found, ok := s.reachesAny(starts, editNodes); found {
			return s.refuse(RefuseConflict, reasonDynamicCapture, sites)
		} else if !ok {
			return cut(site0)
		}
	} else { // a local
		fn := funcBody(sym.Decls[0].Scope)
		file := s.idx[sym.File]
		if d := s.funcDeclOf(file, fn); fn != nil && d != nil {
			start := []Node{{Path: sym.File, Func: s.declSym[d]}}
			if found, ok := s.reachesAny(start, accOld); found {
				return s.refuse(RefuseDynamicScope, reasonDynamicScope, s.symbolDecls(sym))
			} else if !ok {
				return cut(site0)
			}
			if found, ok := s.reachesAny(start, s.varAccessNodes(newName, union)); found {
				return s.refuse(RefuseConflict, reasonDynamicCapture, s.symbolDecls(sym))
			} else if !ok {
				return cut(site0)
			}
		}
	}
	return s.prefixAssignmentRefusal(old, accOld, union, site0)
}

// nodeAt is the graph node whose code holds offset of file.
func (s *Snapshot) nodeAt(file, offset int) Node {
	c := s.CallerAt(s.paths[file], offset)
	return Node{Path: c.Path, Func: c.Func}
}

// prefixAssignmentRefusal refuses when `name=v cmd` appears in view and cmd is
// a function that reaches an access of name: the assignment reaches that
// access through the environment of the call, and no rename of the symbol
// edits it.
func (s *Snapshot) prefixAssignmentRefusal(name string, access map[Node]bool, union bitset, edit Location) *RenameRefusal {
	var sites []Location
	cutAt := Location{}
	union.each(func(f int) {
		ff := s.facts[f]
		for _, d := range ff.Decls {
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
			var starts []Node
			for _, b := range s.bindings[f] {
				if b.Use.Node == call && b.Use.Kind == UseCall && b.Sym != nil && b.Sym.Kind == DeclFunction {
					for _, fd := range b.Sym.Decls {
						starts = append(starts, Node{Path: s.paths[s.declFile[fd]], Func: b.Sym})
					}
				}
			}
			found, ok := s.reachesAny(starts, access)
			if !ok {
				cutAt = s.declLoc(d)
			}
			if found {
				sites = append(sites, s.declLoc(d))
			}
		}
	})
	if cutAt.Path != "" {
		s.noteRenameCapAt(capReach, cutAt.Path, cutAt.Range.Start.Line)
		return s.refuse(RefuseDynamicScope, reasonReachCap, []Location{cutAt})
	}
	if len(sites) > 0 {
		return s.refuse(RefuseDynamicScope, reasonPrefixAssign, sites)
	}
	return nil
}

// errorGaps lists the error gaps of file: the regions the parser cut out
// because it could not read them.
func (s *Snapshot) errorGaps(file int) []tree.Range {
	f := s.facts[file].File
	if f == nil || f.Root == nil || len(f.Errors) == 0 && len(f.Limits) == 0 {
		return nil
	}
	var out []tree.Range
	f.Root.Walk(func(n *tree.Node) bool {
		if n.Kind == tree.KErrorGap {
			out = append(out, n.Range)
		}
		return true
	})
	return out
}

// wordAt returns the offsets in text where word occurs as a whole word:
// not next to a letter, digit or underscore.
func wordAt(text []byte, word string) []int {
	var out []int
	w := []byte(word)
	isWord := func(c byte) bool {
		return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
	}
	for from := 0; from <= len(text)-len(w); {
		i := bytes.Index(text[from:], w)
		if i < 0 {
			break
		}
		at := from + i
		if (at == 0 || !isWord(text[at-1])) && (at+len(w) == len(text) || !isWord(text[at+len(w)])) {
			out = append(out, at)
		}
		from = at + 1
	}
	return out
}

// unreadableRefusal refuses a rename in the neighbourhood of a region the
// parser could not read: an edit at or after an error gap of its file (the
// code after a half-typed construct may be read as part of it), and an error
// gap anywhere in view whose text mentions the name (a use the analysis never
// saw). The edits are sorted. A region the parser repaired is refused when an
// edit lies in or under it.
func (s *Snapshot) unreadableRefusal(old string, files []int, union bitset, edits []Location) *RenameRefusal {
	var sites []Location
	var loc *tree.Locator
	curPath := ""
	for _, e := range edits {
		f := s.facts[s.idx[e.Path]].File
		if f == nil || f.Root == nil {
			continue
		}
		if loc == nil || e.Path != curPath {
			curPath, loc = e.Path, f.NewLocator()
		}
		for n := loc.At(e.Range.Start.Offset); n != nil; n = n.Parent {
			if n.Recovered {
				sites = append(sites, e)
				break
			}
		}
	}
	if len(sites) > 0 {
		return s.refuse(RefuseRecovered, reasonRecovered, sites)
	}
	for _, f := range files {
		gaps := s.errorGaps(f)
		if len(gaps) == 0 {
			continue
		}
		first := gaps[0].Start.Offset
		for _, g := range gaps {
			first = min(first, g.Start.Offset)
		}
		for _, e := range edits {
			if e.Path == s.paths[f] && e.Range.Start.Offset >= first {
				sites = append(sites, e)
			}
		}
	}
	if len(sites) > 0 {
		return s.refuse(RefuseRecovered, reasonGapAfterEdit, sites)
	}
	union.each(func(f int) {
		src := s.facts[f].File.Src
		for _, g := range s.errorGaps(f) {
			a, b := g.Start.Offset, g.End.Offset
			if a < 0 || a > b || b > len(src) {
				continue
			}
			if len(wordAt(src[a:b], old)) > 0 {
				sites = append(sites, Location{Path: s.paths[f], Range: g})
			}
		}
	})
	if len(sites) > 0 {
		return s.refuse(RefuseRecovered, reasonGapMentionsName, sites)
	}
	return nil
}

// covered reports whether offset of path lies in an edit or an exclusion.
func (r *renameSet) covered(path string, offset int) bool {
	if r.spans == nil {
		r.spans = map[string][][2]int{}
		for _, e := range r.edits {
			r.spans[e.Path] = append(r.spans[e.Path], [2]int{e.Range.Start.Offset, e.Range.End.Offset})
		}
		for _, x := range r.excl {
			r.spans[x.Loc.Path] = append(r.spans[x.Loc.Path], [2]int{x.Loc.Range.Start.Offset, x.Loc.Range.End.Offset})
		}
		for _, sp := range r.spans {
			slices.SortFunc(sp, func(a, b [2]int) int { return a[0] - b[0] })
		}
	}
	sp := r.spans[path]
	i, _ := slices.BinarySearchFunc(sp, offset, func(e [2]int, off int) int { return e[0] - off })
	// the span that starts at offset, or the last one before it
	if i < len(sp) && sp[i][0] == offset {
		return true
	}
	return i > 0 && offset < sp[i-1][1]
}

// nameReferenceRefusal refuses when the files in view write the name where the
// binder finds no use of it, and the rename would leave that text behind:
// the arguments of unset, type, `command -v`, `test -v` and `[[ -v ]]`, of
// declare, typeset, local, readonly and export with -n, -f or -F, the strings
// of trap and eval, and, for a variable, a literal value equal to the name in
// a file that expands indirectly (`${!ref}`) or declares namerefs. A place that
// an edit or an exclusion covers is already accounted for.
func (s *Snapshot) nameReferenceRefusal(sym *Symbol, union bitset, rs *renameSet) *RenameRefusal {
	old := sym.Name
	var sites []Location
	union.each(func(f int) {
		ff := s.facts[f]
		if ff.File == nil || ff.File.Root == nil {
			return
		}
		src := ff.File.Src
		path := s.paths[f]
		note := func(from, to int) {
			if from < 0 || to > len(src) || from > to {
				return
			}
			for _, at := range wordAt(src[from:to], old) {
				off := from + at
				if rs.covered(path, off) {
					continue
				}
				sites = append(sites, Location{Path: path, Range: tree.Range{Start: ff.File.PosAt(off), End: ff.File.PosAt(off + len(old))}})
			}
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
				s.noteCallReferences(src, n, note)
			case tree.KDecl:
				s.noteDeclReferences(src, n, note)
			}
			return true
		})
		if sym.Kind == DeclVariable {
			nameref := false
			for _, d := range ff.Decls {
				nameref = nameref || d.Attrs.Nameref
			}
			if indirect || nameref {
				for _, d := range ff.Decls {
					if d.Kind == DeclVariable && d.Literal == old && !rs.covered(path, d.NameRange.Start.Offset) {
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

// noteCallReferences notes the name-bearing arguments of the commands whose
// arguments name functions or variables without being uses.
func (s *Snapshot) noteCallReferences(src []byte, n *tree.Node, note func(from, to int)) {
	words := wordSpans(n)
	if len(words) < 2 {
		return
	}
	cmd := spanText(src, words[0])
	args := words[1:]
	hasFlag := func(flags ...string) bool {
		for _, a := range args {
			if slices.Contains(flags, spanText(src, a)) {
				return true
			}
		}
		return false
	}
	all := false
	switch cmd {
	case "unset", "type", "trap", "eval":
		all = true
	case "command":
		all = hasFlag("-v", "-V")
	case "test", "[":
		all = hasFlag("-v")
	}
	if all {
		for _, a := range args {
			if t := spanText(src, a); !isOptionWord(t) {
				note(a[0], a[1])
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
// functions or namerefs: -n, -f and -F.
func (s *Snapshot) noteDeclReferences(src []byte, n *tree.Node, note func(from, to int)) {
	named := false
	for _, c := range n.Children {
		t := spanText(src, [2]int{c.Range.Start.Offset, c.Range.End.Offset})
		if strings.HasPrefix(t, "-") && strings.ContainsAny(t[1:], "nfF") {
			named = true
		}
	}
	if !named {
		return
	}
	for _, c := range n.Children {
		t := spanText(src, [2]int{c.Range.Start.Offset, c.Range.End.Offset})
		if !strings.HasPrefix(t, "-") {
			note(c.Range.Start.Offset, c.Range.End.Offset)
		}
	}
}

// noteTestV notes the operand of -v in a `[[ ]]` test.
func (s *Snapshot) noteTestV(src []byte, n *tree.Node, note func(from, to int)) {
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
			note(a+toks[k+1][0], a+toks[k+1][1])
		}
	}
}

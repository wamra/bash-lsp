package analysis

import (
	"bytes"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/wamra/bash-lsp/tree"
)

// TextEdit replaces Range with NewText.
type TextEdit struct {
	Range   tree.Range
	NewText string
}

// Fix is a change that resolves a finding. Safe means applying it cannot
// change what the script does beyond what the finding describes, so a "fix
// all" may apply it without review.
type Fix struct {
	Title string
	Edits []TextEdit
	Safe  bool
}

// Diagnostic is one finding. Origin is "bash-lsp" for native rules and
// "shellcheck" for findings of the ShellCheck bridge. Unnecessary marks code
// that is never used (an editor may fade it).
type Diagnostic struct {
	Rule        string
	Severity    Severity
	Range       tree.Range
	Message     string
	Origin      string
	Unnecessary bool
	Fixes       []Fix

	codes []string // the ShellCheck codes of this finding when narrower than its rule's map
}

// RuleInfo describes a native rule. ShellCheck lists the ShellCheck codes
// that report the same problem; an entry ending in "*" is a prefix.
type RuleInfo struct {
	ID, Name   string
	Default    Severity
	ShellCheck []string
	Summary    string
}

// DiagOptions are the settings diagnostics depend on: the bash versions the
// scripts target, the rules turned off and per-rule severities. A zero
// Target means DefaultTarget. Only Target.Min drives WB1301 (a feature is
// reported when its first version is above it); Target.Max is validated and
// carried but no rule reads it yet, and is kept for rules about what newer
// bash changes or removes.
type DiagOptions struct {
	Roots    []string // every workspace folder (absolute); the snapshot's root counts too
	Target   VersionRange
	Disabled map[string]bool
	Severity map[string]Severity
}

// OriginNative is the Origin of every finding Diagnostics returns.
const OriginNative = "bash-lsp"

// Diagnostics returns the native findings of path, after suppression
// directives (`# bash-lsp disable=` and, for rules with a ShellCheck code,
// `# shellcheck disable=`) and opt, sorted by offset and then rule. Never nil.
func (s *Snapshot) Diagnostics(path string, opt DiagOptions) []Diagnostic {
	out := []Diagnostic{}
	file, ok := s.idx[path]
	if !ok {
		return out
	}
	ff := s.facts[file]
	if ff == nil || ff.File == nil {
		return out
	}
	c := &diagCtx{s: s, file: file, ff: ff, f: ff.File, target: opt.Target, disabled: opt.Disabled, extraRoots: opt.Roots}
	if c.target == (VersionRange{}) {
		c.target = DefaultTarget // no target given: the default range
	}
	c.run()
	for _, d := range suppress(ff.File, c.out) {
		if opt.Disabled[d.Rule] {
			continue
		}
		if sev, ok := opt.Severity[d.Rule]; ok {
			d.Severity = sev
		}
		out = append(out, d)
	}
	out, cut := capFindings(c.f, out, opt.Disabled["WB1007"])
	s.noteCapped(file, cut)
	s.noteRunLimits(file, c.runLim)
	sortDiagnostics(out)
	return out
}

// maxFindings is the most findings Diagnostics returns for one file. Past
// it the most severe are kept (then the first in the file) and one WB1007
// says how many were cut: a generated 2 MiB file can otherwise give a
// quarter of a million findings, 30 MB of JSON per edit.
const maxFindings = 2000

// capFindings returns at most maxFindings of ds and how many it cut.
func capFindings(f *tree.File, ds []Diagnostic, quiet bool) ([]Diagnostic, int) {
	if len(ds) <= maxFindings {
		return ds, 0
	}
	slices.SortStableFunc(ds, func(a, b Diagnostic) int {
		if a.Severity != b.Severity {
			return int(a.Severity) - int(b.Severity)
		}
		return a.Range.Start.Offset - b.Range.Start.Offset
	})
	cut := len(ds) - maxFindings
	ds = ds[:maxFindings]
	if !quiet {
		at := f.PosAt(0)
		ds = append(ds, Diagnostic{Rule: "WB1007", Severity: ruleByID["WB1007"].Default, Range: tree.Range{Start: at, End: at},
			Message: fmt.Sprintf("%d more findings not shown", cut), Origin: OriginNative})
	}
	return ds, cut
}

// noteCapped records that Diagnostics cut cut findings of file (LimitsHit); a
// run that cut none clears the file's entry.
func (s *Snapshot) noteCapped(file, cut int) {
	s.capMu.Lock()
	defer s.capMu.Unlock()
	if cut == 0 {
		delete(s.capped, file)
		return
	}
	if s.capped == nil {
		s.capped = map[int]int{}
	}
	s.capped[file] = cut
}

// runLimits are the search budgets one Diagnostics run of a file passed: the
// caller-local reachability search of WB1401 and the version-guard search of
// WB1301, each with the line of the first check it left undecided.
type runLimits struct{ reach, guard limCount }

// noteRunLimits records what the last Diagnostics run of file passed; a run
// that passed none clears the file's entry.
func (s *Snapshot) noteRunLimits(file int, r runLimits) {
	s.capMu.Lock()
	defer s.capMu.Unlock()
	if r == (runLimits{}) {
		delete(s.runLim, file)
		return
	}
	if s.runLim == nil {
		s.runLim = map[int]runLimits{}
	}
	s.runLim[file] = r
}

func sortDiagnostics(ds []Diagnostic) {
	slices.SortStableFunc(ds, func(a, b Diagnostic) int {
		if d := a.Range.Start.Offset - b.Range.Start.Offset; d != 0 {
			return d
		}
		return strings.Compare(a.Rule, b.Rule)
	})
}

// Merge combines native findings with ShellCheck's. A native finding is
// dropped when a ShellCheck finding whose code its rule maps to overlaps its
// range. The result is sorted like Diagnostics and never nil.
func Merge(native, sc []Diagnostic) []Diagnostic {
	out := make([]Diagnostic, 0, len(native)+len(sc))
	out = append(out, sc...)
	for _, n := range native {
		info, ok := ruleByID[n.Rule]
		codes := info.ShellCheck
		if n.codes != nil {
			codes = n.codes
		}
		dup := false
		if ok && len(codes) > 0 {
			for _, d := range sc {
				if mapsTo(codes, d.Rule) && overlaps(n.Range, d.Range) {
					dup = true
					break
				}
			}
		}
		if !dup {
			out = append(out, n)
		}
	}
	sortDiagnostics(out)
	return out
}

// mapsTo reports whether code matches an entry of codes (exactly, or by
// prefix for an entry ending in "*").
func mapsTo(codes []string, code string) bool {
	for _, c := range codes {
		if p, ok := strings.CutSuffix(c, "*"); ok && strings.HasPrefix(code, p) || c == code {
			return true
		}
	}
	return false
}

// overlaps reports whether two ranges share a byte; an empty range overlaps
// a range that holds its offset (ends included).
func overlaps(a, b tree.Range) bool {
	as, ae, bs, be := a.Start.Offset, a.End.Offset, b.Start.Offset, b.End.Offset
	if as == ae || bs == be {
		return (as >= bs && as <= be) || (bs >= as && bs <= ae)
	}
	return as < be && bs < ae
}

// diagCtx computes the findings of one file.
type diagCtx struct {
	s    *Snapshot
	file int
	ff   *FileFacts
	f    *tree.File
	scan *fileScan
	out  []Diagnostic

	target     VersionRange    // the bash versions the file must run on (WB1301)
	disabled   map[string]bool // rules turned off: they do not run
	extraRoots []string        // workspace folders besides the snapshot's root

	oneWords      map[*Symbol]bool    // WB1401: symbols whose every value is one word, on first need
	graph         *callGraph          // calls between the functions in view, on first need
	lreach        *localReach         // which functions a name's locals may reach by calls, on first need
	unresolvedFns map[*tree.Node]bool // functions of the file that call a name resolving to nothing in view, on first need

	runLim runLimits // search budgets this run passed (noteRunLimits)

	union     *nameUnion      // what the file's visibility reads or names, built on first need
	wantReads map[string]bool // the names WB1202 asks readByName about (nil: all)
}

func (c *diagCtx) add(rule string, r tree.Range, msg string) *Diagnostic {
	c.out = append(c.out, Diagnostic{Rule: rule, Severity: ruleByID[rule].Default, Range: r, Message: msg, Origin: OriginNative})
	return &c.out[len(c.out)-1]
}

// enabled reports whether any of ids is on; it tells ruleRunHook which.
func (c *diagCtx) enabled(ids ...string) bool {
	on := false
	for _, id := range ids {
		if !c.disabled[id] {
			on = true
			if ruleRunHook != nil {
				ruleRunHook(id)
			}
		}
	}
	return on
}

// run computes the findings of the rules that are on; a rule turned off in
// the settings costs nothing.
func (c *diagCtx) run() {
	if c.f.Skipped != "" {
		// A file skipped for its nesting keeps the syntax error that says so.
		if c.enabled("WB1002") {
			c.add("WB1002", tree.Range{Start: c.f.PosAt(0), End: c.f.PosAt(0)}, c.f.Skipped)
		}
		if c.f.Root != nil && c.enabled("WB1001", "WB1003", "WB1005") {
			c.syntax()
		}
		return
	}
	steps := []struct {
		ids []string
		fn  func()
	}{
		{[]string{"WB1001", "WB1003", "WB1005"}, c.syntax},
		{[]string{"WB1006"}, c.stringBudget},
		{[]string{"WB1008"}, c.directiveIDs},
		{[]string{"WB1101"}, c.sources},
		{[]string{"WB1102"}, c.commands},
		{[]string{"WB1103", "WB1104"}, c.functions},
		{[]string{"WB1105"}, c.localOutside},
		{[]string{"WB1106"}, c.lostAssignments},
		{[]string{"WB1201"}, c.unusedFunctions},
		{[]string{"WB1202"}, c.unusedVariables},
		{[]string{"WB1401"}, c.quoting},
		{[]string{"WB1301", "WB1302"}, c.versions},
	}
	for _, st := range steps {
		if c.enabled(st.ids...) {
			if c.scan == nil {
				c.scan = c.s.scanOf(c.file)
			}
			st.fn()
		}
	}
}

// --- the per-file scan: one walk of the tree collects every rule's candidates ---

// fileScan is what one walk over a file's tree collects for the rules. It
// depends only on the file's facts; a snapshot keeps the scans it made.
type fileScan struct {
	shebang    bool
	topCommand bool         // a top-level statement other than a definition, assignment, declaration or source, and no top-level return
	topReturn  bool         // `return` outside every function: the file is meant to be sourced
	hasGap     bool         // the tree holds an error gap: code that was not analysed
	topFuncs   []*tree.Node // function definitions that are top-level statements
	funcs      []*tree.Node // every function definition
	wrappers   map[*tree.Node]bool
	locals     []*tree.Node        // `local` commands outside every function
	quotes     []*tree.Node        // unquoted parameter expansions in arguments of file-mutating commands
	indirect   bool                // `${!` appears: a variable may be read through another's value
	nameref    bool                // some declaration is a nameref
	lastpipe   bool                // `lastpipe` appears (shopt -s lastpipe): the last pipeline stage may run in the shell
	localNames map[string]bool     // names some function of the file declares local
	unsetVars  map[string]bool     // names an `unset` of the file names (as a variable or function)
	setsIFS    bool                // the file assigns IFS for the shell (not only for one command or a function's local)
	localIFS   map[*tree.Node]bool // functions that declare `local IFS`
	version    versionScan         // evidence for the version rules (versionrules.go)
}

// fileMentions is what a file names without a binding: words that may name
// a function, and variables read by name (in strings, `-v NAME`, `declare -p
// NAME`). A second walk, made only for files whose visibility holds a
// candidate for an unused finding.
type fileMentions struct {
	dollar    map[string]bool // every `$name` of the text, `-v name`, `declare -p name`, arithmetic operands in `[[ ]]` and `$[ ]`
	reads     map[string]bool // dollar, plus the name of every read and every `"$name"` call, whatever it binds to
	allexport bool            // `set -a` or `set -o allexport`: assignments are exported
}

// diagState holds the scans and mentions Diagnostics made, by file facts.
// Both depend only on the facts, which a snapshot built from the previous one
// shares for every file that did not change: Build carries the entries of
// those files over (carryDiag), so an edit rescans only the edited file.
type diagState struct {
	capMu  sync.Mutex
	capped map[int]int         // file -> findings Diagnostics cut past maxFindings (LimitsHit)
	runLim map[int]runLimits   // file -> search budgets the last Diagnostics run passed (LimitsHit)
	renLim map[int]*renameLims // file -> rename bounds met on this snapshot (LimitsHit)

	pathOnce  sync.Once
	pathFiles bitset // files that assign PATH (WB1102)

	diagMu   sync.Mutex
	scans    map[*FileFacts]*fileScan
	mentions map[*FileFacts]*fileMentions
	words    map[*FileFacts]map[string]bool // literal argument words and string tokens; names after `export -f`, `declare -F`, ...
}

// carryDiag copies from prev the scans and mentions of the facts s still has.
func (s *Snapshot) carryDiag(prev *Snapshot) {
	prev.diagMu.Lock()
	defer prev.diagMu.Unlock()
	if len(prev.scans) == 0 && len(prev.mentions) == 0 && len(prev.words) == 0 {
		return
	}
	s.scans = map[*FileFacts]*fileScan{}
	s.mentions = map[*FileFacts]*fileMentions{}
	s.words = map[*FileFacts]map[string]bool{}
	for _, ff := range s.facts {
		if sc := prev.scans[ff]; sc != nil {
			s.scans[ff] = sc
		}
		if m := prev.mentions[ff]; m != nil {
			s.mentions[ff] = m
		}
		if w := prev.words[ff]; w != nil {
			s.words[ff] = w
		}
	}
}

// loopLoaded reports whether this file may be one that an unresolved
// `source` of a loop variable loads (`for p in plugins/*.sh; do . "$p";
// done` whose pattern matched nothing analysable). Per loop word: a
// directory outside every workspace folder (`$HOME`, `~`, an absolute path
// elsewhere, such as /etc/profile.d) loads none of them; a literal pattern
// loads the files it matches, taken from the loading script's directory and
// from each folder; anything computed may load any script.
func (c *diagCtx) loopLoaded() bool {
	s := c.s
	path := s.paths[c.file]
	for i, es := range s.edges {
		var src []byte
		var dir string
		for _, e := range es {
			if e.Status != SourceUnresolved || e.Ref.loop == nil {
				continue
			}
			if src == nil {
				src, dir = s.facts[i].File.Src, filepath.Dir(s.paths[i])
			}
			words := 0
			for _, w := range e.Ref.loop.Children {
				if w.Kind != tree.KWord {
					continue
				}
				words++
				if c.loopWordLoads(w, src, dir, path) {
					return true
				}
			}
			if words == 0 { // `for p; do`: the arguments, anything
				return true
			}
		}
	}
	return false
}

func (c *diagCtx) loopWordLoads(w *tree.Node, src []byte, dir, path string) bool {
	text := ""
	if a, b := w.Range.Start.Offset, w.Range.End.Offset; a >= 0 && a <= b && b <= len(src) {
		text = strings.TrimLeft(string(src[a:b]), `"'`)
	}
	if strings.HasPrefix(text, "~") || strings.HasPrefix(text, "$HOME") || strings.HasPrefix(text, "${HOME}") {
		return false
	}
	lit, ok := literalText(w)
	if !ok {
		return true
	}
	var pats []string
	if filepath.IsAbs(lit) {
		if !c.inWorkspace(lit) {
			return false
		}
		pats = []string{filepath.Clean(lit)}
	} else {
		pats = append(pats, filepath.Join(dir, lit))
		for _, r := range c.roots() {
			pats = append(pats, filepath.Join(r, lit))
		}
	}
	for _, p := range pats {
		if m, _ := filepath.Match(p, path); m {
			return true
		}
	}
	return false
}

// roots are the workspace folders: the snapshot's root and DiagOptions.Roots.
func (c *diagCtx) roots() []string {
	out := make([]string, 0, 1+len(c.extraRoots))
	if c.s.root != "" {
		out = append(out, c.s.root)
	}
	return append(out, c.extraRoots...)
}

// inWorkspace reports whether absolute path p lies in a workspace folder.
func (c *diagCtx) inWorkspace(p string) bool {
	p = filepath.Clean(p)
	for _, r := range c.roots() {
		if rel, err := filepath.Rel(r, p); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// scanOf returns the scan of file, made on first use.
func (s *Snapshot) scanOf(file int) *fileScan {
	ff := s.facts[file]
	s.diagMu.Lock()
	sc := s.scans[ff]
	s.diagMu.Unlock()
	if sc != nil {
		return sc
	}
	sc = scanFile(ff)
	s.diagMu.Lock()
	if s.scans == nil {
		s.scans = map[*FileFacts]*fileScan{}
	}
	s.scans[ff] = sc
	s.diagMu.Unlock()
	return sc
}

// mentionsOf returns the mentions of file, made on first use.
func (s *Snapshot) mentionsOf(file int) *fileMentions {
	ff := s.facts[file]
	s.diagMu.Lock()
	m := s.mentions[ff]
	s.diagMu.Unlock()
	if m != nil {
		return m
	}
	m = &fileMentions{dollar: map[string]bool{}}
	if ff != nil && ff.File != nil {
		// Every `$name` of the code, read or inside a string a command may
		// evaluate: an over-count (a read bound elsewhere counts too) that
		// needs no walk. The forms only a walk finds are rare; walk only when
		// their text appears.
		codeDollarNames(m.dollar, ff.File, nil)
		if mentionWalkNeeded(ff.File.Src) {
			walkMentions(ff, &scanWalker{m: m})
		}
	}
	m.reads = make(map[string]bool, len(m.dollar))
	for k := range m.dollar {
		m.reads[k] = true
	}
	if ff != nil {
		for _, u := range ff.Uses {
			if u.Kind == UseVarRead || u.Kind == UseCall && u.Via == ViaVariable {
				m.reads[u.Name] = true
			}
		}
	}
	s.diagMu.Lock()
	if s.mentions == nil {
		s.mentions = map[*FileFacts]*fileMentions{}
	}
	s.mentions[ff] = m
	s.diagMu.Unlock()
	return m
}

// mentionWalkNeeded reports text that the forms only a walk recognises
// contain: an option cluster of declaration, `set` or test letters holding
// v, p or a (`-v`, `-xp`, `set -ea`), an arithmetic comparison in `[[ ]]`
// (`-eq` and the others), `$[ ]` and `allexport`. A cluster starts a word
// (`my-tool`, `--parallel` and `-type` are none). One pass over the dashes.
func mentionWalkNeeded(src []byte) bool {
	if bytes.Contains(src, []byte("$[")) || bytes.Contains(src, []byte("allexport")) {
		return true
	}
	for i := bytes.IndexByte(src, '-'); i >= 0 && i+1 < len(src); {
		if i == 0 || clusterStart[src[i-1]] {
			j := i + 1
			for j < len(src) && optionLetter[src[j]] {
				j++
			}
			end := j == len(src) || clusterEnds(src, j)
			if run := src[i+1 : j]; len(run) > 0 && end && (bytes.ContainsAny(run, "vpa") || arithOps[string(src[i:j])]) {
				return true
			}
		}
		k := bytes.IndexByte(src[i+1:], '-')
		if k < 0 {
			break
		}
		i += 1 + k
	}
	return false
}

// optionLetter holds the letters of declaration (`declare -aAfFgilnprtux`),
// `set` (`-abefhkmnptuvxBCEHPT`) and test (`-eq`, `-gt`, …) options.
var optionLetter = func() (t [256]bool) {
	for _, c := range "aAbBCeEfFghHiklmnpPqrtTuvx" {
		t[c] = true
	}
	return t
}()

// clusterStart and clusterEnd hold the bytes that may stand before and
// after an option cluster: blanks, `;`, a quote (`set "-a"`), and after it
// `]` and a redirect or pipe (`set -a>/dev/null`). Not `)`: `(set -a)` and
// `$(set -a)` change no option of the shell, and the walk does not tell a
// subshell apart.
var clusterStart, clusterEnd = func() (s, e [256]bool) {
	for _, c := range " \t\n;\"'" {
		s[c], e[c] = true, true
	}
	for _, c := range "]<>|&" {
		e[c] = true
	}
	return s, e
}()

// clusterEnds reports whether the option cluster that stops at src[j] ends
// there, as a word of `set` or a declaration does. A pipe or a background
// `&` after it (`set -a | cat`, `set -a &`, with blanks or without) is no
// end: the command runs in a subshell, so its options reach no later
// command. `||`, `&&` and `&>` (a redirect) end it as before.
func clusterEnds(src []byte, j int) bool {
	if !clusterEnd[src[j]] {
		return false
	}
	for j < len(src) && (src[j] == ' ' || src[j] == '\t') {
		j++
	}
	if j >= len(src) {
		return true
	}
	next := byte(0)
	if j+1 < len(src) {
		next = src[j+1]
	}
	switch src[j] {
	case '|':
		return next == '|'
	case '&':
		return next == '&' || next == '>'
	}
	return true
}

// runsInSubshell reports whether the command n runs in a subshell of the
// shell that reads the file: a stage of a pipeline, a subshell, a command or
// process substitution, a coprocess, or a statement followed by `&`. The
// options it sets (`set -a`) reach no later command.
func runsInSubshell(n *tree.Node, src []byte) bool {
	var stmt *tree.Node
walk:
	for p := n.Parent; p != nil; p = p.Parent {
		switch p.Kind {
		case tree.KSubshell, tree.KPipeline, tree.KCmdSubst, tree.KProcSubst, tree.KCoproc:
			return true
		case tree.KStmt:
			if stmt == nil {
				stmt = p
			}
		case tree.KFuncDecl, tree.KFile:
			break walk
		}
	}
	if stmt == nil {
		return false
	}
	// `&` after the statement: the parser's range ends before it
	i := stmt.Range.End.Offset
	for i >= 0 && i < len(src) && (src[i] == ' ' || src[i] == '\t') {
		i++
	}
	return i >= 0 && i < len(src) && src[i] == '&' && (i+1 >= len(src) || src[i+1] != '&' && src[i+1] != '>')
}

// wordsOf returns the words of file (fileMentions describes them), made on
// first use by their own walk: only WB1201 needs them.
func (s *Snapshot) wordsOf(file int) map[string]bool {
	ff := s.facts[file]
	s.diagMu.Lock()
	ws := s.words[ff]
	s.diagMu.Unlock()
	if ws != nil {
		return ws
	}
	ws = map[string]bool{}
	walkMentions(ff, &scanWalker{words: ws})
	s.diagMu.Lock()
	if s.words == nil {
		s.words = map[*FileFacts]map[string]bool{}
	}
	s.words[ff] = ws
	s.diagMu.Unlock()
	return ws
}

func walkMentions(ff *FileFacts, w *scanWalker) {
	if ff == nil || ff.File == nil || ff.File.Root == nil {
		return
	}
	w.src = ff.File.Src
	for _, st := range ff.File.Root.Children {
		w.visit(st, nil, false)
	}
}

// scanFile collects the rule candidates of a file from its facts (its
// declarations and command uses) and the top level of its tree: no walk of
// the whole tree.
func scanFile(ff *FileFacts) *fileScan {
	sc := &fileScan{wrappers: map[*tree.Node]bool{}}
	if ff == nil || ff.File == nil || ff.File.Root == nil {
		return sc
	}
	f := ff.File
	src := f.Src
	sc.shebang = len(src) >= 2 && src[0] == '#' && src[1] == '!'
	sc.indirect = bytes.Contains(src, []byte("${!"))
	sc.lastpipe = bytes.Contains(src, []byte("lastpipe"))
	for _, st := range f.Root.Children {
		if st.Kind == tree.KErrorGap {
			sc.hasGap = true
			continue
		}
		if isTopCommand(st) {
			sc.topCommand = true
		}
		if st.Kind == tree.KStmt {
			for _, k := range st.Children {
				if k.Kind == tree.KFuncDecl && k.Name != "" {
					sc.topFuncs = append(sc.topFuncs, k)
				}
			}
		}
	}
	var lastLocal *tree.Node
	var lastVersionDecl [3]*tree.Node
	var ifsDecls []*Decl
	for _, d := range ff.Decls {
		if d.Kind == DeclVariable && (d.Attrs.Assoc || d.Attrs.Global || d.Attrs.Nameref) {
			sc.version.decl(d, &lastVersionDecl)
		}
		switch {
		case d.Kind == DeclFunction && d.Node != nil:
			sc.funcs = append(sc.funcs, d.Node)
		case d.Kind == DeclVariable:
			sc.nameref = sc.nameref || d.Attrs.Nameref
			if d.Name == "IFS" && !d.Temp {
				ifsDecls = append(ifsDecls, d)
			}
			if d.Attrs.Local {
				if sc.localNames == nil {
					sc.localNames = map[string]bool{}
				}
				sc.localNames[d.Name] = true
			}
			if d.Attrs.Local && d.Scope != nil && d.Scope.Func == "" && d.Node != nil {
				if p := d.Node.Parent; p != nil && p.Kind == tree.KDecl && p.Name == "local" && p != lastLocal {
					sc.locals = append(sc.locals, p)
					lastLocal = p
				}
			}
		}
	}
	// IFS: a function with `local IFS` changes it for itself and what it
	// calls; any other assignment (not a one-command prefix, not a bare
	// `export IFS`), by `=` or by a builtin that writes it (`read IFS`,
	// `for IFS in`, `printf -v IFS`, `mapfile`, `getopts`, arithmetic),
	// changes it everywhere.
	for _, d := range ifsDecls {
		if fn := funcBody(d.Scope); fn != nil && d.Attrs.Local {
			if sc.localIFS == nil {
				sc.localIFS = map[*tree.Node]bool{}
			}
			sc.localIFS[fn] = true
		}
	}
	for _, d := range ifsDecls {
		i := d.NameRange.End.Offset
		assigns := d.Implicit != "" || i >= 0 && i < len(src) && (src[i] == '=' || src[i] == '+')
		if fn := funcBody(d.Scope); assigns && !d.Attrs.Local && (fn == nil || !sc.localIFS[fn]) {
			sc.setsIFS = true
		}
	}
	sc.version.src = src
	var words []*tree.Node
	for _, u := range ff.Uses {
		if u.Kind == UseVarRead {
			switch u.Name {
			case "EPOCHSECONDS", "EPOCHREALTIME":
				sc.version.epoch = append(sc.version.epoch, u)
			case "BASH_VERSION", "BASH_VERSINFO":
				sc.version.read(u)
			}
		}
		if u.Kind != UseCall || u.Via != ViaNone || u.ViaAlias || u.Node == nil || u.Node.Kind != tree.KCall {
			continue
		}
		if u.Name == "return" && namedFunc(u.Scope) == nil {
			sc.topReturn = true
		}
		if !inspected[u.Name] {
			continue
		}
		words = words[:0]
		for _, c := range u.Node.Children {
			if c.Kind == tree.KWord {
				words = append(words, c)
			}
		}
		if len(words) > 0 {
			sc.command(u, words, src)
		}
	}
	sc.version.finish(ff.Uses)
	if sc.topReturn || dualUse(src) {
		sc.topCommand = false
	}
	return sc
}

// dualUse reports the guard of a file meant to be both run and sourced,
// `[[ "${BASH_SOURCE[0]}" == "$0" ]]` and its spellings: like a top-level
// `return`, it marks a library.
func dualUse(src []byte) bool {
	return bytes.Contains(src, []byte("BASH_SOURCE")) && (bytes.Contains(src, []byte(`"$0"`)) || bytes.Contains(src, []byte(`${0}`)) || bytes.Contains(src, []byte(` $0 `)))
}

// inspected are the command names scanFile looks into: the file-changing
// commands and the prefixes before them, and builtin wrappers.
var inspected = func() map[string]bool {
	m := map[string]bool{"sudo": true, "command": true, "builtin": true, "shopt": true, "wait": true, "unset": true}
	for k := range mutating {
		m[k] = true
	}
	return m
}()

// command collects the WB1401 candidates of a command and notes a wrapper
// of a builtin (`cd() { builtin cd "$@"; }`).
func (sc *fileScan) command(u *Use, words []*tree.Node, src []byte) {
	name := u.Name
	if name == "unset" { // variables removed: their values are not proven (WB1401); `unset -f` removes functions
		for _, w := range words[1:] {
			if t, ok := literalText(w); ok && len(t) > 1 && t[0] == '-' {
				if strings.ContainsRune(t, 'f') {
					return
				}
				continue
			}
			if t, ok := literalText(w); ok && identRe.MatchString(t) {
				if sc.unsetVars == nil {
					sc.unsetVars = map[string]bool{}
				}
				sc.unsetVars[t] = true
			}
		}
		return
	}
	if name == "shopt" || name == "wait" {
		sc.version.command(name, words)
		return
	}
	if name == "builtin" || name == "command" {
		if fn := namedFunc(u.Scope); fn != nil && len(words) > 1 {
			if arg, ok := wordName(words[1]); ok && arg == fn.Name {
				sc.wrappers[fn] = true
			}
		}
	}
	first := 1
	if (name == "sudo" || name == "command") && len(words) > 1 {
		var ok bool
		if name, ok = wordName(words[1]); !ok {
			return
		}
		first = 2
	}
	if !mutating[name] {
		return
	}
	for _, wd := range words[first:] {
		for _, p := range wd.Children {
			if p.Kind == tree.KParamExp && quoteCandidate(p, src) {
				sc.quotes = append(sc.quotes, p)
			}
		}
	}
}

// namedFunc returns the KFuncDecl node of the innermost named function
// around scope sc, nil at file level.
func namedFunc(sc *Scope) *tree.Node {
	if sc != nil && sc.closed {
		return sc.named
	}
	for ; sc != nil; sc = sc.Parent {
		if sc.Kind == ScopeFunction && sc.Func != "" {
			return sc.Node
		}
	}
	return nil
}

// notCommands are top-level commands a library runs too: they do not make a
// file a script.
var notCommands = map[string]bool{"source": true, ".": true, "set": true, "shopt": true, "alias": true, "unalias": true,
	"export": true, "readonly": true, "declare": true, "typeset": true, "local": true, ":": true, "true": true,
	"false": true, "trap": true, "unset": true, "umask": true}

// isTopCommand reports whether the top-level statement st runs something:
// not a function definition, an assignment, a declaration or one of
// notCommands.
func isTopCommand(st *tree.Node) bool {
	if st.Kind != tree.KStmt || len(st.Children) == 0 {
		return false
	}
	cmd := st.Children[0]
	switch cmd.Kind {
	case tree.KFuncDecl, tree.KDecl, tree.KRedirect, tree.KComment:
		return false
	case tree.KCall:
		for _, w := range cmd.Children {
			if w.Kind == tree.KWord {
				name, ok := wordName(w)
				return !ok || !notCommands[name]
			}
		}
		return false // assignments only
	}
	return true
}

// wordName is commandName without allocating for the common word, one
// literal without a backslash.
func wordName(w *tree.Node) (string, bool) {
	if len(w.Children) == 1 {
		if l := w.Children[0]; l.Kind == tree.KLit && !l.Recovered && strings.IndexByte(l.Value, '\\') < 0 {
			return l.Value, true
		}
	}
	name, ok, _ := commandName(w)
	return name, ok
}

// scanWalker walks a tree for its mentions: the variables it names (m set)
// or its words (words set).
type scanWalker struct {
	m     *fileMentions
	words map[string]bool
	src   []byte
	cmd   []*tree.Node // scratch: the words of the command being looked at
}

// mutating are the commands whose arguments name files they change or
// remove (WB1401).
var mutating = map[string]bool{"rm": true, "rmdir": true, "mv": true, "cp": true, "ln": true, "chmod": true, "chown": true,
	"chgrp": true, "mkdir": true, "touch": true, "unlink": true, "truncate": true}

// visit walks n. fn is the innermost function definition around it, nil at
// file level. cmdWord is set for the parts of a command word, whose literal
// text names the command rather than mentioning a name.
func (w *scanWalker) visit(n *tree.Node, fn *tree.Node, cmdWord bool) {
	switch n.Kind {
	case tree.KFuncDecl:
		for _, c := range n.Children {
			w.visit(c, n, false)
		}
		return
	case tree.KLit, tree.KSglQuoted:
		switch {
		case w.words != nil && !cmdWord:
			addTokens(w.words, n.Value)
			for _, f := range strings.Fields(n.Value) { // `ns::work`, `lib.cleanup`, `x@y`: a whole word may name a function
				w.words[f] = true
			}
		}
	case tree.KArithExp:
		// `$[ expr ]`, the old form of `$(( ))`, which the binder does not read
		if a, b := n.Range.Start.Offset, n.Range.End.Offset; w.m != nil && a >= 0 && a+2 <= b && b <= len(w.src) && string(w.src[a:a+2]) == "$[" {
			addIdents(w.m.dollar, string(w.src[a+2:b]))
		}
	case tree.KDecl:
		w.decl(n)
	case tree.KCall, tree.KTest:
		w.call(n, fn)
		return
	}
	inWord := cmdWord && (n.Kind == tree.KWord || n.Kind == tree.KDblQuoted)
	for _, c := range n.Children {
		w.visit(c, fn, inWord)
	}
}

// decl looks at a declaration command: the function-name forms (-f, -F:
// the names are functions) and the listing form (-p, which reads the
// variables it names).
func (w *scanWalker) decl(n *tree.Node) {
	var funcs, prints bool
	for _, a := range n.Children {
		if a.Kind != tree.KAssign || a.Name != "" || len(a.Children) == 0 || a.Children[0].Kind != tree.KWord {
			continue
		}
		if t, ok := literalText(a.Children[0]); ok && len(t) > 1 && t[0] == '-' {
			funcs = funcs || strings.ContainsAny(t[1:], "fF")
			prints = prints || strings.Contains(t[1:], "p")
		}
	}
	for _, a := range n.Children {
		if a.Kind == tree.KAssign && a.Name != "" {
			if funcs && w.words != nil {
				w.words[a.Name] = true
			}
			if prints && w.m != nil {
				w.m.dollar[a.Name] = true
			}
		}
	}
}

// call looks at a command or a `[[ ]]` test, then visits its parts.
func (w *scanWalker) call(n *tree.Node, fn *tree.Node) {
	w.cmd = w.cmd[:0]
	for _, c := range n.Children {
		if c.Kind == tree.KWord {
			w.cmd = append(w.cmd, c)
		}
	}
	var cmd *tree.Node
	if n.Kind == tree.KCall && len(w.cmd) > 0 {
		cmd = w.cmd[0]
	}
	if w.m != nil {
		w.mention(n, cmd != nil)
	}
	for _, c := range n.Children {
		w.visit(c, fn, c == cmd)
	}
}

// mention records `-v NAME` (a test that reads NAME by name) and `set -a`.
func (w *scanWalker) mention(n *tree.Node, isCall bool) {
	words := w.cmd
	addVar := func(wd *tree.Node) {
		name, _ := literalText(wd)
		if j := strings.IndexByte(name, '['); j > 0 {
			name = name[:j]
		}
		if name != "" {
			w.m.dollar[name] = true
		}
	}
	for i, wd := range words {
		if a, b := wd.Range.Start.Offset, wd.Range.End.Offset; i+1 < len(words) && b-a == 2 && b <= len(w.src) && string(w.src[a:b]) == "-v" {
			addVar(words[i+1])
		}
		// `[[ -v name ]]`: the operator is not a node
		if j := wd.Range.Start.Offset - 1; n.Kind == tree.KTest && j >= 2 && j < len(w.src) && w.src[j] == ' ' && w.src[j-2] == '-' && w.src[j-1] == 'v' {
			addVar(wd)
		}
		// `[[ a -gt lim ]]`: the operands of an arithmetic comparison are
		// arithmetic, where a bare name is a read
		if n.Kind == tree.KTest && w.arithOperand(n, i) {
			if t, ok := literalText(wd); ok {
				addIdents(w.m.dollar, t)
			}
		}
	}
	if isCall && !runsInSubshell(n, w.src) {
		if name, ok := wordName(words[0]); ok && name == "set" {
			for _, wd := range words[1:] {
				l, _ := literalText(wd)
				if l == "allexport" || len(l) > 1 && l[0] == '-' && l[1] != '-' && strings.Contains(l, "a") {
					w.m.allexport = true
				}
			}
		}
	}
}

var arithOps = map[string]bool{"-eq": true, "-ne": true, "-lt": true, "-le": true, "-gt": true, "-ge": true}

// arithOperand reports whether word i of the test n stands next to an
// arithmetic comparison operator (the operators are not nodes: the text
// between the words holds them).
func (w *scanWalker) arithOperand(n *tree.Node, i int) bool {
	words := w.cmd
	gap := func(a, b int) bool { // a field of src[a:b] is one of arithOps, read without allocating
		if a < 0 || b > len(w.src) || a >= b {
			return false
		}
		t := w.src[a:b]
		for i := 0; i+3 <= len(t); i++ {
			if t[i] == '-' && (i == 0 || t[i-1] == ' ' || t[i-1] == '\t') && (i+3 == len(t) || t[i+3] == ' ' || t[i+3] == '\t' || t[i+3] == '\n') &&
				arithOps[string(t[i:i+3])] {
				return true
			}
		}
		return false
	}
	before := n.Range.Start.Offset
	if i > 0 {
		before = words[i-1].Range.End.Offset
	}
	after := n.Range.End.Offset
	if i+1 < len(words) {
		after = words[i+1].Range.Start.Offset
	}
	return gap(before, words[i].Range.Start.Offset) || gap(words[i].Range.End.Offset, after)
}

// addIdents adds every identifier in v (arithmetic text) to m.
func addIdents(m map[string]bool, v string) {
	for i := 0; i < len(v); {
		if !isIdentStart(v[i]) || i > 0 && isIdentChar(v[i-1]) {
			i++
			continue
		}
		j := i + 1
		for j < len(v) && isIdentChar(v[j]) {
			j++
		}
		m[v[i:j]] = true
		i = j
	}
}

// quoteCandidate reports whether p, an unquoted part of a word, is `$name`
// or `${name...}` with name an identifier; not `${#name}` (a length) and not
// `${name:+x}` or `${name+x}` (quoting those turns "unset" into an empty
// argument), and not `${!name*}` or `${!name@}` (the names matching a prefix).
func quoteCandidate(p *tree.Node, src []byte) bool {
	a, b := p.Range.Start.Offset, p.Range.End.Offset
	if a < 0 || b > len(src) || a >= b || src[a] != '$' || !identRe.MatchString(p.Name) {
		return false
	}
	t := string(src[a:b])
	body, braced := strings.CutPrefix(t, "${")
	if !braced {
		return t == "$"+p.Name
	}
	if strings.HasPrefix(body, "#") {
		return false
	}
	indirect := strings.HasPrefix(body, "!")
	body = strings.TrimPrefix(body, "!")
	rest, ok := strings.CutPrefix(body, p.Name)
	if indirect && (strings.HasPrefix(rest, "*") || strings.HasPrefix(rest, "@")) {
		return false // `${!pre*}` and `${!pre@}` join the names that match
	}
	return ok && !strings.HasPrefix(rest, ":+") && !strings.HasPrefix(rest, "+")
}

// addTokens adds every identifier-shaped token of v to m; a token may hold
// '-' (a function name may), and then its parts before the first '-' too.
func addTokens(m map[string]bool, v string) {
	for i := 0; i < len(v); {
		if !isIdentStart(v[i]) {
			i++
			continue
		}
		j := i + 1
		for j < len(v) && (isIdentChar(v[j]) || v[j] == '-') {
			j++
		}
		m[v[i:j]] = true
		if k := strings.IndexByte(v[i:j], '-'); k >= 0 {
			m[v[i:i+k]] = true
		}
		i = j
	}
}

// evalCommands run a string argument as shell code, now or later: a
// single-quoted `$name` in their arguments is a read.
var evalCommands = map[string]bool{"eval": true, "trap": true, "bash": true, "sh": true, "dash": true, "ksh": true,
	"zsh": true, "alias": true, "su": true, "sudo": true, "ssh": true, "xargs": true, "env": true, "nohup": true, "timeout": true,
	"watch": true, "parallel": true, "exec": true, "find": true, "script": true, "complete": true, "bind": true}

// codeDollarNames adds every name after `$`, `${`, `${!` or `${#` in f's text
// except in comments, in single-quoted strings that no command evaluates
// (`echo '$x is literal'`, not `trap 'rm "$tmp"' EXIT` or `x='$y'`), and in
// the body of a heredoc whose tag is quoted.
func codeDollarNames(m map[string]bool, f *tree.File, want map[string]bool) {
	codeDollarNamesWith(m, f, want, (&heredocIndex{f: f}).op)
}

// heredocIndex answers, for the `<<` the text scan reads as a heredoc
// operator, whether the tree agrees: it is the operator of a redirect that
// holds a heredoc, and not a shift in arithmetic (`$((1<<2))`, `(( y = 1 << n
// ))`, `${a[1<<2]}`). It is built by one walk of the tree on the first `<<`
// and answers each one by lookup, so a file with a heredoc every three lines
// costs no more per heredoc than one with few. With no tree, or in code the
// parser left in an error gap, the text decides: it says yes.
type heredocIndex struct {
	f     *tree.File
	built bool
	ops   map[int]bool // offsets of the `<<` of redirects that hold a heredoc
	gaps  [][2]int     // error gaps, sorted and disjoint: start, end offsets
}

func (h *heredocIndex) op(i int) bool {
	if h.f.Root == nil {
		return true
	}
	if !h.built {
		h.build()
	}
	if h.ops[i] {
		return true
	}
	k := sort.Search(len(h.gaps), func(k int) bool { return h.gaps[k][1] > i })
	return k < len(h.gaps) && h.gaps[k][0] <= i
}

func (h *heredocIndex) build() {
	h.built = true
	h.ops = map[int]bool{}
	src := h.f.Src
	h.f.Root.Walk(func(n *tree.Node) bool {
		switch n.Kind {
		case tree.KErrorGap:
			h.gaps = append(h.gaps, [2]int{n.Range.Start.Offset, n.Range.End.Offset})
			return false
		case tree.KRedirect:
			if !slices.ContainsFunc(n.Children, func(k *tree.Node) bool { return k.Kind == tree.KHeredoc }) {
				return true
			}
			// the operator follows the redirect's fd (`3<<`, `{fd}<<`), if any
			j := n.Range.Start.Offset
			if j >= 0 && j < len(src) && src[j] == '{' {
				k := j + 1
				for k < len(src) && isIdentChar(src[k]) {
					k++
				}
				if k > j+1 && k < len(src) && src[k] == '}' {
					j = k + 1
				}
			} else {
				for j >= 0 && j < len(src) && src[j] >= '0' && src[j] <= '9' {
					j++
				}
			}
			if j >= 0 && j+1 < len(src) && src[j] == '<' && src[j+1] == '<' {
				h.ops[j] = true
			}
		}
		return true
	})
	slices.SortFunc(h.gaps, func(a, b [2]int) int { return a[0] - b[0] })
	merged := h.gaps[:0]
	for _, g := range h.gaps {
		if k := len(merged) - 1; k >= 0 && g[0] < merged[k][1] {
			merged[k][1] = max(merged[k][1], g[1])
			continue
		}
		merged = append(merged, g)
	}
	h.gaps = merged
}

// codeDollarNamesWith is codeDollarNames with the heredoc test given.
func codeDollarNamesWith(m map[string]bool, f *tree.File, want map[string]bool, heredocOp func(i int) bool) {
	src := f.Src
	cmts := f.Comments
	var loc *tree.Locator
	ask := func(i int) bool { // the tree decides: is the `$` at i inert text?
		if f.Root == nil {
			return false
		}
		if loc == nil {
			loc = f.NewLocator()
		}
		return inertText(loc.At(i), src)
	}
	// name reads the name after the `$` at i; k is past it, 0 for none.
	name := func(i int) (j, k int) {
		j = i + 1
		if j < len(src) && src[j] == '{' {
			j++
			if j < len(src) && (src[j] == '!' || src[j] == '#') {
				j++
			}
		}
		k = j
		for k < len(src) && isIdentChar(src[k]) {
			k++
		}
		if k == j || !isIdentStart(src[j]) {
			return j, 0
		}
		return j, k
	}
	record := func(j, k int, inSingle bool, i int) {
		if n := src[j:k]; want != nil && !want[string(n)] || m[string(n)] {
			return
		}
		if inSingle && ask(i) {
			return
		}
		m[string(src[j:k])] = true
	}
	// One pass of quote states, as a filter: a `$` this lexer sees inside
	// single quotes is checked against the tree; one it sees outside is
	// code. Heredoc bodies are read apart (a quoted tag's body is text, an
	// unquoted one's `$name` is code), so an apostrophe in one does not
	// turn the state over.
	const (
		normal = iota
		single
		double
	)
	var heredocs []heredocTag // started on the current line, bodies after it
	state, ci := normal, 0
	for i := 0; i < len(src); i++ {
		// jump to the next byte that can change the state or start a name
		stop := &lexStopNormal
		switch state {
		case single:
			stop = &lexStopSingle
		case double:
			stop = &lexStopDouble
		}
		for i < len(src) && !stop[src[i]] && !(state == normal && ci < len(cmts) && cmts[ci].Range.Start.Offset == i) {
			i++
		}
		if i >= len(src) {
			break
		}
		for ci < len(cmts) && cmts[ci].Range.End.Offset <= i {
			ci++
		}
		if state == normal && ci < len(cmts) && cmts[ci].Range.Start.Offset <= i {
			i = cmts[ci].Range.End.Offset - 1
			continue
		}
		c := src[i]
		switch state {
		case single:
			if c == '\'' {
				state = normal
				continue
			}
		case double:
			switch c {
			case '\\':
				if i+1 < len(src) && src[i+1] != '$' { // `\$x` stays a mention: eval may expand it later
					i++
				}
				continue
			case '"':
				state = normal
				continue
			}
		default:
			switch c {
			case '\\':
				if i+1 < len(src) && src[i+1] != '$' {
					i++
				}
				continue
			case '\'':
				state = single
				continue
			case '"':
				state = double
				continue
			case '<':
				if i+2 < len(src) && src[i+1] == '<' && src[i+2] == '<' { // `<<<` here-string: its word is lexed as any word
					i += 2
					continue
				}
				if h, end, ok := heredocAt(src, i); ok && heredocOp(i) {
					heredocs = append(heredocs, h)
					i = end - 1
				}
				continue
			case '\n':
				for _, h := range heredocs {
					i = h.body(src, i+1, func(d int) {
						if j, k := name(d); k > 0 {
							record(j, k, false, d)
						}
					}) - 1
				}
				heredocs = heredocs[:0]
				continue
			}
		}
		if c != '$' {
			continue
		}
		if j := i + 1; j < len(src) && src[j] == '\'' && state == normal { // $'...': ANSI-C quoting, text
			state = single
			i = j
			continue
		}
		if j, k := name(i); k > 0 {
			record(j, k, state == single, i)
		}
	}
}

// fdPrefix reports text that may stand before a redirect operator: nothing,
// an fd number or `{name}`.
func fdPrefix(p []byte) bool {
	if len(p) >= 3 && p[0] == '{' && p[len(p)-1] == '}' {
		p = p[1 : len(p)-1]
		for _, c := range p {
			if !isIdentChar(c) {
				return false
			}
		}
		return true
	}
	for _, c := range p {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

var lexStopNormal, lexStopSingle, lexStopDouble [256]bool

func init() {
	for _, c := range "\\'\"$<\n" {
		lexStopNormal[c] = true
	}
	for _, c := range "'$" {
		lexStopSingle[c] = true
	}
	for _, c := range "\\\"$" {
		lexStopDouble[c] = true
	}
}

// heredocTag is a heredoc operator read from the text: its tag, whether the
// tag was quoted (the body is then text) and whether it was `<<-` (leading
// tabs of the closing line are dropped).
type heredocTag struct {
	tag          string
	quoted, dash bool
}

// heredocAt reads a heredoc operator at src[i] (`<<TAG`, `<<-TAG`, a tag
// with quotes or a backslash); end is past it.
func heredocAt(src []byte, i int) (h heredocTag, end int, ok bool) {
	j := i + 2
	if j > len(src) || src[i+1] != '<' || j < len(src) && src[j] == '<' {
		return h, i + 1, false
	}
	if j < len(src) && src[j] == '-' {
		h.dash = true
		j++
	}
	for j < len(src) && (src[j] == ' ' || src[j] == '\t') {
		j++
	}
	var tag []byte
	for j < len(src) {
		c := src[j]
		switch {
		case c == '\'' || c == '"' || c == '\\':
			h.quoted = true
		case c == '_' || c == '-' || c == '.' || isIdentChar(c):
			tag = append(tag, c)
		default:
			goto done
		}
		j++
	}
done:
	if len(tag) == 0 {
		return h, i + 2, false
	}
	h.tag = string(tag)
	return h, j, true
}

// body reads the heredoc body that starts at from, up to and including its
// closing line, and returns the offset after it. For an unquoted tag, each
// `$` of the body is passed to dollar.
func (h heredocTag) body(src []byte, from int, dollar func(int)) int {
	for i := from; i < len(src); {
		e := bytes.IndexByte(src[i:], '\n')
		if e < 0 {
			e = len(src)
		} else {
			e += i
		}
		line := src[i:e]
		if h.dash {
			line = bytes.TrimLeft(line, "\t")
		}
		if string(bytes.TrimRight(line, "\r")) == h.tag {
			return min(e+1, len(src))
		}
		if !h.quoted {
			for k := i; k < e; k++ {
				if src[k] == '$' {
					dollar(k)
				}
			}
		}
		i = e + 1
	}
	return len(src)
}

// inertText reports whether node n (the deepest at a `$`) is text the
// shell never expands: a single-quoted string outside an evaluating command
// or assignment, or a quoted heredoc's body.
func inertText(n *tree.Node, src []byte) bool {
	switch {
	case n == nil:
		return false
	case n.Kind == tree.KSglQuoted:
		for p := n.Parent; p != nil; p = p.Parent {
			switch p.Kind {
			case tree.KAssign:
				return false
			case tree.KCall:
				for _, w := range p.Children {
					if w.Kind == tree.KWord {
						name, ok := wordName(w)
						return !ok || !evalCommands[name] && !isShell(name)
					}
				}
				return true
			case tree.KStmt, tree.KFile:
				return true
			}
		}
		return true
	case n.Kind == tree.KLit:
		if h := n.Enclosing(tree.KHeredoc); h != nil && h.Parent != nil {
			for _, w := range h.Parent.Children {
				if w.Kind == tree.KWord {
					for _, q := range w.Children {
						if q.Kind == tree.KSglQuoted || q.Kind == tree.KDblQuoted || q.Kind == tree.KLit && strings.Contains(q.Value, "\\") {
							return true
						}
					}
					return false
				}
			}
		}
	}
	return false
}

// nameUnion is what the files visible from a file read or mention by name,
// and whether one of them may read or call by a computed name. The flags
// come from the scans; reads and words, each a walk of every visible file
// (cached per file), only when a rule asks.
type nameUnion struct {
	indirect bool
	nameref  bool
	dynamic  bool            // an expansion or an unknown variable used as a command name
	gap      bool            // code not analysed: an error gap or a skipped file
	reads    map[string]bool // the file's own reads by name (readByName)
	others   []int           // the other visible files, on first need
	union    map[string]bool // their reads by name, once many names were looked up
	lookups  int
	words    map[string]bool // literal argument words and string tokens
}

// visible returns the flags of the file's visibility.
func (c *diagCtx) visible() *nameUnion {
	if c.union != nil {
		return c.union
	}
	s := c.s
	u := &nameUnion{}
	s.visible[c.file].each(func(j int) {
		ff := s.facts[j]
		if ff == nil || ff.File == nil || ff.File.Skipped != "" {
			u.gap = true
			return
		}
		sc := s.scanOf(j)
		u.indirect = u.indirect || sc.indirect
		u.nameref = u.nameref || sc.nameref
		u.gap = u.gap || sc.hasGap
		u.dynamic = u.dynamic || len(s.dyn[j]) > 0
	})
	c.union = u
	return u
}

// readByName reports whether the file's visibility may read name by name:
// in the file itself, a read bound to no declaration (or rebound through
// dynamic scope), a `"$name"` call or a mention (fileMentions); in another
// visible file, any read, call or mention of the name, whatever it binds to
// (cached per file, so an edit does not revisit every visible file's
// bindings). Few names are looked up one by one; many through one union.
func (c *diagCtx) readByName(name string) bool {
	u := c.visible()
	if u.reads == nil {
		c.ownReads(u)
	}
	if u.reads[name] {
		return true
	}
	if u.others == nil {
		c.s.visible[c.file].each(func(j int) {
			if j != c.file {
				if ff := c.s.facts[j]; ff != nil && ff.File != nil && ff.File.Skipped == "" {
					u.others = append(u.others, j)
				}
			}
		})
		if u.others == nil {
			u.others = []int{}
		}
	}
	if u.lookups++; u.lookups > 64 && u.union == nil {
		u.union = map[string]bool{}
		for _, j := range u.others {
			for k := range c.s.mentionsOf(j).reads {
				u.union[k] = true
			}
		}
	}
	if u.union != nil {
		return u.union[name]
	}
	for _, j := range u.others {
		if c.s.mentionsOf(j).reads[name] {
			return true
		}
	}
	return false
}

// allExport reports whether a visible file turns on allexport.
func (c *diagCtx) allExport() bool {
	found := false
	c.s.visible[c.file].each(func(j int) {
		if ff := c.s.facts[j]; !found && ff != nil && ff.File != nil && ff.File.Skipped == "" {
			found = c.s.mentionsOf(j).allexport
		}
	})
	return found
}

// ownReads fills u.reads with what the file itself reads by name.
func (c *diagCtx) ownReads(u *nameUnion) {
	s := c.s
	u.reads = map[string]bool{}
	// The file's own mentions, for the wanted names only (the file was just
	// edited: its mentions are not cached yet, and most of them are no
	// candidate's).
	codeDollarNames(u.reads, c.f, c.wantReads)
	if mentionWalkNeeded(c.f.Src) {
		m := &fileMentions{dollar: map[string]bool{}}
		walkMentions(c.ff, &scanWalker{m: m})
		for k := range m.dollar {
			u.reads[k] = true
		}
	}
	for k := range s.bindings[c.file] {
		b := &s.bindings[c.file][k]
		if b.Use.Kind == UseVarRead && (b.Res == Unresolved || b.Reason == ReasonDynamicScope) {
			u.reads[b.Use.Name] = true
		}
	}
	for _, us := range c.ff.Uses {
		if us.Kind == UseCall && us.Via == ViaVariable {
			u.reads[us.Name] = true
		}
	}
}

// visibleWords returns the words of the file's visibility.
func (c *diagCtx) visibleWords() map[string]bool {
	u := c.visible()
	if u.words != nil {
		return u.words
	}
	u.words = map[string]bool{}
	c.s.visible[c.file].each(func(j int) {
		if ff := c.s.facts[j]; ff != nil && ff.File != nil && ff.File.Skipped == "" {
			for k := range c.s.wordsOf(j) {
				u.words[k] = true
			}
		}
	})
	return u.words
}

// ruleRunHook, when set (by tests), is told each rule id a run computes.
var ruleRunHook func(id string)

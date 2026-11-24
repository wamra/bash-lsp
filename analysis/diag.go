package analysis

import (
	"bytes"
	"slices"
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
// scripts target, the rules turned off and per-rule severities.
type DiagOptions struct {
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
	c := &diagCtx{s: s, file: file, ff: ff, f: ff.File}
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
	sortDiagnostics(out)
	return out
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
		dup := false
		if ok && len(info.ShellCheck) > 0 {
			for _, d := range sc {
				if mapsTo(info.ShellCheck, d.Rule) && overlaps(n.Range, d.Range) {
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

	union *nameUnion // what the file's visibility reads or names, built on first need
}

func (c *diagCtx) add(rule string, r tree.Range, msg string) *Diagnostic {
	c.out = append(c.out, Diagnostic{Rule: rule, Severity: ruleByID[rule].Default, Range: r, Message: msg, Origin: OriginNative})
	return &c.out[len(c.out)-1]
}

func (c *diagCtx) run() {
	if c.f.Skipped != "" {
		// A file skipped for its nesting keeps the syntax error that says so.
		c.add("WB1002", tree.Range{Start: c.f.PosAt(0), End: c.f.PosAt(0)}, c.f.Skipped)
		if c.f.Root != nil {
			c.syntax()
		}
		return
	}
	c.scan = c.s.scanOf(c.file)
	c.syntax()
	c.sources()
	c.functions()
	c.localOutside()
	c.lostAssignments()
	c.unusedFunctions()
	c.unusedVariables()
	c.quoting()
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
	locals     []*tree.Node // `local` commands outside every function
	quotes     []*tree.Node // unquoted parameter expansions in arguments of file-mutating commands
	indirect   bool         // `${!` appears: a variable may be read through another's value
	nameref    bool         // some declaration is a nameref
	lastpipe   bool         // `lastpipe` appears (shopt -s lastpipe): the last pipeline stage may run in the shell
}

// fileMentions is what a file names without a binding: words that may name
// a function, and variables read by name (in strings, `-v NAME`, `declare -p
// NAME`). A second walk, made only for files whose visibility holds a
// candidate for an unused finding.
type fileMentions struct {
	dollar    map[string]bool // `$name` inside strings, `-v name`, `declare -p name`
	reads     map[string]bool // dollar, plus the name of every read and every `"$name"` call, whatever it binds to
	allexport bool            // `set -a` or `set -o allexport`: assignments are exported
}

// diagState holds the scans and mentions Diagnostics made, by file facts.
// Both depend only on the facts, which a snapshot built from the previous one
// shares for every file that did not change: Build carries the entries of
// those files over (carryDiag), so an edit rescans only the edited file.
type diagState struct {
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
	walkMentions(ff, &scanWalker{m: m})
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
	for _, d := range ff.Decls {
		switch {
		case d.Kind == DeclFunction && d.Node != nil:
			sc.funcs = append(sc.funcs, d.Node)
		case d.Kind == DeclVariable:
			sc.nameref = sc.nameref || d.Attrs.Nameref
			if d.Attrs.Local && d.Scope != nil && d.Scope.Func == "" && d.Node != nil {
				if p := d.Node.Parent; p != nil && p.Kind == tree.KDecl && p.Name == "local" && p != lastLocal {
					sc.locals = append(sc.locals, p)
					lastLocal = p
				}
			}
		}
	}
	var words []*tree.Node
	for _, u := range ff.Uses {
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
	if sc.topReturn {
		sc.topCommand = false
	}
	return sc
}

// inspected are the command names scanFile looks into: the file-changing
// commands and the prefixes before them, and builtin wrappers.
var inspected = func() map[string]bool {
	m := map[string]bool{"sudo": true, "command": true, "builtin": true}
	for k := range mutating {
		m[k] = true
	}
	return m
}()

// command collects the WB1401 candidates of a command and notes a wrapper
// of a builtin (`cd() { builtin cd "$@"; }`).
func (sc *fileScan) command(u *Use, words []*tree.Node, src []byte) {
	name := u.Name
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
		case w.m != nil && strings.IndexByte(n.Value, '$') >= 0:
			addDollarNames(w.m.dollar, n.Value)
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
	}
	if isCall {
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

// quoteCandidate reports whether p, an unquoted part of a word, is `$name`
// or `${name...}` with name an identifier; not `${#name}` (a length) and not
// `${name:+x}` or `${name+x}` (quoting those turns "unset" into an empty
// argument).
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
	body = strings.TrimPrefix(body, "!")
	rest, ok := strings.CutPrefix(body, p.Name)
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

// addDollarNames adds every name that follows `$` or `${` in v: text the
// shell may expand later (an eval or trap string, an escaped `\$x`).
func addDollarNames(m map[string]bool, v string) {
	for i := strings.IndexByte(v, '$'); i >= 0; {
		j := i + 1
		if j < len(v) && v[j] == '{' {
			j++
			if j < len(v) && (v[j] == '!' || v[j] == '#') {
				j++
			}
		}
		k := j
		for k < len(v) && isIdentChar(v[k]) {
			k++
		}
		if k > j && isIdentStart(v[j]) {
			m[v[j:k]] = true
		}
		next := strings.IndexByte(v[i+1:], '$')
		if next < 0 {
			break
		}
		i += 1 + next
	}
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
	for k := range s.mentionsOf(c.file).dollar {
		u.reads[k] = true
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

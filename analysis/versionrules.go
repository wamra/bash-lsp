package analysis

import (
	"bytes"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/wamra/bash-lsp/tree"
)

// FeatureInfo describes one feature the version and dialect rules know.
// MinBash is the first bash release series that has it (the zero value for
// a feature every bash has, such as `[[`); BashOnly marks syntax that an sh
// script does not have, which the parser front records as evidence
// (tree.File.Features) and WB1302 reports. Name is how a message calls it,
// in the plural: "associative arrays".
type FeatureInfo struct {
	Key      string
	MinBash  BashVersion
	BashOnly bool
	Name     string
}

// The first versions below are FDS-016's table (docs/internal/specs/
// 2026-11-22-bash-lsp-brd-fds-v0.2.md, row FDS-016), which takes them from
// the bash NEWS file. They are unconfirmed: no bash of those series is on
// the development machine, and bench/results/version-probe-m3.json confirms
// only that each fails on 3.2.57 (docs/version-table.md). `mapfile` and
// `readarray` are left out: FDS-016 records one conflicting source for
// their first version.
var featureTable = []FeatureInfo{
	{"assoc-array", BashVersion{4, 0}, false, "associative arrays"},
	{"case-modification", BashVersion{4, 0}, true, "case-modification expansions"},
	{"coproc", BashVersion{4, 0}, true, "coprocesses"},
	{"globstar", BashVersion{4, 0}, false, "globstar patterns"},
	{"case-fallthrough", BashVersion{4, 0}, true, "case fallthrough terminators"},
	{"pipe-all", BashVersion{4, 0}, true, "pipes of both output streams"},
	{"append-all", BashVersion{4, 0}, true, "appending redirections of both output streams"},
	{"declare-g", BashVersion{4, 2}, false, "global declarations"},
	{"lastpipe", BashVersion{4, 2}, false, "lastpipe settings"},
	{"nameref", BashVersion{4, 3}, false, "namerefs"},
	{"wait-n", BashVersion{4, 3}, false, "waiting for any job"},
	{"negative-subscript", BashVersion{4, 3}, true, "negative array subscripts"},
	{"transform-op", BashVersion{4, 4}, true, "parameter transformations"},
	{"epochseconds", BashVersion{5, 0}, false, "the EPOCHSECONDS and EPOCHREALTIME variables"},
	{"wait-p", BashVersion{5, 1}, false, "job ids captured by wait"},

	// Bash-only syntax that bash 3.2 has: no version, only WB1302.
	{"double-bracket", BashVersion{}, true, "double-bracket tests"},
	{"function-keyword", BashVersion{}, true, "functions declared with the function keyword"},
	{"array-assign", BashVersion{}, true, "array assignments"},
	{"ansi-c-quote", BashVersion{}, true, "ANSI-C quoted strings"},
	{"herestring", BashVersion{}, true, "here-strings"},
	{"redirect-all", BashVersion{}, true, "redirections of both output streams"},
	{"process-subst", BashVersion{}, true, "process substitutions"},
	{"source-builtin", BashVersion{}, true, "source commands"},
}

// featureSyntax is the form of each feature a message shows, in backticks.
var featureSyntax = map[string]string{
	"assoc-array":        "`declare -A`",
	"case-modification":  "`${v^^}`, `${v,,}`",
	"coproc":             "`coproc`",
	"globstar":           "`shopt -s globstar`",
	"case-fallthrough":   "`;&`, `;;&`",
	"pipe-all":           "`|&`",
	"append-all":         "`&>>`",
	"declare-g":          "`declare -g`",
	"lastpipe":           "`shopt -s lastpipe`",
	"nameref":            "`declare -n`",
	"wait-n":             "`wait -n`",
	"negative-subscript": "`${a[-1]}`",
	"transform-op":       "`${v@Q}`",
	"epochseconds":       "`$EPOCHSECONDS`",
	"wait-p":             "`wait -p`",
	"double-bracket":     "`[[ ]]`",
	"function-keyword":   "`function f`",
	"array-assign":       "`a=(1 2)`",
	"ansi-c-quote":       "`$'...'`",
	"herestring":         "`<<<`",
	"redirect-all":       "`&>`",
	"process-subst":      "`<(cmd)`",
	"source-builtin":     "`source`",
}

var featureByKey = func() map[string]FeatureInfo {
	m := make(map[string]FeatureInfo, len(featureTable))
	for _, f := range featureTable {
		m[f.Key] = f
	}
	return m
}()

// FeatureTable lists the features WB1301 and WB1302 know.
func FeatureTable() []FeatureInfo { return slices.Clone(featureTable) }

// featureHit is evidence the analysis finds in the facts rather than the
// syntax: a declaration option or a command option.
type featureHit struct {
	key string
	r   tree.Range
}

// versionScan is what scanFile collects for the version rules from the
// facts: no walk of the tree.
type versionScan struct {
	hits    []featureHit
	epoch   []*Use // reads of EPOCHSECONDS and EPOCHREALTIME
	assoc   map[string]bool
	src     []byte
	top     guardSet                 // checks outside every function
	funcs   map[*tree.Node]*guardSet // checks inside each function
	pending []pendingRead
}

// unknownMajor stands for a check whose guarantee cannot be decided: it
// guards every feature, as a check did before thresholds.
const unknownMajor = -1

// guardSet holds, for one scope (the top level or one function), the offset of
// the first check for each bash major version the check guarantees (0 to 10,
// larger values count as 10) and for the checks of unknown guarantee. An offset
// is stored plus one; zero means none.
type guardSet struct{ first [12]int }

func guardIndex(major int) int {
	if major < 0 {
		return 11
	}
	return min(major, 10)
}

func (g *guardSet) note(off, major int) {
	i := guardIndex(major)
	if g.first[i] == 0 || off+1 < g.first[i] {
		g.first[i] = off + 1
	}
}

// covers reports whether a check at or before off guarantees a feature that
// needs bash major version need.
func (g *guardSet) covers(off, need int) bool {
	for i, f := range g.first {
		if f != 0 && f-1 <= off && (i == 11 || i >= need) {
			return true
		}
	}
	return false
}

// any is covers for every offset.
func (g *guardSet) any(need int) bool { return g.covers(1<<62, need) }

// pendingRead is a read of a version variable whose value is assigned to
// name: it is a check only if name is tested later.
type pendingRead struct {
	name string
	n    *tree.Node
	off  int
}

// printers are the commands whose arguments are only shown: a read of the
// bash version there is a report, not a check.
var printers = map[string]bool{"echo": true, "printf": true, "log": true}

type readKind int

const (
	readPrint  readKind = iota // only shown
	readCheck                  // compared, tested, or decides a branch
	readAssign                 // assigned to a variable: a check if that is tested
)

// read notes a read of BASH_VERSION or BASH_VERSINFO. Only a read in code
// that does more than print the value counts as a check: comments,
// single-quoted strings and heredoc bodies are not reads; the arguments of
// echo, printf and log only print, unless the print is a stage of a pipeline
// that decides an `||`, `&&` or `if` (`echo "$BASH_VERSION" | grep -q ^4 ||
// exit 1`); an assignment (`v=$BASH_VERSION`) counts once the variable is
// tested (finish). A check guards what follows it in its own scope (the top
// level or its function); one at the top level also guards every function
// body, and a function that holds one guards the scope that calls it, from
// the call on (finish).
func (vs *versionScan) read(u *Use) {
	kind, name := versionCheck(u.Node)
	switch kind {
	case readCheck:
		vs.note(u.Node, u.Range.Start.Offset, vs.threshold(u.Node))
	case readAssign:
		vs.pending = append(vs.pending, pendingRead{name, u.Node, u.Range.Start.Offset})
	}
}

func (vs *versionScan) note(n *tree.Node, off, major int) {
	g := &vs.top
	if fn := enclosingFunc(n); fn != nil {
		if vs.funcs == nil {
			vs.funcs = map[*tree.Node]*guardSet{}
		}
		if vs.funcs[fn] == nil {
			vs.funcs[fn] = &guardSet{}
		}
		g = vs.funcs[fn]
	}
	g.note(off, major)
}

// finish settles the assignments from version variables (one is a check when
// the assigned variable is read after it in a test, an arithmetic expression
// or the word of a `case`) and the calls of functions that hold a check.
func (vs *versionScan) finish(uses []*Use) {
	if len(vs.pending) == 0 && len(vs.funcs) == 0 {
		return
	}
	var byName map[string]*guardSet
	for fn, g := range vs.funcs {
		if fn.Name == "" {
			continue
		}
		if byName == nil {
			byName = map[string]*guardSet{}
		}
		if byName[fn.Name] == nil {
			byName[fn.Name] = &guardSet{}
		}
		for i, f := range g.first {
			if f != 0 {
				byName[fn.Name].first[i] = 1
			}
		}
	}
	for _, u := range uses {
		off := u.Range.Start.Offset
		switch u.Kind {
		case UseVarRead:
			if len(vs.pending) == 0 || !testContext(u.Node) {
				continue
			}
			for i := 0; i < len(vs.pending); i++ {
				if p := vs.pending[i]; p.name == u.Name && off > p.off {
					vs.note(p.n, p.off, unknownMajor)
					vs.pending = slices.Delete(vs.pending, i, i+1)
					i--
				}
			}
		case UseCall:
			if g := byName[u.Name]; g != nil && u.Node != nil {
				for i, f := range g.first {
					if f == 0 {
						continue
					}
					major := i
					if i == 11 {
						major = unknownMajor
					}
					vs.note(u.Node, off, major)
				}
			}
		}
	}
	vs.pending = nil
}

// covered reports whether a feature of bash major version need at node at,
// offset off, sits behind a check.
func (vs *versionScan) covered(at *tree.Node, off, need int) bool {
	if vs.top.covers(off, need) || inFunc(at) && vs.top.any(need) {
		return true
	}
	for p := at; p != nil; p = p.Parent {
		if p.Kind == tree.KFuncDecl && vs.funcs[p] != nil && vs.funcs[p].covers(off, need) {
			return true
		}
	}
	return false
}

// majorExpr is the major version of bash as a test or an arithmetic
// expression reads it.
const majorExpr = `(?:"?\$\{BASH_VERSINFO(?:\[0\])?\}"?|"?\$BASH_VERSINFO"?|BASH_VERSINFO\[0\]|"?\$\{BASH_VERSION%%\.\*\}"?)`

var majorCompare = regexp.MustCompile(majorExpr + `\s*(-ge|-gt|-le|-lt|-eq|>=|>|<=|<|==|=)\s*"?(\d{1,3})"?`)
var exitWord = regexp.MustCompile(`\b(exit|return)\b`)

// threshold is the bash major version a check at n guarantees, or
// unknownMajor. Decided only for the simple form: the test or the
// arithmetic expression around the read holds exactly one mention of a
// version variable, compared with an integer literal by -ge, -gt, -eq, >=,
// >, == (the major is at least N, N+1, N), or by -lt, -le, <, <= when the
// statement around it also exits or returns (the major is at least N, N+1
// after it). Anything else, minor and patch numbers and string comparisons
// included, is unknown.
func (vs *versionScan) threshold(n *tree.Node) int {
	var box *tree.Node
	for p := n; p != nil && box == nil; p = p.Parent {
		switch p.Kind {
		case tree.KTest, tree.KArith:
			box = p
		case tree.KCall:
			if name, ok := callWord(p); ok && (name == "[" || name == "test") {
				box = p
			}
			if box == nil {
				return unknownMajor
			}
		case tree.KStmt:
			return unknownMajor
		}
	}
	if box == nil {
		return unknownMajor
	}
	a, b := box.Range.Start.Offset, box.Range.End.Offset
	if a < 0 || b > len(vs.src) || a >= b {
		return unknownMajor
	}
	text := vs.src[a:b]
	if bytes.Count(text, []byte("BASH_VERSINFO"))+bytes.Count(text, []byte("BASH_VERSION")) != 1 {
		return unknownMajor
	}
	m := majorCompare.FindSubmatch(text)
	if m == nil {
		return unknownMajor
	}
	var v int
	fmt.Sscanf(string(m[2]), "%d", &v)
	switch string(m[1]) {
	case "-ge", ">=", "-eq", "==", "=":
		return v
	case "-gt", ">":
		return v + 1
	}
	// -lt, -le, <, <=: a guard only when the statement leaves the script.
	st := box
	for p := box.Parent; p != nil; p = p.Parent {
		if p.Kind != tree.KStmt {
			continue
		}
		switch g := p.Parent; {
		case g != nil && g.Kind == tree.KAndOr:
			st = g
		case g != nil && g.Kind == tree.KBlock && g.Parent != nil && g.Parent.Kind == tree.KIf && g.Parent.Children[0] == g:
			st = g.Parent
		default:
			st = p
		}
		break
	}
	c, d := st.Range.Start.Offset, st.Range.End.Offset
	if c < 0 || d > len(vs.src) || c >= d || !exitWord.Match(vs.src[c:d]) {
		return unknownMajor
	}
	if string(m[1]) == "-le" || string(m[1]) == "<=" {
		return v + 1
	}
	return v
}

// versionCheck classifies a read at n; for an assignment it names the
// variable.
func versionCheck(n *tree.Node) (readKind, string) {
	for p := n; p != nil; p = p.Parent {
		switch p.Kind {
		case tree.KHeredoc:
			return readPrint, ""
		case tree.KTest, tree.KArith, tree.KArithExp, tree.KLet, tree.KStmt:
			return readCheck, ""
		case tree.KAssign:
			if p.Name != "" {
				return readAssign, p.Name
			}
		case tree.KDecl:
			return readCheck, ""
		case tree.KCall:
			name, _ := callWord(p)
			if printers[name] && !decidesBranch(p) {
				return readPrint, ""
			}
			return readCheck, ""
		}
	}
	return readCheck, ""
}

// decidesBranch reports whether a print call is a stage of a pipeline that is
// the left side of an `||` or `&&`, or the condition of an `if` or `while`.
func decidesBranch(call *tree.Node) bool {
	st := call.Parent
	if st == nil || st.Parent == nil || st.Parent.Kind != tree.KPipeline || len(st.Parent.Children) < 2 {
		return false
	}
	ps := st.Parent.Parent
	if ps == nil || ps.Kind != tree.KStmt || ps.Parent == nil {
		return false
	}
	switch p := ps.Parent; p.Kind {
	case tree.KAndOr:
		return slices.Index(p.Children, ps) < len(p.Children)-1
	case tree.KBlock:
		g := p.Parent
		return g != nil && (g.Kind == tree.KIf || g.Kind == tree.KWhile) && len(g.Children) > 0 && g.Children[0] == p
	}
	return false
}

// testContext reports whether a read at n is an operand of a test, an
// arithmetic expression, or the word of a `case`.
func testContext(n *tree.Node) bool {
	for p := n; p != nil && p.Kind != tree.KStmt; p = p.Parent {
		switch p.Kind {
		case tree.KTest, tree.KArith, tree.KArithExp, tree.KLet:
			return true
		case tree.KCall:
			name, ok := callWord(p)
			return ok && (name == "[" || name == "test")
		case tree.KWord:
			if g := p.Parent; g != nil && g.Kind == tree.KCase && len(g.Children) > 0 && g.Children[0] == p {
				return true
			}
		}
	}
	return false
}

// enclosingFunc is the nearest function declaration around n, nil at the top level.
func enclosingFunc(n *tree.Node) *tree.Node {
	for p := n; p != nil; p = p.Parent {
		if p.Kind == tree.KFuncDecl {
			return p
		}
	}
	return nil
}

// inFunc reports whether n is inside a function body.
func inFunc(n *tree.Node) bool {
	for p := n; p != nil; p = p.Parent {
		if p.Kind == tree.KFuncDecl {
			return true
		}
	}
	return false
}

// callWord is the literal command word of a call node.
func callWord(call *tree.Node) (string, bool) {
	for _, c := range call.Children {
		if c.Kind == tree.KWord {
			return literalText(c)
		}
	}
	return "", false
}

// decl notes the version evidence of a declaration: an associative array, a
// global declaration or a nameref. One finding per declaration command, not
// per name. last holds the command already noted for each key.
func (vs *versionScan) decl(d *Decl, last *[3]*tree.Node) {
	if d.Node == nil || d.Node.Parent == nil || d.Node.Parent.Kind != tree.KDecl {
		return
	}
	cmd := d.Node.Parent
	keyword := cmd.Name == "declare" || cmd.Name == "typeset" || cmd.Name == "local"
	for i, w := range [...]struct {
		on   bool
		key  string
		flag byte
	}{
		{d.Attrs.Assoc, "assoc-array", 'A'},
		{d.Attrs.Global && keyword, "declare-g", 'g'},
		{d.Attrs.Nameref && keyword, "nameref", 'n'},
	} {
		if !w.on || last[i] == cmd {
			continue
		}
		last[i] = cmd
		vs.hits = append(vs.hits, featureHit{w.key, declOptionRange(cmd, w.flag)})
		if i == 0 {
			if vs.assoc == nil {
				vs.assoc = map[string]bool{}
			}
			vs.assoc[d.Name] = true
		}
	}
}

// declOptionRange is the range from the declaration keyword to the end of
// the first option word that holds flag (`declare -A`), the keyword alone
// when none is found.
func declOptionRange(cmd *tree.Node, flag byte) tree.Range {
	for _, a := range cmd.Children {
		if a.Kind != tree.KAssign || a.Name != "" || len(a.Children) == 0 || a.Children[0].Kind != tree.KWord {
			continue
		}
		if t, ok := literalText(a.Children[0]); ok && len(t) > 1 && t[0] == '-' && strings.IndexByte(t[1:], flag) >= 0 {
			return tree.Range{Start: cmd.Range.Start, End: a.Children[0].Range.End}
		}
	}
	if cmd.NameRange.End.Offset > cmd.NameRange.Start.Offset {
		return cmd.NameRange
	}
	return cmd.Range
}

// command notes the version evidence of a `shopt` or `wait` call; words are
// the call's words, the command name first.
func (vs *versionScan) command(name string, words []*tree.Node) {
	at := func(w *tree.Node) tree.Range { return tree.Range{Start: words[0].Range.Start, End: w.Range.End} }
	switch name {
	case "shopt":
		set := false
		for _, w := range words[1:] {
			t, ok := literalText(w)
			if !ok {
				continue
			}
			switch t {
			case "-s":
				set = true
			case "-u", "-q", "-p":
				set = false
			case "globstar", "lastpipe":
				if set {
					vs.hits = append(vs.hits, featureHit{t, at(w)})
				}
			}
		}
	case "wait":
		for _, w := range words[1:] {
			t, ok := literalText(w)
			if !ok || len(t) < 2 || t[0] != '-' {
				continue
			}
			if t == "--" {
				break
			}
			if strings.IndexByte(t[1:], 'n') >= 0 {
				vs.hits = append(vs.hits, featureHit{"wait-n", at(w)})
			}
			if strings.IndexByte(t[1:], 'p') >= 0 {
				vs.hits = append(vs.hits, featureHit{"wait-p", at(w)})
			}
		}
	}
}

// --- WB1301, WB1302 ---

// versions runs the two rules that depend on the file's dialect: WB1301
// for a bash file, against the target range, and WB1302 for an sh file. A
// sourced file without a shebang is silent for both: the shell that sources
// it decides which one it runs in.
func (c *diagCtx) versions() {
	if !c.scan.shebang && c.s.sourced[c.file] {
		return
	}
	switch c.f.Dialect {
	case tree.Bash:
		c.bashVersions()
	case tree.POSIX:
		c.bashOnly()
	}
}

// bashVersions reports each feature whose first version is above the
// target's minimum, except where the script shows it copes with an older
// bash: a check of BASH_VERSION or BASH_VERSINFO guards every feature after
// it (the guard may be an `if`, an `&&`, or an early exit) and, when made at
// the top level, every function body; a use that tolerates failure is
// skipped (see tolerated).
func (c *diagCtx) bashVersions() {
	vs := &c.scan.version
	min := c.target.Min
	var loc *tree.Locator
	report := func(key string, r tree.Range) {
		info := featureByKey[key]
		if info.MinBash == (BashVersion{}) || !min.Less(info.MinBash) {
			return
		}
		if loc == nil {
			loc = c.f.NewLocator()
		}
		at := loc.At(r.Start.Offset)
		if vs.covered(at, r.Start.Offset, info.MinBash.Major) {
			return
		}
		if tolerated(c.f.Src, at) {
			return
		}
		c.add("WB1301", r, fmt.Sprintf("%s (%s) need bash %s; the target range starts at %s", info.Name, featureSyntax[key], info.MinBash, min))
	}
	var bs []Binding
	for _, ft := range c.f.Features {
		if ft.Key == tree.FeatNegativeSubscript && c.assocSubscript(&bs, ft.Range) {
			continue
		}
		report(ft.Key, ft.Range)
	}
	for _, h := range vs.hits {
		report(h.key, h.r)
	}
	if len(vs.epoch) > 0 {
		bs = c.s.bindings[c.file]
		for _, u := range vs.epoch {
			if r, ok := c.epochRead(bs, u); ok && !inTest(u.Node) {
				report("epochseconds", r)
			}
		}
	}
}

// assocSubscript reports whether the subscript expression in r indexes an
// associative array, where `-1` is a string key: the array is declared
// with -A in this file, or the name binds to such a declaration in another.
func (c *diagCtx) assocSubscript(bs *[]Binding, r tree.Range) bool {
	src := c.f.Src
	a, b := r.Start.Offset, r.End.Offset
	if a < 0 || b > len(src) || a >= b {
		return false
	}
	text := strings.TrimPrefix(string(src[a:b]), "$")
	text = strings.TrimPrefix(text, "{")
	text = strings.TrimLeft(text, "!#")
	i := 0
	for i < len(text) && (isIdentChar(text[i])) {
		i++
	}
	name := text[:i]
	if name == "" {
		return false
	}
	if c.scan.version.assoc[name] {
		return true
	}
	if *bs == nil {
		*bs = c.s.bindings[c.file]
	}
	b2 := *bs
	k := sort.Search(len(b2), func(k int) bool { return b2[k].Use.Range.Start.Offset >= a })
	for ; k < len(b2) && b2[k].Use.Range.Start.Offset < b; k++ {
		if x := b2[k]; x.Use.Name == name && x.Sym != nil && symFlags(x.Sym, func(v VarAttrs) bool { return v.Assoc }) {
			return true
		}
	}
	return false
}

// epochRead returns the range of a read of EPOCHSECONDS or EPOCHREALTIME
// that needs bash 5.0: the variable is not assigned by the script (every
// binding is the environment's), and the read does not give a fallback or
// test it (`${EPOCHSECONDS:-$(date +%s)}`, `${EPOCHSECONDS+x}`), the way a
// script that supports older bash reads it.
func (c *diagCtx) epochRead(bs []Binding, u *Use) (tree.Range, bool) {
	k := sort.Search(len(bs), func(k int) bool { return bs[k].Use.Range.Start.Offset >= u.Range.Start.Offset })
	for ; k < len(bs) && bs[k].Use.Range.Start.Offset == u.Range.Start.Offset; k++ {
		if bs[k].Use == u && bs[k].Res != Unresolved {
			return tree.Range{}, false
		}
	}
	r := u.Range
	if u.Node != nil && u.Node.Kind == tree.KParamExp {
		r = u.Node.Range
		src := c.f.Src
		if a, b := r.Start.Offset, r.End.Offset; a >= 0 && b <= len(src) && a < b {
			t := strings.TrimPrefix(string(src[a:b]), "${")
			t = strings.TrimLeft(t, "!#")
			if rest, ok := strings.CutPrefix(t, u.Name); ok {
				for _, op := range []string{":-", "-", ":=", "=", ":+", "+", ":?", "?"} {
					if strings.HasPrefix(rest, op) {
						return tree.Range{}, false
					}
				}
			}
		}
	}
	return r, true
}

// bashOnly reports each bash-only construct the parser front recorded in an
// sh file.
func (c *diagCtx) bashOnly() {
	for _, ft := range c.f.Features {
		info, ok := featureByKey[ft.Key]
		if !ok || !info.BashOnly {
			continue
		}
		c.add("WB1302", ft.Range, fmt.Sprintf("%s (%s) are bash only; this file is an sh script", info.Name, featureSyntax[ft.Key]))
	}
}

// tolerated reports whether the command at n copes with the feature being
// missing. n is the deepest node at the start of the finding. The command is
// tolerated when it is the left side of `||` in an and-or list, or its
// standard error goes to /dev/null (`shopt -s globstar 2>/dev/null ||
// true`). A test in front of it does not tolerate anything: what a test
// checks cannot be told from a version probe, so `[ -f x ] && declare -A m`
// and the body of an `if [ -f x ]` are reported like any other use. The
// version variables guard (see versionScan); an EPOCHSECONDS read inside a
// test is a probe (inTest).
func tolerated(src []byte, n *tree.Node) bool {
	for n != nil && n.Kind != tree.KStmt {
		n = n.Parent
	}
	if n == nil {
		return false
	}
	if stderrToNull(src, n) {
		return true
	}
	p := n.Parent
	if p == nil || p.Kind != tree.KAndOr {
		return false
	}
	i := slices.Index(p.Children, n)
	return i >= 0 && i+1 < len(p.Children) && andOrOp(src, p, i) == "||"
}

// stderrToNull reports whether the statement redirects standard error to
// /dev/null: `2>/dev/null`, `2>>/dev/null`, `&>/dev/null`, `&>>/dev/null`.
func stderrToNull(src []byte, st *tree.Node) bool {
	for _, c := range st.Children {
		if c.Kind != tree.KRedirect {
			continue
		}
		a, b := c.Range.Start.Offset, c.Range.End.Offset
		if a < 0 || b > len(src) || a >= b {
			continue
		}
		t := strings.Join(strings.Fields(string(src[a:b])), "")
		if t == "2>/dev/null" || t == "2>>/dev/null" || t == "&>/dev/null" || t == "&>>/dev/null" {
			return true
		}
	}
	return false
}

// andOrOp is the operator between operands i and i+1 of an and-or node:
// "&&", "||" or "" when the text between them holds neither.
func andOrOp(src []byte, p *tree.Node, i int) string {
	a, b := p.Children[i].Range.End.Offset, p.Children[i+1].Range.Start.Offset
	if a < 0 || b > len(src) || a > b {
		return ""
	}
	t := src[a:b]
	for k := 0; k+1 < len(t); k++ {
		if t[k] == '&' && t[k+1] == '&' {
			return "&&"
		}
		if t[k] == '|' && t[k+1] == '|' {
			return "||"
		}
	}
	return ""
}

// inTest reports whether the read at n is an operand of a test: inside
// `[[ ]]`, a `[` or `test` call, or a `(( ))` used as a condition. A test of
// EPOCHSECONDS is how a script finds out whether it has it.
func inTest(n *tree.Node) bool {
	for p := n; p != nil && p.Kind != tree.KStmt; p = p.Parent {
		switch p.Kind {
		case tree.KTest:
			return true
		case tree.KCall:
			name, ok := callWord(p)
			return ok && (name == "[" || name == "test")
		case tree.KArith:
			return arithIsCondition(p)
		}
	}
	return false
}

// arithIsCondition reports whether a `(( ))` command is an operand of an
// and-or list or part of an if or while condition.
func arithIsCondition(a *tree.Node) bool {
	st := a.Parent
	if st == nil || st.Kind != tree.KStmt || st.Parent == nil {
		return false
	}
	switch p := st.Parent; p.Kind {
	case tree.KAndOr:
		return true
	case tree.KBlock:
		g := p.Parent
		return g != nil && (g.Kind == tree.KIf || g.Kind == tree.KWhile) && len(g.Children) > 0 && g.Children[0] == p
	}
	return false
}

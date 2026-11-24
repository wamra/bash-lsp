package analysis

import (
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/wamra/bash-lsp/tree"
)

// Suppression directives, as comments:
//
//	# bash-lsp disable=WB1401,WB1202
//	# shellcheck disable=SC2086
//
// The ShellCheck form silences a native rule when one of the listed codes
// is in the rule's ShellCheck map; it takes codes, ranges (SC3000-SC4000)
// and `all`, as `man shellcheck` (section DIRECTIVES) describes. A directive
// before the first command applies to the whole file; otherwise to the
// command or block that follows it.
var (
	wbDirectiveRe = regexp.MustCompile(`^bash-lsp\s+(?:.*\s)?disable=(.*)$`)
	scDirectiveRe = regexp.MustCompile(`^shellcheck\s+(?:.*\s)?disable=(\S+)`)
	scCodeRe      = regexp.MustCompile(`^SC(\d{4})$`)
	bareCodeRe    = regexp.MustCompile(`^\d{4}$`)
	listSepRe     = regexp.MustCompile(`[,\s]+`)
	ruleIDShapeRe = regexp.MustCompile(`^[A-Za-z]{2}\d{4}$`)
)

type directive struct {
	wb         map[string]bool
	all        bool
	lo, hi     []int // ShellCheck code ranges, lo[i] to hi[i] inclusive
	start, end int   // covered offsets [start, end)
}

// silences reports whether the directive turns off a finding of rule whose
// ShellCheck codes are codes (the rule's map when nil).
func (d *directive) silences(rule string, codes []string) bool {
	if d.wb[rule] {
		return true
	}
	if codes == nil {
		codes = ruleByID[rule].ShellCheck
	}
	if len(codes) == 0 {
		return false
	}
	if d.all {
		return true
	}
	for _, c := range codes {
		a, b, ok := codeSpan(c)
		if !ok {
			continue
		}
		for i := range d.lo {
			if a <= d.hi[i] && d.lo[i] <= b {
				return true
			}
		}
	}
	return false
}

// codeSpan turns a map entry ("SC2086", or a prefix "SC3*") into the code
// numbers it covers.
func codeSpan(c string) (lo, hi int, ok bool) {
	if p, pre := strings.CutSuffix(c, "*"); pre {
		digits := strings.TrimPrefix(p, "SC")
		if len(digits) > 4 {
			return 0, 0, false
		}
		n, err := strconv.Atoi("0" + digits)
		if err != nil {
			return 0, 0, false
		}
		scale := 1
		for range 4 - len(digits) {
			scale *= 10
		}
		return n * scale, (n+1)*scale - 1, true
	}
	if m := scCodeRe.FindStringSubmatch(c); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n, n, true
	}
	return 0, 0, false
}

// parseDirective reads one comment; ok is false when it is not a directive.
func parseDirective(text string) (d directive, ok bool) {
	if m := wbDirectiveRe.FindStringSubmatch(text); m != nil {
		// The ids run until the first word that is not shaped like a rule id
		// (two letters and four digits): the rest is the author's comment
		// (`# bash-lsp disable=WB1401 -- the list splits on purpose`).
		d.wb = map[string]bool{}
		for _, id := range listSepRe.Split(strings.TrimSpace(m[1]), -1) {
			if id == "" {
				continue
			}
			if !ruleIDShapeRe.MatchString(id) {
				break
			}
			d.wb[id] = true
		}
		return d, true
	}
	m := scDirectiveRe.FindStringSubmatch(text)
	if m == nil {
		return d, false
	}
	for _, code := range strings.Split(m[1], ",") {
		code = strings.TrimSpace(code)
		if code == "all" {
			d.all = true
			continue
		}
		a, b, isRange := strings.Cut(code, "-")
		if !isRange {
			b = a
		}
		// ShellCheck 0.11.0 takes a code without its SC prefix (`disable=2086`)
		// and refuses a lower-case prefix (`sc2086`: SC1072, SC1073).
		if bareCodeRe.MatchString(a) {
			a = "SC" + a
		}
		if bareCodeRe.MatchString(b) {
			b = "SC" + b
		}
		x, _, okA := codeSpan(a)
		_, y, okB := codeSpan(b)
		if okA && okB && x <= y {
			d.lo, d.hi = append(d.lo, x), append(d.hi, y)
		}
	}
	return d, true
}

// suppress drops the findings a directive in f silences.
func suppress(f *tree.File, ds []Diagnostic) []Diagnostic {
	if len(ds) == 0 || len(f.Comments) == 0 || f.Root == nil {
		return ds
	}
	firstCmd := len(f.Src) + 1
	if len(f.Root.Children) > 0 {
		firstCmd = f.Root.Children[0].Range.Start.Offset
	}
	var dirs []directive
	for _, c := range f.Comments {
		d, ok := parseDirective(c.Value)
		if !ok || !placed(f.Src, c) {
			continue
		}
		if c.Range.Start.Offset < firstCmd {
			d.start, d.end = 0, len(f.Src)+1
		} else if n := nextCommand(c); n != nil && onlyComments(f.Src, c.Range.End.Offset, n.Range.Start.Offset) {
			d.start, d.end = n.Range.Start.Offset, n.Range.End.Offset
		} else {
			continue
		}
		dirs = append(dirs, d)
	}
	if len(dirs) == 0 {
		return ds
	}
	// Findings by rule, each list sorted by offset. A directive marks the
	// findings of each rule it silences in its range, jumping over those
	// already dropped (skip), so nested directives over many findings stay
	// linear.
	byRule := map[string][]int{} // by rule and, for a finding with its own codes, those codes
	for i, d := range ds {
		k := d.Rule
		if d.codes != nil {
			k += "\x00" + strings.Join(d.codes, ",")
		}
		byRule[k] = append(byRule[k], i)
	}
	type bucket struct {
		idx  []int
		skip []int // skip[k]: the first position at or after k not yet dropped
	}
	buckets := map[string]*bucket{}
	for r, idx := range byRule {
		slices.SortStableFunc(idx, func(a, b int) int { return ds[a].Range.Start.Offset - ds[b].Range.Start.Offset })
		skip := make([]int, len(idx)+1)
		for k := range skip {
			skip[k] = k
		}
		buckets[r] = &bucket{idx, skip}
	}
	var find func(b *bucket, k int) int
	find = func(b *bucket, k int) int {
		for b.skip[k] != k {
			b.skip[k] = b.skip[b.skip[k]]
			k = b.skip[k]
		}
		return k
	}
	drop := make([]bool, len(ds))
	for k := range dirs {
		d := &dirs[k]
		for _, b := range buckets {
			if f := ds[b.idx[0]]; !d.silences(f.Rule, f.codes) {
				continue
			}
			i := sort.Search(len(b.idx), func(i int) bool { return ds[b.idx[i]].Range.Start.Offset >= d.start })
			for i = find(b, i); i < len(b.idx) && ds[b.idx[i]].Range.Start.Offset < d.end; i = find(b, i+1) {
				drop[b.idx[i]] = true
				b.skip[i] = i + 1
			}
		}
	}
	out := ds[:0]
	for i, d := range ds {
		if !drop[i] {
			out = append(out, d)
		}
	}
	return out
}

// openers are the words after which a directive on the same line still
// stands before the command that follows (`do # shellcheck disable=…`).
var openers = map[string]bool{"then": true, "do": true, "else": true, "{": true, "(": true}

var (
	funcHeaderRe  = regexp.MustCompile(`^(?:function\s+)?[^\s()=;&|<>]+\s*\(\s*\)$|^function\s+[^\s()=;&|<>]+$`)
	casePatternRe = regexp.MustCompile(`^\(?[^()]*\)$`)
)

// placed reports whether comment c stands where a directive applies: alone
// on its line, or after an opener on it (`then`, `do`, `else`, `{`, `(`, a
// function header, a case pattern), as ShellCheck 0.11.0 takes it. After a
// command it applies to nothing (ShellCheck refuses it there, SC1126).
func placed(src []byte, c *tree.Node) bool {
	o := c.Range.Start.Offset
	if o < 0 || o > len(src) {
		return false
	}
	i := o
	for i > 0 && src[i-1] != '\n' {
		i--
	}
	before := strings.TrimSpace(string(src[i:o]))
	if before == "" {
		return true
	}
	fields := strings.Fields(before)
	if last := fields[len(fields)-1]; openers[last] || strings.HasSuffix(last, ";then") || strings.HasSuffix(last, ";do") {
		return true
	}
	if funcHeaderRe.MatchString(before) {
		return true
	}
	return casePatternRe.MatchString(before) && c.Enclosing(tree.KCase) != nil
}

// onlyComments reports whether src[a:b] holds nothing but blank space and
// comment lines: a directive before `else`, `elif` or a case pattern stands
// before no command (ShellCheck refuses it: SC1123, SC1124).
func onlyComments(src []byte, a, b int) bool {
	if a < 0 || b > len(src) || a > b {
		return false
	}
	for _, line := range strings.Split(string(src[a:b]), "\n") {
		if t := strings.TrimSpace(line); t != "" && t[0] != '#' {
			return false
		}
	}
	return true
}

// bodies are the nodes a directive cannot reach out of: a compound command
// or a body. A directive with no command after it in one covers nothing.
var bodies = map[tree.Kind]bool{tree.KFile: true, tree.KIf: true, tree.KWhile: true, tree.KFor: true, tree.KCStyleFor: true,
	tree.KCase: true, tree.KBlock: true, tree.KSubshell: true, tree.KFuncDecl: true, tree.KCoproc: true, tree.KTime: true,
	tree.KCmdSubst: true, tree.KProcSubst: true}

// nextCommand returns the statement that follows comment c in the body
// that holds it (a body's first statement when what follows is a body, as
// the `then` part after a directive inside `if`); nil when the body has no
// statement after it.
func nextCommand(c *tree.Node) *tree.Node {
	end := c.Range.End.Offset
	for n := c.Parent; n != nil; n = n.Parent {
		kids := n.Children
		i := sort.Search(len(kids), func(i int) bool { return kids[i].Range.Start.Offset >= end })
		for ; i < len(kids); i++ {
			if s := firstStmt(kids[i]); s != nil {
				return s
			}
		}
		if bodies[n.Kind] {
			return nil
		}
	}
	return nil
}

// firstStmt returns n when it is a statement, else the first statement in
// n when n is a body or compound command, else nil.
func firstStmt(n *tree.Node) *tree.Node {
	if n.Kind == tree.KStmt {
		return n
	}
	if !bodies[n.Kind] {
		return nil
	}
	for _, k := range n.Children {
		if s := firstStmt(k); s != nil {
			return s
		}
	}
	return nil
}

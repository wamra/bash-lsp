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
	wbDirectiveRe = regexp.MustCompile(`^bash-lsp\s+(?:.*\s)?disable=(\S+)`)
	scDirectiveRe = regexp.MustCompile(`^shellcheck\s+(?:.*\s)?disable=(\S+)`)
	scCodeRe      = regexp.MustCompile(`^SC(\d{4})$`)
)

type directive struct {
	wb         map[string]bool
	all        bool
	lo, hi     []int // ShellCheck code ranges, lo[i] to hi[i] inclusive
	start, end int   // covered offsets [start, end)
}

// silences reports whether the directive turns rule off.
func (d *directive) silences(rule string) bool {
	if d.wb[rule] {
		return true
	}
	codes := ruleByID[rule].ShellCheck
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
		d.wb = map[string]bool{}
		for _, id := range strings.Split(m[1], ",") {
			d.wb[strings.TrimSpace(id)] = true
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
		if !ok {
			continue
		}
		if c.Range.Start.Offset < firstCmd {
			d.start, d.end = 0, len(f.Src)+1
		} else if n := nextCommand(c); n != nil {
			d.start, d.end = n.Range.Start.Offset, n.Range.End.Offset
		} else {
			continue
		}
		dirs = append(dirs, d)
	}
	if len(dirs) == 0 {
		return ds
	}
	// Each directive marks the findings in its range: sort a copy of the
	// offsets once and search it per directive.
	order := make([]int, len(ds))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(a, b int) int { return ds[a].Range.Start.Offset - ds[b].Range.Start.Offset })
	drop := make([]bool, len(ds))
	for k := range dirs {
		d := &dirs[k]
		i := sort.Search(len(order), func(i int) bool { return ds[order[i]].Range.Start.Offset >= d.start })
		for ; i < len(order) && ds[order[i]].Range.Start.Offset < d.end; i++ {
			if j := order[i]; !drop[j] && d.silences(ds[j].Rule) {
				drop[j] = true
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

// nextCommand returns the statement that follows comment c: the first
// statement after it among the children of the node holding it, else of
// that node's ancestors.
func nextCommand(c *tree.Node) *tree.Node {
	end := c.Range.End.Offset
	for n := c.Parent; n != nil; n = n.Parent {
		kids := n.Children
		i := sort.Search(len(kids), func(i int) bool { return kids[i].Range.Start.Offset >= end })
		for ; i < len(kids); i++ {
			if k := kids[i]; k.Kind == tree.KStmt {
				return k
			}
		}
	}
	return nil
}

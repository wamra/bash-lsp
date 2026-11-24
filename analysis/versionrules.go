package analysis

import (
	"bytes"
	"fmt"
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
	guarded bool // the text mentions BASH_VERSION or BASH_VERSINFO
}

func (vs *versionScan) text(src []byte) {
	vs.guarded = bytes.Contains(src, []byte("BASH_VERSINFO")) || bytes.Contains(src, []byte("BASH_VERSION"))
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
// target's minimum. A file that mentions BASH_VERSION or BASH_VERSINFO is
// taken to guard its newer features and is not reported (the guard may be
// an `if`, an `&&`, or an early exit before the feature).
func (c *diagCtx) bashVersions() {
	vs := &c.scan.version
	if vs.guarded {
		return
	}
	min := c.target.Min
	report := func(key string, r tree.Range) {
		info := featureByKey[key]
		if info.MinBash == (BashVersion{}) || !min.Less(info.MinBash) {
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
			if r, ok := c.epochRead(bs, u); ok {
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

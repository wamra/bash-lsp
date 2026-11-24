package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/internal/config"
	"github.com/wamra/bash-lsp/internal/extool"
	"github.com/wamra/bash-lsp/internal/shellcheck"
	"github.com/wamra/bash-lsp/internal/workspace"
	"github.com/wamra/bash-lsp/tree"
)

const checkUsage = "usage: bash-lsp check [paths] [--format text|json] [--shellcheck auto|off] [--fail-on error|warning|info|hint] [--root DIR]"

// Exit codes of check: 0 no finding at or above --fail-on, 1 findings, 2 usage,
// 3 runtime failure (a path that cannot be read, a workspace that cannot be
// opened, a ShellCheck run that failed: the findings are then incomplete).
const (
	checkClean    = 0
	checkFindings = 1
	checkUsageErr = 2
	checkRuntime  = 3
)

type checkOpts struct {
	format string
	sc     string
	failOn analysis.Severity
	root   string
	paths  []string
}

func checkUsageError(stderr io.Writer, msg string) int {
	fmt.Fprintf(stderr, "bash-lsp: %s\n%s\n", msg, checkUsage)
	return checkUsageErr
}

var severityByName = map[string]analysis.Severity{
	"error": analysis.SevError, "warning": analysis.SevWarning, "info": analysis.SevInfo, "hint": analysis.SevHint,
}

func severityName(s analysis.Severity) string {
	switch s {
	case analysis.SevError:
		return "error"
	case analysis.SevWarning:
		return "warning"
	case analysis.SevHint:
		return "hint"
	}
	return "info"
}

// parseCheckArgs reads the flags (--flag value or --flag=value) and the paths.
func parseCheckArgs(args []string) (checkOpts, error) {
	o := checkOpts{format: "text", sc: "auto", failOn: analysis.SevWarning}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			if strings.HasPrefix(a, "-") && a != "-" {
				return o, fmt.Errorf("unknown flag %q for check", a)
			}
			o.paths = append(o.paths, a)
			continue
		}
		name, val, hasVal := strings.Cut(a, "=")
		switch name {
		case "--format", "--shellcheck", "--fail-on", "--root":
		default:
			return o, fmt.Errorf("unknown flag %q for check", name)
		}
		if !hasVal {
			if i+1 >= len(args) {
				return o, fmt.Errorf("%s needs a value", name)
			}
			i++
			val = args[i]
		}
		switch name {
		case "--format":
			if val != "text" && val != "json" && val != "sarif" {
				return o, fmt.Errorf("--format %q: want text or json", val)
			}
			o.format = val
		case "--shellcheck":
			if val != "auto" && val != "off" {
				return o, fmt.Errorf("--shellcheck %q: want auto or off", val)
			}
			o.sc = val
		case "--fail-on":
			sev, ok := severityByName[val]
			if !ok {
				return o, fmt.Errorf("--fail-on %q: want error, warning, info or hint", val)
			}
			o.failOn = sev
		case "--root":
			o.root = val
		}
	}
	return o, nil
}

// checkTarget is a file check reports on.
type checkTarget struct {
	path    string // the spelling snapshots use
	display string
}

type checkFix struct {
	Title string      `json:"title"`
	Safe  bool        `json:"safe"`
	Edits []checkEdit `json:"edits"`
}

type checkEdit struct {
	Line    int    `json:"line"`
	Col     int    `json:"col"`
	EndLine int    `json:"endLine"`
	EndCol  int    `json:"endCol"`
	NewText string `json:"newText"`
}

type checkDiag struct {
	Path     string     `json:"path"`
	Line     int        `json:"line"`
	Col      int        `json:"col"`
	EndLine  int        `json:"endLine"`
	EndCol   int        `json:"endCol"`
	Severity string     `json:"severity"`
	Rule     string     `json:"rule"`
	Origin   string     `json:"origin"`
	Message  string     `json:"message"`
	Fixes    []checkFix `json:"fixes"`

	sev analysis.Severity
}

// cmdCheck prints the findings of shell files: the native rules and, when a
// ShellCheck was found and is allowed, its findings merged in. The files are
// the named files, or the files below the named directories, taken from the
// workspace of findRoot (a repository's own files give `source` and the other
// cross-file rules their context). A file given with no repository above it is
// analysed alone: no directory around it is read.
func cmdCheck(args []string, stdout, stderr io.Writer) int {
	o, err := parseCheckArgs(args)
	if err != nil {
		return checkUsageError(stderr, err.Error())
	}
	if o.format == "sarif" {
		fmt.Fprintf(stderr, "bash-lsp: sarif: not available in this build\n")
		return checkUsageErr
	}
	if len(o.paths) == 0 {
		o.paths = []string{"."}
	}
	root, walk := findRoot(o.paths, o.root)
	st := config.Load(root, nil)
	for _, w := range st.Warnings {
		fmt.Fprintf(stderr, "bash-lsp: settings: %s\n", w)
	}
	for _, w := range st.CheckRules(analysis.KnownRule) {
		fmt.Fprintf(stderr, "bash-lsp: settings: %s\n", w)
	}
	prof := extool.ProfileFromEnv()
	if st.PathProfile != nil {
		prof = extool.NewProfile(st.PathProfile)
	}

	failed := false
	var snap *analysis.Snapshot
	var targets []checkTarget
	var ws *workspace.Workspace
	if walk {
		ws, err = workspace.OpenWith(root, st.Limits(), nil, st.Workspace(prof.Commands()))
		if err != nil {
			return fail3(stderr, err)
		}
		root = ws.Root()
		snap, targets, failed = checkWalked(ws, o.paths, stderr)
	} else {
		snap, targets, failed = checkAlone(root, o.paths, prof, stderr)
	}

	dopt := analysis.DiagOptions{Roots: []string{root}, Target: st.Target, Disabled: st.Disabled, Severity: st.Severity}
	native := make([][]analysis.Diagnostic, len(targets))
	for i, t := range targets {
		native[i] = snap.Diagnostics(t.path, dopt)
	}

	var runner *shellcheck.Runner
	if o.sc == "auto" && st.ShellCheck.Enabled {
		tool, ref, ok := extool.Find("shellcheck", st.ShellCheck.Path, prof, []string{root})
		switch {
		case ref != nil:
			fmt.Fprintf(stderr, "bash-lsp: shellcheck: refused %s: %s\n", ref.Path, ref.Reason)
		case ok:
			runner = shellcheck.New(tool, shellcheck.Options{Timeout: st.ShellCheck.Timeout, Roots: []string{root}})
		}
	}
	scDiags, notes, scFailed := runShellCheck(runner, snap, targets, stderr)
	failed = failed || scFailed
	for _, n := range notes {
		fmt.Fprintf(stderr, "bash-lsp: shellcheck: %s\n", n)
	}

	var out []checkDiag
	for i, t := range targets {
		merged := analysis.Merge(native[i], scDiags[i])
		ff := snap.Facts(t.path)
		if ff == nil || ff.File == nil {
			continue
		}
		conv := ff.File.NewConverter()
		for _, d := range merged {
			out = append(out, toCheckDiag(t.display, conv, d))
		}
	}

	// The bounds met while analysing: the findings cap only after Diagnostics ran.
	var limits []string
	if ws != nil {
		limits = append(limits, ws.Report().LimitsHit...)
	}
	limits = append(limits, targetLimits(snap.LimitsHit(), root, targets)...)
	for _, l := range limits {
		fmt.Fprintf(stderr, "bash-lsp: limit hit: %s\n", l)
	}

	if o.format == "json" {
		if out == nil {
			out = []checkDiag{}
		}
		if err := writeJSON(stdout, struct {
			Generation  uint64      `json:"generation"`
			Diagnostics []checkDiag `json:"diagnostics"`
		}{snap.Generation(), out}); err != nil {
			return fail3(stderr, err)
		}
	} else {
		for _, d := range out {
			fmt.Fprintf(stdout, "%s:%d:%d: %s %s %s\n", d.Path, d.Line, d.Col, d.Severity, d.Rule, oneLine(d.Message))
		}
	}
	if failed {
		return checkRuntime
	}
	for _, d := range out {
		if d.sev <= o.failOn {
			return checkFindings
		}
	}
	return checkClean
}

func fail3(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "bash-lsp: %v\n", err)
	return checkRuntime
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func toCheckDiag(path string, conv *tree.Converter, d analysis.Diagnostic) checkDiag {
	s, e := pos16(conv, d.Range.Start), pos16(conv, d.Range.End)
	fixes := []checkFix{}
	for _, f := range d.Fixes {
		cf := checkFix{Title: f.Title, Safe: f.Safe, Edits: []checkEdit{}}
		for _, ed := range f.Edits {
			a, b := pos16(conv, ed.Range.Start), pos16(conv, ed.Range.End)
			cf.Edits = append(cf.Edits, checkEdit{a[0], a[1], b[0], b[1], ed.NewText})
		}
		fixes = append(fixes, cf)
	}
	return checkDiag{Path: path, Line: s[0], Col: s[1], EndLine: e[0], EndCol: e[1], Severity: severityName(d.Severity),
		Rule: d.Rule, Origin: d.Origin, Message: d.Message, Fixes: fixes, sev: d.Severity}
}

// checkWalked takes the targets from the workspace: a directory gives the
// files the workspace loaded below it; a file the workspace did not load (an
// ignored or excluded one, or one outside the root) is added to it as an open
// document, so a file named on the command line is always checked.
func checkWalked(ws *workspace.Workspace, paths []string, stderr io.Writer) (*analysis.Snapshot, []checkTarget, bool) {
	failed := false
	type want struct {
		abs  string
		dir  bool
		disp string
	}
	var wants []want
	for _, p := range paths {
		abs := absResolved(p)
		fi, err := os.Stat(abs)
		if err != nil {
			fmt.Fprintf(stderr, "bash-lsp: %v\n", err)
			failed = true
			continue
		}
		if fi.IsDir() && !ws.Contains(abs) {
			fmt.Fprintf(stderr, "bash-lsp: %s: directory is outside the workspace root %s\n", p, ws.Root())
			failed = true
			continue
		}
		wants = append(wants, want{abs, fi.IsDir(), p})
	}
	loaded := map[string]bool{}
	for _, p := range ws.Snapshot().Paths() {
		loaded[p] = true
	}
	for _, w := range wants {
		if w.dir {
			continue
		}
		c := ws.Canon(w.abs)
		if loaded[c] {
			continue
		}
		src, err := readCapped(w.abs)
		if err != nil {
			fmt.Fprintf(stderr, "bash-lsp: %v\n", err)
			failed = true
			continue
		}
		ws.SetOverlay(c, 0, src)
		loaded[c] = true
	}
	snap := ws.Snapshot()
	seen := map[string]bool{}
	var targets []checkTarget
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			targets = append(targets, checkTarget{p, displayPath(ws, p)})
		}
	}
	all := snap.Paths()
	sort.Strings(all)
	for _, w := range wants {
		if !w.dir {
			add(ws.Canon(w.abs))
			continue
		}
		for _, p := range all {
			if rel, err := filepath.Rel(ws.Canon(w.abs), p); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				add(p)
			}
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].path < targets[j].path })
	return snap, targets, failed
}

func displayPath(ws *workspace.Workspace, p string) string {
	if ws.Contains(p) {
		return relSlash(ws.Root(), p)
	}
	return filepath.ToSlash(p)
}

// checkAlone analyses exactly the named files, with no walk. A directory
// among them is an error: nothing here reads a directory the user did not name
// a repository for.
func checkAlone(root string, paths []string, prof *extool.Profile, stderr io.Writer) (*analysis.Snapshot, []checkTarget, bool) {
	failed := false
	files := map[string][]byte{}
	var targets []checkTarget
	for _, p := range paths {
		abs := absResolved(p)
		fi, err := os.Stat(abs)
		switch {
		case err != nil:
			fmt.Fprintf(stderr, "bash-lsp: %v\n", err)
			failed = true
			continue
		case fi.IsDir():
			fmt.Fprintf(stderr, "bash-lsp: %s: a directory next to a file needs --root, or a .git or .bash-lsp.toml above them\n", p)
			failed = true
			continue
		}
		if _, dup := files[abs]; dup {
			continue
		}
		src, err := readCapped(abs)
		if err != nil {
			fmt.Fprintf(stderr, "bash-lsp: %v\n", err)
			failed = true
			continue
		}
		files[abs] = src
		targets = append(targets, checkTarget{abs, filepath.ToSlash(filepath.Clean(p))})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].path < targets[j].path })
	return analysis.Analyze(files, analysis.Options{Root: root, Commands: prof.Commands()}), targets, failed
}

// readCapped reads a file for check: a regular file, at most one byte over the
// size the analysis accepts (Analyze marks a longer one as skipped).
func readCapped(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, analysis.MaxFileBytes+1))
}

// runShellCheck runs the runner (nil: none) over the targets, a few at a time
// (the runner bounds the processes itself), and returns each target's
// findings, the distinct .shellcheckrc notices, and whether a run failed.
func runShellCheck(r *shellcheck.Runner, snap *analysis.Snapshot, targets []checkTarget, stderr io.Writer) ([][]analysis.Diagnostic, []string, bool) {
	res := make([][]analysis.Diagnostic, len(targets))
	if r == nil {
		return res, nil, false
	}
	type outcome struct {
		notes []string
		err   error
	}
	outs := make([]outcome, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, t := range targets {
		ff := snap.Facts(t.path)
		if ff == nil || ff.File == nil {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			ds, notes, err := r.CheckNotes(context.Background(), ff.File)
			res[i], outs[i] = ds, outcome{notes, err}
		}()
	}
	wg.Wait()
	failed := false
	seen := map[string]bool{}
	var notes []string
	for i, o := range outs {
		for _, n := range o.notes {
			if !seen[n] {
				seen[n] = true
				notes = append(notes, n)
			}
		}
		if o.err != nil {
			fmt.Fprintf(stderr, "bash-lsp: %s: %v\n", targets[i].display, o.err)
			failed = true
		}
	}
	sort.Strings(notes)
	return res, notes, failed
}

// doctorTools prints what `doctor` reports about the settings and the external
// programs of the workspace at root: the settings files read and the repo
// settings refused, the target, the PATH profile, ShellCheck and code-index. It
// reads the settings again (the warnings were printed when the workspace was
// opened) and starts at most one program, ShellCheck with --version.
func doctorTools(root string, stdout io.Writer) {
	st := config.Load(root, nil)
	prof := extool.ProfileFromEnv()
	if st.PathProfile != nil {
		prof = extool.NewProfile(st.PathProfile)
	}
	roots := []string{root}

	switch tool, ref, ok := extool.Find("shellcheck", st.ShellCheck.Path, prof, roots); {
	case !st.ShellCheck.Enabled:
		fmt.Fprintln(stdout, "shellcheck: disabled (shellcheck.enabled = false)")
	case ref != nil:
		fmt.Fprintf(stdout, "shellcheck: refused %s: %s\n", ref.Path, ref.Reason)
	case !ok:
		fmt.Fprintln(stdout, "shellcheck: absent")
	default:
		ctx, cancel := context.WithTimeout(context.Background(), st.ShellCheck.Timeout)
		v, err := shellcheck.New(tool, shellcheck.Options{Timeout: st.ShellCheck.Timeout, Roots: roots}).Version(ctx)
		cancel()
		if err != nil {
			fmt.Fprintf(stdout, "shellcheck: %s (version unknown: %v)\n", tool.Path, err)
		} else {
			fmt.Fprintf(stdout, "shellcheck: %s %s\n", tool.Path, v)
		}
	}
	fmt.Fprintln(stdout, "shellcheck memory guard: none (FDS-017 unmet, see docs/m3-acceptance.md)")
	for _, r := range st.Refused {
		fmt.Fprintf(stdout, "refused setting: %s %s: %s\n", r.File, r.Key, r.Reason)
	}
	if len(st.Files) == 0 {
		fmt.Fprintln(stdout, "config: none")
	} else {
		fmt.Fprintf(stdout, "config: %s\n", strings.Join(st.Files, ", "))
	}
	target := st.Target
	if target == (analysis.VersionRange{}) {
		target = analysis.DefaultTarget
	}
	fmt.Fprintf(stdout, "target: %s\n", target)
	fmt.Fprintf(stdout, "path profile: %d dirs (%d skipped)\n", prof.Dirs(), len(prof.Skipped()))
	for _, s := range prof.Skipped() {
		fmt.Fprintf(stdout, "  skipped: %s\n", s)
	}
	switch ci, ref, ok := extool.Find("code-index", st.CodeIndexPath, prof, roots); {
	case ref != nil:
		fmt.Fprintf(stdout, "code-index: refused %s: %s\n", ref.Path, ref.Reason)
	case !ok:
		fmt.Fprintln(stdout, "code-index: absent")
	default:
		fmt.Fprintf(stdout, "code-index: %s\n", ci.Path)
	}
}

// targetLimits keeps the snapshot's limit lines (`path:line: ...`, path
// relative to the root when inside it) that are about a file check reported
// on: the snapshot holds the whole workspace, and a check of one file should
// not list the limits of its neighbours.
func targetLimits(lines []string, root string, targets []checkTarget) []string {
	var out []string
	for _, l := range lines {
		for _, t := range targets {
			if strings.HasPrefix(l, relSlash(root, t.path)+":") {
				out = append(out, l)
				break
			}
		}
	}
	return out
}

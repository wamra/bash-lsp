// Package shellcheck is the bridge to the ShellCheck program: it sends one
// file at a time on standard input, reads the json1 report and turns the
// findings into analysis diagnostics. The program is started only through the
// extool runner, which bounds its time and output.
//
// Known limits. The user's own ShellCheck rc is honoured by design: when it
// sets external-sources=true, a repository file with `source /dev/zero` makes
// ShellCheck grow to gigabytes until the timeout (the repo's own rc is never
// given to it). No per-child memory limit exists in the standard library. A
// descendant that calls setsid escapes the process-group kill. On Windows only
// the child is killed. The program path is checked and then run, so a swap in
// between needs write access outside the workspace. A shim that picks its
// program by its own name or by a relative PATH entry may fail.
package shellcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/internal/extool"
	"github.com/wamra/bash-lsp/tree"
)

// OriginShellCheck is the Origin of every diagnostic Parse returns.
const OriginShellCheck = "shellcheck"

const fixTitle = "ShellCheck: apply fix"

// Options bound the runs of one Runner. A value of zero or less means the
// default: 10 s per run, 4 MiB of output, 2 runs at a time.
type Options struct {
	Timeout     time.Duration
	MaxOutput   int
	Concurrency int
	Roots       []string // the workspace folders (absolute): the child's PATH loses entries inside them, and their .shellcheckrc files are read by the server
}

// Runner runs one ShellCheck program, at most Options.Concurrency times at once.
type Runner struct {
	tool extool.Tool
	opt  Options
	sem  chan struct{}

	extraEnv []string // tests only: variables passed through the environment filter
}

// New returns a Runner for t.
func New(t extool.Tool, o Options) *Runner {
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	if o.MaxOutput <= 0 {
		o.MaxOutput = 4 << 20
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 2
	}
	return &Runner{tool: t, opt: o, sem: make(chan struct{}, o.Concurrency)}
}

// ShellFlag is the value of ShellCheck's --shell for a dialect; ok is false
// for a dialect ShellCheck cannot check (zsh) or an unknown one.
func ShellFlag(d tree.Dialect) (string, bool) {
	switch d {
	case tree.Bash, tree.Bats:
		return "bash", true
	case tree.POSIX:
		return "sh", true
	case tree.Mksh:
		return "ksh", true
	}
	return "", false
}

func (r *Runner) acquire(ctx context.Context) error {
	select {
	case r.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Runner) run(ctx context.Context, args []string, stdin []byte, dir string) (extool.Result, error) {
	if err := r.acquire(ctx); err != nil {
		return extool.Result{}, err
	}
	defer func() { <-r.sem }()
	return extool.Run(ctx, r.tool, args, extool.RunOptions{
		Dir: dir, Stdin: stdin, Env: extool.ChildEnv(r.opt.Roots, r.extraEnv...), Timeout: r.opt.Timeout, MaxOutput: r.opt.MaxOutput,
	})
}

// Check runs ShellCheck on f; see CheckNotes.
func (r *Runner) Check(ctx context.Context, f *tree.File) ([]analysis.Diagnostic, error) {
	ds, _, err := r.CheckNotes(ctx, f)
	return ds, err
}

// CheckNotes runs ShellCheck on f and also returns notices about entries of
// the repo's .shellcheckrc that were not used. The file's text goes on
// standard input and the arguments are fixed (the file's path or content is
// never part of them), so nothing in the analysed file can act as an option.
// The child runs in a neutral directory (extool.NeutralDir), so it finds no
// .shellcheckrc of the repository (which could make it read outside the
// workspace or without bound) and no program by a relative name; its own
// user-level rc still applies. Instead the server reads the repo rc (see
// readRC): codes it disables are dropped from the result, and its enable list
// is passed as --enable. SC1090 and SC1091 are excluded: ShellCheck cannot
// follow sources from here, and the native analysis, which has an index of the
// workspace, decides what a source resolves to (WB1101). A file that was
// skipped (File.Skipped) or whose dialect ShellCheck lacks yields nothing and
// starts nothing. Exit statuses 0 and 1 are success; any other is an error
// carrying the first line ShellCheck wrote to stderr.
func (r *Runner) CheckNotes(ctx context.Context, f *tree.File) ([]analysis.Diagnostic, []string, error) {
	if f == nil || f.Skipped != "" {
		return nil, nil, nil
	}
	shell, ok := ShellFlag(f.Dialect)
	if !ok {
		return nil, nil, nil
	}
	rc := readRC(f.Path, r.opt.Roots)
	args := []string{"--format=json1", "--shell=" + shell, "--exclude=SC1090,SC1091"}
	if len(rc.enable) > 0 {
		args = append(args, "--enable="+strings.Join(rc.enable, ","))
	}
	args = append(args, "-")
	res, err := r.run(ctx, args, f.Src, extool.NeutralDir())
	if err != nil {
		return nil, rc.notes, fmt.Errorf("shellcheck: %w", err)
	}
	if res.Exit != 0 && res.Exit != 1 {
		return nil, rc.notes, fmt.Errorf("shellcheck: exit status %d: %s", res.Exit, firstLine(res.Stderr))
	}
	ds, err := Parse(f, res.Stdout)
	if err != nil {
		return nil, rc.notes, fmt.Errorf("shellcheck: %w", err)
	}
	if len(rc.ranges) > 0 || rc.all {
		kept := ds[:0]
		for _, d := range ds {
			if code, err := strconv.Atoi(strings.TrimPrefix(d.Rule, "SC")); err == nil && rc.disabled(code) {
				continue
			}
			kept = append(kept, d)
		}
		ds = kept
	}
	return ds, rc.notes, nil
}

// Version asks the program for its version: the text after "version: " in
// the output of --version.
func (r *Runner) Version(ctx context.Context) (string, error) {
	res, err := r.run(ctx, []string{"--version"}, nil, extool.NeutralDir())
	if err != nil {
		return "", fmt.Errorf("shellcheck: %w", err)
	}
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "version: "); ok && v != "" {
			return v, nil
		}
	}
	return "", errors.New("shellcheck: no version line in the output of --version")
}

func firstLine(b []byte) string {
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return "(no message)"
}

// --- the json1 report ---

type comment struct {
	File      string `json:"file"`
	Line      int    `json:"line"`
	EndLine   int    `json:"endLine"`
	Column    int    `json:"column"`
	EndColumn int    `json:"endColumn"`
	Level     string `json:"level"`
	Code      int    `json:"code"`
	Message   string `json:"message"`
	Fix       *struct {
		Replacements []replacement `json:"replacements"`
	} `json:"fix"`
}

type replacement struct {
	Line        int    `json:"line"`
	EndLine     int    `json:"endLine"`
	Column      int    `json:"column"`
	EndColumn   int    `json:"endColumn"`
	Replacement string `json:"replacement"`
}

// Parse turns the json1 output of a run on f's text into diagnostics. Only
// comments about the standard input (file "-") count. The rule is "SC" and
// the code; level error, warning, info and style map to severities 1 to 4
// (an unknown level is info); a fix becomes one unsafe Fix whose edits are
// ShellCheck's replacements. ShellCheck counts columns in code points (a tab
// is one); they become byte offsets, then positions of f, in time linear in
// the size of the file and the report. A position outside the file is clamped
// to it, and a range whose end is before its start ends where it starts. The
// report must hold a comments array.
func Parse(f *tree.File, out []byte) ([]analysis.Diagnostic, error) {
	var rep struct {
		Comments json.RawMessage `json:"comments"`
	}
	if err := json.Unmarshal(out, &rep); err != nil {
		return nil, fmt.Errorf("unreadable ShellCheck output: %w", err)
	}
	if b := bytes.TrimSpace(rep.Comments); len(b) == 0 || b[0] != '[' {
		return nil, errors.New("unreadable ShellCheck output: no comments list")
	}
	var comments []comment
	if err := json.Unmarshal(rep.Comments, &comments); err != nil {
		return nil, fmt.Errorf("unreadable ShellCheck output: %w", err)
	}
	m := newMapper(f)
	at := func(line, col int) tree.Pos { return f.PosAt(m.offset(line, col)) }
	span := func(l1, c1, l2, c2 int) tree.Range {
		start, end := at(l1, c1), at(l2, c2)
		if end.Offset < start.Offset {
			end = start
		}
		return tree.Range{Start: start, End: end}
	}
	ds := make([]analysis.Diagnostic, 0, len(comments))
	for _, c := range comments {
		if c.File != "-" {
			continue
		}
		d := analysis.Diagnostic{
			Rule:     fmt.Sprintf("SC%d", c.Code),
			Severity: severity(c.Level),
			Range:    span(c.Line, c.Column, c.EndLine, c.EndColumn),
			Message:  c.Message,
			Origin:   OriginShellCheck,
		}
		if c.Fix != nil && len(c.Fix.Replacements) > 0 {
			fix := analysis.Fix{Title: fixTitle}
			for _, rp := range c.Fix.Replacements {
				fix.Edits = append(fix.Edits, analysis.TextEdit{
					Range:   span(rp.Line, rp.Column, rp.EndLine, rp.EndColumn),
					NewText: rp.Replacement,
				})
			}
			d.Fixes = []analysis.Fix{fix}
		}
		ds = append(ds, d)
	}
	return ds, nil
}

func severity(level string) analysis.Severity {
	switch level {
	case "error":
		return analysis.SevError
	case "warning":
		return analysis.SevWarning
	case "style":
		return analysis.SevHint
	}
	return analysis.SevInfo
}

// mapper turns a line and a code point column into a byte offset. A line of
// ASCII needs no walk; any other line is indexed once, on first use.
type mapper struct {
	src   []byte
	lines []int
	ascii map[int]bool    // line -> is all ASCII
	runes map[int][]int32 // line -> byte offset (from the line start) of each code point
}

func newMapper(f *tree.File) *mapper {
	m := &mapper{src: f.Src, ascii: map[int]bool{}, runes: map[int][]int32{}}
	m.lines = []int{0}
	for i, b := range f.Src {
		if b == '\n' {
			m.lines = append(m.lines, i+1)
		}
	}
	return m
}

// offset is the byte offset of the 1-based line and column; a line past the
// end gives the end of the file, a column past the end of the line the end
// of the line (before its newline).
func (m *mapper) offset(line, col int) int {
	if line < 1 {
		return 0
	}
	if line > len(m.lines) {
		return len(m.src)
	}
	start, end := m.lines[line-1], len(m.src)
	if line < len(m.lines) {
		end = m.lines[line] - 1 // the "\n"
	}
	if col <= 1 {
		return start
	}
	a, ok := m.ascii[line]
	if !ok {
		a = true
		for _, b := range m.src[start:end] {
			if b >= utf8.RuneSelf {
				a = false
				break
			}
		}
		m.ascii[line] = a
		if !a {
			offs := make([]int32, 0, end-start)
			for off := start; off < end; {
				offs = append(offs, int32(off-start))
				_, size := utf8.DecodeRune(m.src[off:end])
				off += size
			}
			m.runes[line] = offs
		}
	}
	if a {
		if col-1 >= end-start {
			return end
		}
		return start + col - 1
	}
	offs := m.runes[line]
	if col-1 >= len(offs) {
		return end
	}
	return start + int(offs[col-1])
}

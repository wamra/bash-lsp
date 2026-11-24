// Package shellcheck is the bridge to the ShellCheck program: it sends one
// file at a time on standard input, reads the json1 report and turns the
// findings into analysis diagnostics. The program is started only through the
// extool runner, which bounds its time and output.
package shellcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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

// childEnv is the environment ShellCheck gets: the variables it or the
// platform needs (a search path for a wrapper script, the home and config
// directories where it reads its .shellcheckrc, locale, temp and Windows
// system directories). Everything else is dropped, notably GHCRTS (Haskell
// runtime options, which can crash or hang it) and SHELLCHECK_OPTS (options
// belong to the settings files, not to whatever the editor was started from).
func (r *Runner) childEnv() []string {
	keep := func(name string) bool {
		switch strings.ToUpper(name) {
		case "PATH", "HOME", "XDG_CONFIG_HOME", "LANG", "LANGUAGE", "TMPDIR", "TEMP", "TMP",
			"SYSTEMROOT", "USERPROFILE", "APPDATA", "LOCALAPPDATA":
			return true
		}
		return strings.HasPrefix(name, "LC_")
	}
	if runtime.GOOS == "windows" {
		keep0 := keep
		keep = func(name string) bool { return keep0(strings.ToUpper(name)) }
	}
	env := []string{}
	for _, kv := range os.Environ() {
		if name, _, ok := strings.Cut(kv, "="); ok && name != "" && keep(name) {
			env = append(env, kv)
		}
	}
	return append(env, r.extraEnv...)
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
		Dir: dir, Stdin: stdin, Env: r.childEnv(), Timeout: r.opt.Timeout, MaxOutput: r.opt.MaxOutput,
	})
}

// Check runs ShellCheck on f. The file's text goes on standard input and the
// arguments are fixed (the file's path or content is never part of them), so
// nothing in the analysed file can act as an option; the working directory is
// the file's own, where ShellCheck looks for a .shellcheckrc. SC1091 is
// excluded because the native analysis decides, by its own index of the
// workspace, whether a sourced file is found (WB1101). A file that was skipped
// (File.Skipped) or whose dialect ShellCheck lacks yields no findings and no
// error without starting anything. Exit statuses 0 and 1 are success; any
// other is an error carrying the first line ShellCheck wrote to stderr.
func (r *Runner) Check(ctx context.Context, f *tree.File) ([]analysis.Diagnostic, error) {
	if f == nil || f.Skipped != "" {
		return nil, nil
	}
	shell, ok := ShellFlag(f.Dialect)
	if !ok {
		return nil, nil
	}
	args := []string{"--format=json1", "--shell=" + shell, "--exclude=SC1091", "-"}
	res, err := r.run(ctx, args, f.Src, dirOf(f.Path))
	if err != nil {
		return nil, fmt.Errorf("shellcheck: %w", err)
	}
	if res.Exit != 0 && res.Exit != 1 {
		return nil, fmt.Errorf("shellcheck: exit status %d: %s", res.Exit, firstLine(res.Stderr))
	}
	ds, err := Parse(f, res.Stdout)
	if err != nil {
		return nil, fmt.Errorf("shellcheck: %w", err)
	}
	return ds, nil
}

// Version asks the program for its version: the text after "version: " in
// the output of --version.
func (r *Runner) Version(ctx context.Context) (string, error) {
	res, err := r.run(ctx, []string{"--version"}, nil, "")
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

// dirOf is the directory of an absolute file path that exists, else "" (the
// server's own directory).
func dirOf(path string) string {
	if !filepath.IsAbs(path) {
		return ""
	}
	dir := filepath.Dir(path)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return ""
	}
	return dir
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

type jsonReport struct {
	Comments []comment `json:"comments"`
}

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
// is one); they become byte offsets, then positions of f. A position outside
// the file is clamped to it.
func Parse(f *tree.File, out []byte) ([]analysis.Diagnostic, error) {
	var rep jsonReport
	if err := json.Unmarshal(out, &rep); err != nil {
		return nil, fmt.Errorf("unreadable ShellCheck output: %w", err)
	}
	if rep.Comments == nil && !bytes.Contains(out, []byte(`"comments"`)) {
		return nil, errors.New("unreadable ShellCheck output: no comments list")
	}
	lines := lineStarts(f.Src)
	at := func(line, col int) tree.Pos { return f.PosAt(offsetOf(f.Src, lines, line, col)) }
	ds := make([]analysis.Diagnostic, 0, len(rep.Comments))
	for _, c := range rep.Comments {
		if c.File != "-" {
			continue
		}
		d := analysis.Diagnostic{
			Rule:     fmt.Sprintf("SC%d", c.Code),
			Severity: severity(c.Level),
			Range:    tree.Range{Start: at(c.Line, c.Column), End: at(c.EndLine, c.EndColumn)},
			Message:  c.Message,
			Origin:   OriginShellCheck,
		}
		if c.Fix != nil && len(c.Fix.Replacements) > 0 {
			fix := analysis.Fix{Title: fixTitle}
			for _, rp := range c.Fix.Replacements {
				fix.Edits = append(fix.Edits, analysis.TextEdit{
					Range:   tree.Range{Start: at(rp.Line, rp.Column), End: at(rp.EndLine, rp.EndColumn)},
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

func lineStarts(src []byte) []int {
	starts := []int{0}
	for i, b := range src {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

// offsetOf is the byte offset of the 1-based line and code point column; a
// line past the end gives the end of the file, a column past the end of the
// line the end of the line (before its newline).
func offsetOf(src []byte, lines []int, line, col int) int {
	if line < 1 {
		return 0
	}
	if line > len(lines) {
		return len(src)
	}
	start := lines[line-1]
	end := len(src)
	if line < len(lines) {
		end = lines[line] - 1 // the "\n"
	}
	off := start
	for n := 1; n < col && off < end; n++ {
		_, size := utf8.DecodeRune(src[off:end])
		off += size
	}
	return off
}

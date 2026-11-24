// Package config merges bash-lsp settings from the user file, the repo file
// and the language server's initialization options.
//
// Precedence is defaults, then the user file, then the repo file, then
// initialization options. Executable and hook keys are accepted only from the
// user file and initialization options; a repo file naming one is refused and
// the key is ignored.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/format"
	"github.com/wamra/bash-lsp/internal/workspace"
	"github.com/wamra/bash-lsp/tree"
)

// Source says where a settings layer came from.
type Source uint8

const (
	SrcDefault Source = iota
	SrcUser
	SrcRepo
	SrcInit
)

// Refusal is a repo setting that was ignored because only user settings may
// carry it.
type Refusal struct{ File, Key, Reason string }

// Settings are the merged settings.
type Settings struct {
	Target     analysis.VersionRange
	Dialect    tree.Dialect // bash.dialect: fallback for files with no directive, no shebang and a .sh or no extension; 0 = Bash
	Disabled   map[string]bool
	Severity   map[string]analysis.Severity
	ShellCheck struct {
		Enabled bool
		Path    string
		Timeout time.Duration
	}
	CodeIndexPath    string
	PathProfile      []string // nil = $PATH
	Include, Exclude []string // files.include / files.exclude, gitignore syntax, root-relative
	SourcePaths      []string // sources.paths, root-relative, appended after .shellcheckrc source-path entries
	MaxFiles         int      // [limits] max_files; 0 = built-in; may only lower
	MaxFileBytes     int64    // [limits] max_file_bytes; 0 = built-in; may only lower
	Format           format.Settings
	HookEnabled      bool
	ResolvedOnly     bool // references.resolvedOnly
	Refused          []Refusal
	Warnings         []string // unknown keys, parse errors, values above a cap, with file:line
	Files            []string // config files read
}

const (
	repoFileName   = ".bash-lsp.toml"
	maxConfigBytes = 64 << 10
	refusalReason  = "executables and hooks are accepted only from user settings"

	defaultShellCheckTimeout = 10 * time.Second
	maxShellCheckSeconds     = 120
	maxIndent                = 16
)

type kind uint8

const (
	kString kind = iota + 1
	kInt
	kBool
	kList
)

var kindName = map[kind]string{kString: "a string", kInt: "an integer", kBool: "a boolean", kList: "an array of strings"}

type spec struct {
	kind kind
	exec bool // user file and init options only
}

var known = map[string]spec{
	"bash.target":                {kind: kString},
	"bash.dialect":               {kind: kString},
	"diagnostics.disable":        {kind: kList},
	"shellcheck.enabled":         {kind: kBool},
	"shellcheck.path":            {kind: kString, exec: true},
	"shellcheck.timeout_seconds": {kind: kInt},
	"codeindex.path":             {kind: kString, exec: true},
	"path.profile":               {kind: kList, exec: true},
	"hook.enabled":               {kind: kBool, exec: true},
	"files.include":              {kind: kList},
	"files.exclude":              {kind: kList},
	"sources.paths":              {kind: kList},
	"limits.max_files":           {kind: kInt},
	"limits.max_file_bytes":      {kind: kInt},
	"format.indent":              {kind: kInt},
	"format.binary_next_line":    {kind: kBool},
	"format.switch_case_indent":  {kind: kBool},
	"format.space_redirects":     {kind: kBool},
	"format.function_next_line":  {kind: kBool},
	"format.simplify":            {kind: kBool},
	"references.resolvedOnly":    {kind: kBool},
}

var dialects = map[string]tree.Dialect{"bash": tree.Bash, "sh": tree.POSIX, "mksh": tree.Mksh, "bats": tree.Bats}

var severities = map[string]analysis.Severity{
	"error": analysis.SevError, "warning": analysis.SevWarning, "info": analysis.SevInfo, "hint": analysis.SevHint,
}

// unsupported marks an init option value that has no settings type.
type unsupported struct{ what string }

type layer struct {
	label   string
	src     Source
	entries []entry
}

// Merge combines settings; defaults < user < repo < init. It is pure: user and
// repo are the contents of the two files (nil when absent), init is the
// initializationOptions JSON. A file that does not parse is ignored whole and
// reported in Warnings.
func Merge(user, repo []byte, init json.RawMessage) Settings {
	var s Settings
	s.Target = analysis.DefaultTarget
	s.ShellCheck.Enabled = true
	s.ShellCheck.Timeout = defaultShellCheckTimeout
	if len(user) > 0 {
		s.applyFile("config.toml", SrcUser, user)
	}
	if len(repo) > 0 {
		s.applyFile(repoFileName, SrcRepo, repo)
	}
	if len(init) > 0 {
		s.applyInit(init)
	}
	return s
}

func (s *Settings) applyFile(label string, src Source, data []byte) {
	entries, _, err := parseEntries(data)
	if err != nil {
		var pe *ParseError
		if errors.As(err, &pe) {
			s.Warnings = append(s.Warnings, fmt.Sprintf("%s:%d: %s (file ignored)", label, pe.Line, pe.Msg))
		} else {
			s.Warnings = append(s.Warnings, fmt.Sprintf("%s: %v (file ignored)", label, err))
		}
		return
	}
	s.apply(layer{label, src, entries})
}

func (s *Settings) applyInit(raw json.RawMessage) {
	const label = "initializationOptions"
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var top any
	if err := dec.Decode(&top); err != nil {
		s.Warnings = append(s.Warnings, fmt.Sprintf("%s: %v (ignored)", label, err))
		return
	}
	if top == nil {
		return
	}
	obj, ok := top.(map[string]any)
	if !ok {
		s.Warnings = append(s.Warnings, label+": expected an object (ignored)")
		return
	}
	var entries []entry
	for _, sec := range sortedKeys(obj) {
		sub, isObj := obj[sec].(map[string]any)
		if !isObj {
			entries = append(entries, entry{"", sec, initValue(obj[sec]), 0})
			continue
		}
		for _, k := range sortedKeys(sub) {
			entries = append(entries, entry{sec, k, initValue(sub[k]), 0})
		}
	}
	s.apply(layer{label, SrcInit, entries})
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func initValue(v any) any {
	switch x := v.(type) {
	case string, bool:
		return x
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return n
		}
		return unsupported{"a non-integer number"}
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			str, ok := e.(string)
			if !ok {
				return unsupported{"an array with non-string elements"}
			}
			out = append(out, str)
		}
		return out
	case nil:
		return unsupported{"null"}
	}
	return unsupported{"an object"}
}

func (s *Settings) warn(l layer, e entry, format string, args ...any) {
	loc := l.label
	if e.line > 0 {
		loc = fmt.Sprintf("%s:%d", l.label, e.line)
	}
	s.Warnings = append(s.Warnings, loc+": "+fmt.Sprintf(format, args...))
}

func entryName(e entry) string {
	if e.section == "" {
		return e.key
	}
	return e.section + "." + e.key
}

func typeOf(v any) kind {
	switch v.(type) {
	case string:
		return kString
	case int64:
		return kInt
	case bool:
		return kBool
	case []string:
		return kList
	}
	return 0
}

func (s *Settings) apply(l layer) {
	for _, e := range l.entries {
		name := entryName(e)
		if e.section == "severity" {
			s.applySeverity(l, e)
			continue
		}
		sp, ok := known[name]
		if !ok {
			s.warn(l, e, "unknown key %s", name)
			continue
		}
		if sp.exec && l.src == SrcRepo {
			s.Refused = append(s.Refused, Refusal{repoFileName, name, refusalReason})
			continue
		}
		if typeOf(e.val) != sp.kind {
			what := "a different type"
			if u, isU := e.val.(unsupported); isU {
				what = u.what
			} else if k := typeOf(e.val); k != 0 {
				what = kindName[k]
			}
			s.warn(l, e, "%s must be %s, not %s (ignored)", name, kindName[sp.kind], what)
			continue
		}
		s.set(l, e, name)
	}
}

func (s *Settings) applySeverity(l layer, e entry) {
	name := entryName(e)
	if !bareKey(e.key) {
		s.warn(l, e, "bad rule id in %s (ignored)", name)
		return
	}
	str, ok := e.val.(string)
	sev, valid := severities[str]
	if !ok || !valid {
		s.warn(l, e, "%s must be one of error, warning, info, hint (ignored)", name)
		return
	}
	if s.Severity == nil {
		s.Severity = map[string]analysis.Severity{}
	}
	s.Severity[e.key] = sev
}

func (s *Settings) set(l layer, e entry, name string) {
	switch name {
	case "bash.target":
		r, err := analysis.ParseVersionRange(e.val.(string))
		if err != nil {
			s.warn(l, e, "bash.target: %v (ignored)", err)
			return
		}
		s.Target = r
	case "bash.dialect":
		d, ok := dialects[e.val.(string)]
		if !ok {
			s.warn(l, e, "bash.dialect %q is not supported; use bash, sh, mksh or bats (ignored)", e.val)
			return
		}
		s.Dialect = d
	case "diagnostics.disable":
		s.Disabled = map[string]bool{}
		for _, id := range e.val.([]string) {
			s.Disabled[id] = true
		}
	case "shellcheck.enabled":
		s.ShellCheck.Enabled = e.val.(bool)
	case "shellcheck.path":
		s.ShellCheck.Path = e.val.(string)
	case "shellcheck.timeout_seconds":
		n := e.val.(int64)
		if n < 1 || n > maxShellCheckSeconds {
			s.warn(l, e, "shellcheck.timeout_seconds must be 1 to %d, not %d (ignored)", maxShellCheckSeconds, n)
			return
		}
		s.ShellCheck.Timeout = time.Duration(n) * time.Second
	case "codeindex.path":
		s.CodeIndexPath = e.val.(string)
	case "path.profile":
		s.PathProfile = append([]string{}, e.val.([]string)...)
	case "hook.enabled":
		s.HookEnabled = e.val.(bool)
	case "files.include":
		s.Include = append([]string{}, e.val.([]string)...)
	case "files.exclude":
		s.Exclude = append([]string{}, e.val.([]string)...)
	case "sources.paths":
		s.SourcePaths = append([]string{}, e.val.([]string)...)
	case "limits.max_files":
		n := e.val.(int64)
		if n < 1 || n > workspace.DefaultMaxFiles {
			s.warn(l, e, "limits.max_files must be 1 to %d (limits may only lower the built-in cap), not %d (ignored)", workspace.DefaultMaxFiles, n)
			return
		}
		s.MaxFiles = int(n)
	case "limits.max_file_bytes":
		n := e.val.(int64)
		if n < 1 || n > workspace.DefaultMaxFileBytes {
			s.warn(l, e, "limits.max_file_bytes must be 1 to %d (limits may only lower the built-in cap), not %d (ignored)", int64(workspace.DefaultMaxFileBytes), n)
			return
		}
		s.MaxFileBytes = n
	case "format.indent":
		n := e.val.(int64)
		if n < 0 || n > maxIndent {
			s.warn(l, e, "format.indent must be 0 to %d, not %d (ignored)", maxIndent, n)
			return
		}
		u := uint(n)
		s.Format.Indent = &u
	case "format.binary_next_line":
		s.Format.BinaryNextLine = boolPtr(e)
	case "format.switch_case_indent":
		s.Format.SwitchCaseIndent = boolPtr(e)
	case "format.space_redirects":
		s.Format.SpaceRedirects = boolPtr(e)
	case "format.function_next_line":
		s.Format.FunctionNextLine = boolPtr(e)
	case "format.simplify":
		s.Format.Simplify = boolPtr(e)
	case "references.resolvedOnly":
		s.ResolvedOnly = e.val.(bool)
	}
}

func boolPtr(e entry) *bool {
	b := e.val.(bool)
	return &b
}

// UserPath is the user settings file: $BASH_LSP_CONFIG when set (an empty
// value means no user file), else <user config dir>/bash-lsp/config.toml.
func UserPath() (string, error) {
	if v, ok := os.LookupEnv("BASH_LSP_CONFIG"); ok {
		return v, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "bash-lsp", "config.toml"), nil
}

// Load reads the user file and <root>/.bash-lsp.toml (each at most 64 KiB; the
// repo file must not be a symlink leaving root) and merges them with init.
func Load(root string, init json.RawMessage) Settings {
	var notes, files []string
	var user, repo []byte
	if p, err := UserPath(); err == nil && p != "" {
		var note string
		if user, note = readCapped(p, ""); note != "" {
			notes = append(notes, note)
		} else if user != nil {
			files = append(files, p)
		}
	}
	if root != "" {
		p := filepath.Join(root, repoFileName)
		var note string
		if repo, note = readCapped(p, root); note != "" {
			notes = append(notes, note)
		} else if repo != nil {
			files = append(files, p)
		}
	}
	s := Merge(user, repo, init)
	s.Warnings = append(notes, s.Warnings...)
	s.Files = files
	return s
}

// readCapped returns the file's bytes (non-nil even when empty), or a note
// when it is unusable. A missing file gives neither. With within set, the file
// must resolve to a path inside that directory.
func readCapped(path, within string) ([]byte, string) {
	if within != "" {
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, noteFor(path, err)
		}
		base, err := filepath.EvalSymlinks(within)
		if err != nil {
			return nil, noteFor(path, err)
		}
		if rel, err := filepath.Rel(base, real); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Sprintf("%s: resolves outside the workspace root (ignored)", path)
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, noteFor(path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, noteFor(path, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Sprintf("%s: not a regular file (ignored)", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return nil, noteFor(path, err)
	}
	if len(data) > maxConfigBytes {
		return nil, fmt.Sprintf("%s: over 64 KiB (ignored)", path)
	}
	if data == nil {
		data = []byte{}
	}
	return data, ""
}

func noteFor(path string, err error) string {
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	return fmt.Sprintf("%s: %v (ignored)", path, err)
}

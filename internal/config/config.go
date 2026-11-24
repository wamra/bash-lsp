// Package config merges bash-lsp settings from the user file, the repo file
// and the language server's initialization options.
//
// Precedence is defaults, then the user file, then the repo file, then
// initialization options. Initialization options come from the editor the user
// runs and override everything.
//
// User settings are a floor against the repo file, which is untrusted input:
//   - executable and hook keys (shellcheck.path, codeindex.path, path.profile,
//     hook.enabled) are accepted only from the user file and initialization
//     options; a repo file naming one is refused (Settings.Refused) and the key
//     is ignored;
//   - a repo limit applies only when it is lower than the effective value so
//     far (the built-in cap, or the user's lower value);
//   - a repo file cannot set shellcheck.enabled = true once the user file set
//     it false;
//   - the repo's diagnostics.disable is added to the user's list and never
//     removes from it;
//   - shellcheck.timeout_seconds applies from a repo file only when lower than
//     the effective value (the built-in 10 s, or the user's);
//   - a repo [severity] entry cannot change a rule the user disabled or gave a
//     severity of its own; rules the user did not mention may be set.
//
// Each ignored repo value is reported in Settings.Warnings with the key and
// the line. The keys that describe the repository (bash.target, bash.dialect,
// files.include, files.exclude, sources.paths, the [format] options) are
// replaced by the repo file, except that its files.exclude is added to the
// user's and a negated pattern in it is dropped: they cannot run code or widen a bound, and the
// repo knows its own layout. A sources.paths entry is kept to that: the
// workspace (workspace.Config) drops, with a notice in Report.Notices, one that
// is absolute, spelled with ~ or $ (never expanded) or resolves outside the
// root; a .shellcheckrc source-path is held to the same rule. Lists are
// replaced by the later layer; [severity] and [format] merge per key.
//
// The initialization option for references.resolved_only is spelled
// references.resolved_only; references.resolvedOnly is accepted as an alias.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
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
	Target     analysis.VersionRange // bash.target "MIN-MAX": only MIN drives WB1301; MAX is validated and reserved (see analysis.DiagOptions)
	Dialect    tree.Dialect          // bash.dialect: fallback for files with no directive, no shebang and a .sh or no extension; 0 = Bash
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
	ResolvedOnly     bool // references.resolved_only (alias references.resolvedOnly)
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
	"references.resolved_only":   {kind: kBool},
	"references.resolvedOnly":    {kind: kBool},
}

// sectionNames lists the sections a settings file or init object may hold.
var sectionNames = func() map[string]bool {
	m := map[string]bool{"severity": true}
	for k := range known {
		m[k[:strings.IndexByte(k, '.')]] = true
	}
	return m
}()

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
	if _, err := dec.Token(); err != io.EOF {
		s.Warnings = append(s.Warnings, label+": text after the first JSON value (ignored)")
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
	l := layer{label, SrcInit, nil}
	for _, sec := range sortedKeys(obj) {
		if strings.Contains(sec, ".") {
			s.warn(l, entry{key: sec}, "%s: dotted keys are not supported (ignored)", sec)
			continue
		}
		sub, isObj := obj[sec].(map[string]any)
		if !isObj {
			if sectionNames[sec] {
				s.warn(l, entry{key: sec}, "%s must be an object (ignored)", sec)
				continue
			}
			entries = append(entries, entry{"", sec, initValue(obj[sec]), 0})
			continue
		}
		for _, k := range sortedKeys(sub) {
			if strings.Contains(k, ".") {
				s.warn(l, entry{key: k}, "%s.%s: dotted keys are not supported (ignored)", sec, k)
				continue
			}
			entries = append(entries, entry{sec, k, initValue(sub[k]), 0})
		}
	}
	l.entries = entries
	s.apply(l)
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
	// Rules the user disabled before this layer: a repo file cannot undo that.
	var userDisabled map[string]bool
	var userSeverity map[string]analysis.Severity
	if l.src == SrcRepo {
		userDisabled = s.Disabled
		userSeverity = maps.Clone(s.Severity)
	}
	var canon, alias *entry
	for i := range l.entries {
		e := l.entries[i]
		name := entryName(e)
		if e.section == "severity" {
			s.applySeverity(l, e, userDisabled, userSeverity)
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
		switch name {
		case "references.resolved_only":
			canon = &l.entries[i]
		case "references.resolvedOnly":
			alias = &l.entries[i]
		default:
			s.set(l, e, name)
		}
	}
	switch {
	case canon != nil && alias != nil:
		if canon.val.(bool) != alias.val.(bool) {
			s.warn(l, *alias, "references.resolvedOnly differs from references.resolved_only; using references.resolved_only (ignored)")
		}
		s.ResolvedOnly = canon.val.(bool)
	case canon != nil:
		s.ResolvedOnly = canon.val.(bool)
	case alias != nil:
		s.ResolvedOnly = alias.val.(bool)
	}
}

func (s *Settings) applySeverity(l layer, e entry, userDisabled map[string]bool, userSeverity map[string]analysis.Severity) {
	name := entryName(e)
	if _, set := userSeverity[e.key]; set {
		s.warn(l, e, "%s: the user settings set this severity; a repo file cannot change it (ignored)", name)
		return
	}
	if userDisabled[e.key] {
		s.warn(l, e, "%s: the user settings disable this rule; a repo file cannot re-enable it (ignored)", name)
		return
	}
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
		if l.src != SrcRepo || s.Disabled == nil {
			s.Disabled = map[string]bool{}
		} else {
			s.Disabled = maps.Clone(s.Disabled)
		}
		for _, id := range e.val.([]string) {
			s.Disabled[id] = true
		}
	case "shellcheck.enabled":
		if l.src == SrcRepo && e.val.(bool) && !s.ShellCheck.Enabled {
			s.warn(l, e, "shellcheck.enabled: the user settings turn ShellCheck off; a repo file cannot turn it on (ignored)")
			return
		}
		s.ShellCheck.Enabled = e.val.(bool)
	case "shellcheck.path":
		s.ShellCheck.Path = e.val.(string)
	case "shellcheck.timeout_seconds":
		n := e.val.(int64)
		if n < 1 || n > maxShellCheckSeconds {
			s.warn(l, e, "shellcheck.timeout_seconds must be 1 to %d, not %d (ignored)", maxShellCheckSeconds, n)
			return
		}
		if d := time.Duration(n) * time.Second; l.src == SrcRepo && d >= s.ShellCheck.Timeout {
			s.warn(l, e, "shellcheck.timeout_seconds %d is not lower than %d; a repo file may only lower it (ignored)", n, int64(s.ShellCheck.Timeout/time.Second))
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
		pats := e.val.([]string)
		if l.src != SrcRepo {
			s.Exclude = append([]string{}, pats...)
			break
		}
		// The repo file adds to the user's excludes. A negation would bring
		// back what the user excluded, so it is left out.
		for _, p := range pats {
			if strings.HasPrefix(p, "!") {
				s.warn(l, e, "files.exclude: negated pattern %q is not allowed in a repo file (ignored)", p)
				continue
			}
			s.Exclude = append(s.Exclude, p)
		}
	case "sources.paths":
		s.SourcePaths = append([]string{}, e.val.([]string)...)
	case "limits.max_files":
		n := e.val.(int64)
		if n < 1 || n > workspace.DefaultMaxFiles {
			s.warn(l, e, "limits.max_files must be 1 to %d (limits may only lower the built-in cap), not %d (ignored)", workspace.DefaultMaxFiles, n)
			return
		}
		if l.src == SrcRepo && s.MaxFiles != 0 && n >= int64(s.MaxFiles) {
			s.warn(l, e, "limits.max_files %d is not lower than the user's %d; a repo file may only lower it (ignored)", n, s.MaxFiles)
			return
		}
		s.MaxFiles = int(n)
	case "limits.max_file_bytes":
		n := e.val.(int64)
		if n < 1 || n > workspace.DefaultMaxFileBytes {
			s.warn(l, e, "limits.max_file_bytes must be 1 to %d (limits may only lower the built-in cap), not %d (ignored)", int64(workspace.DefaultMaxFileBytes), n)
			return
		}
		if l.src == SrcRepo && s.MaxFileBytes != 0 && n >= s.MaxFileBytes {
			s.warn(l, e, "limits.max_file_bytes %d is not lower than the user's %d; a repo file may only lower it (ignored)", n, s.MaxFileBytes)
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
	}
}

func boolPtr(e entry) *bool {
	b := e.val.(bool)
	return &b
}

// Limits are the [limits] values for workspace.OpenWith; a zero field means
// the built-in cap.
func (s Settings) Limits() workspace.Limits {
	return workspace.Limits{MaxFiles: s.MaxFiles, MaxFileBytes: s.MaxFileBytes}
}

// Workspace is the part of the settings workspace.OpenWith takes besides the
// limits; commands is the PATH profile lookup (nil: no command edges).
func (s Settings) Workspace(commands func(name string) (string, bool)) workspace.Config {
	return workspace.Config{Commands: commands, Include: s.Include, Exclude: s.Exclude, SourcePaths: s.SourcePaths, Dialect: s.Dialect}
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
// must resolve to a path inside that directory. A file that is not a regular
// file (FIFO, device, socket, directory) is never opened blocking: the path is
// resolved and stat'ed first, opened with O_NONBLOCK, and the opened file must
// be the one that was stat'ed.
func readCapped(path, within string) ([]byte, string) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, noteFor(path, err)
	}
	if within != "" {
		base, err := filepath.EvalSymlinks(within)
		if err != nil {
			return nil, noteFor(path, err)
		}
		if rel, err := filepath.Rel(base, real); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Sprintf("%s: resolves outside the workspace root (ignored)", path)
		}
	}
	before, err := os.Lstat(real)
	if err != nil {
		return nil, noteFor(path, err)
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Sprintf("%s: not a regular file (ignored)", path)
	}
	f, err := os.OpenFile(real, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, noteFor(path, err)
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return nil, noteFor(path, err)
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, fmt.Sprintf("%s: changed while it was read, or not a regular file (ignored)", path)
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

// CheckRules reports every rule id named in diagnostics.disable or [severity]
// that known does not accept, as "diagnostics.disable: unknown rule id ..." or
// "severity.ID: unknown rule id ...", sorted. Merge keeps unknown ids (the rule
// set belongs to the analysis, not to this package); the caller that owns the
// registry passes it here and publishes the result as warnings.
func (s Settings) CheckRules(known func(id string) bool) []string {
	var out []string
	for id := range s.Disabled {
		if !known(id) {
			out = append(out, fmt.Sprintf("diagnostics.disable: unknown rule id %q", id))
		}
	}
	for id := range s.Severity {
		if !known(id) {
			out = append(out, fmt.Sprintf("severity.%s: unknown rule id %q", id, id))
		}
	}
	sort.Strings(out)
	return out
}

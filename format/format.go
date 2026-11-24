package format

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/wamra/bash-lsp/internal/editorconfig"
	"github.com/wamra/bash-lsp/internal/front"
	"github.com/wamra/bash-lsp/tree"
)

// Options are the printer's options, as shfmt's.
type Options struct {
	Indent           uint // spaces per level; 0 means tabs (shfmt)
	BinaryNextLine   bool
	SwitchCaseIndent bool
	SpaceRedirects   bool
	FunctionNextLine bool
	Simplify         bool
}

func (o Options) front() front.FormatOptions {
	return front.FormatOptions{Indent: o.Indent, BinaryNextLine: o.BinaryNextLine, SwitchCaseIndent: o.SwitchCaseIndent,
		SpaceRedirects: o.SpaceRedirects, FunctionNextLine: o.FunctionNextLine, Simplify: o.Simplify}
}

// ErrSyntax is wrapped by the error of a file that has a syntax error, a
// statement the parser had to recover, or nesting beyond a limit; the message
// ends in "line:col: text". ErrUnsupported is wrapped by every other refusal:
// zsh, a file over 2 MiB, NUL bytes, a byte order mark, line endings that are
// neither all LF nor all CRLF, a range outside the file, and output that
// would not parse to the same script. In every refusal there is no text.
var (
	ErrSyntax      = front.ErrFormatSyntax
	ErrUnsupported = front.ErrFormatUnsupported
)

// Source formats a whole file and returns the new text. The file's dialect is
// the one analysis detects from path and src, and it is formatted with the
// grammar it was parsed with. Comments, heredoc bodies, string contents, the
// shebang line, CRLF line endings and a missing final newline are kept; a
// file with a parse error is never rewritten. The printed text is parsed
// again and has to give the same tree as the input, or it is refused.
func Source(path string, src []byte, o Options) ([]byte, error) {
	return front.Format(path, src, o.front())
}

// Range formats the top-level statements that the byte range [start, end)
// touches (an empty range touches the statement at that offset), with the
// comments above them, and returns the one replacement for them: the bytes
// [editStart, editEnd) of src become text. A range that touches no statement
// returns editStart == editEnd and no text. Like Source, it refuses a file
// with any parse error, even outside the range.
func Range(path string, src []byte, start, end int, o Options) (editStart, editEnd int, text []byte, err error) {
	return front.FormatRange(path, src, start, end, o.front())
}

// SourceWith is Source with fallback as the dialect of a file that names none
// itself (no directive, no shebang that names one, and an extension that says
// nothing): the bash.dialect setting. 0 means Bash, which is what Source uses.
func SourceWith(path string, src []byte, o Options, fallback tree.Dialect) ([]byte, error) {
	return front.FormatWith(path, src, o.front(), fallback)
}

// RangeWith is Range with a fallback dialect, as SourceWith is for Source.
func RangeWith(path string, src []byte, start, end int, o Options, fallback tree.Dialect) (editStart, editEnd int, text []byte, err error) {
	return front.FormatRangeWith(path, src, start, end, o.front(), fallback)
}

// Edit returns one replacement that turns old into new: the bytes
// [start, end) of old become text. Equal prefix and suffix are left out, at
// whole lines: start is at the start of a line and end at the start of one (or
// at the end of old). Equal inputs give start == end and no text.
func Edit(old, new []byte) (start, end int, text []byte) {
	p := 0
	for p < len(old) && p < len(new) && old[p] == new[p] {
		p++
	}
	if p == len(old) && p == len(new) {
		return 0, 0, nil
	}
	for p > 0 && old[p-1] != '\n' {
		p--
	}
	s := 0
	for s < len(old)-p && s < len(new)-p && old[len(old)-1-s] == new[len(new)-1-s] {
		s++
	}
	for ; s > 0; s-- {
		so, sn := len(old)-s, len(new)-s
		if (so == p || old[so-1] == '\n') && (sn == p || new[sn-1] == '\n') {
			break
		}
	}
	return p, len(old) - s, new[p : len(new)-s]
}

// LSPOptions are the editor's FormattingOptions: the tab width and whether
// the editor indents with spaces.
type LSPOptions struct {
	TabSize      uint
	InsertSpaces bool
}

// OptionsFor is the options for the file at path in a workspace at root.
// Highest first: the .editorconfig files from the file's directory up to
// root, then s (the merged settings), then the editor's FormattingOptions
// (lsp, nil outside the language server), then shfmt's defaults (tabs, every
// flag off). The editorconfig keys are shfmt's: indent_style, indent_size
// (and tab_width for indent_size = tab), binary_next_line, switch_case_indent
// (also case_indent), space_redirects, function_next_line and simplify.
func OptionsFor(path, root string, s Settings, lsp *LSPOptions) Options {
	var o Options
	if lsp != nil && lsp.InsertSpaces && lsp.TabSize > 0 {
		o.Indent = lsp.TabSize
	}
	if s.Indent != nil {
		o.Indent = *s.Indent
	}
	set := func(dst *bool, v *bool) {
		if v != nil {
			*dst = *v
		}
	}
	set(&o.BinaryNextLine, s.BinaryNextLine)
	set(&o.SwitchCaseIndent, s.SwitchCaseIndent)
	set(&o.SpaceRedirects, s.SpaceRedirects)
	set(&o.FunctionNextLine, s.FunctionNextLine)
	set(&o.Simplify, s.Simplify)
	props, _ := editorconfig.LookupNotes(path, root, editorconfigReader(root))
	applyProps(&o, props)
	return o
}

// EditorconfigNotes lists the .editorconfig sections that apply to no file
// because their glob is not supported (see internal/editorconfig).
func EditorconfigNotes(path, root string) []string {
	_, notes := editorconfig.LookupNotes(path, root, editorconfigReader(root))
	return notes
}

// maxEditorconfigRead bounds what is read of a .editorconfig file; a larger
// one is ignored by the reader of the properties.
const maxEditorconfigRead = 1 << 20

// editorconfigReader reads .editorconfig files for a workspace at root. A
// .editorconfig is repo-controlled, so it is read only if it is a regular
// file: it is resolved and stat'ed first, opened without blocking (a FIFO
// would hang the open), and the opened file must be the one that was stat'ed.
// One inside root must also resolve to a place inside root (a link out of the
// workspace is not followed). One outside root is the file of a path the user
// named, and is only checked for being a regular file.
func editorconfigReader(root string) func(string) ([]byte, bool) {
	return func(p string) ([]byte, bool) {
		p = filepath.FromSlash(p)
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			return nil, false
		}
		if root != "" && within(root, p) {
			base, err := filepath.EvalSymlinks(root)
			if err != nil || !within(base, real) {
				return nil, false
			}
		}
		before, err := os.Lstat(real)
		if err != nil || !before.Mode().IsRegular() {
			return nil, false
		}
		f, err := os.OpenFile(real, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, false
		}
		defer f.Close()
		after, err := f.Stat()
		if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
			return nil, false
		}
		b, err := io.ReadAll(io.LimitReader(f, maxEditorconfigRead))
		return b, err == nil
	}
}

// within reports whether p is dir or lies under it (lexically).
func within(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func applyProps(o *Options, props map[string]string) {
	switch props["indent_style"] {
	case "tab":
		o.Indent = 0
	case "space":
		// shfmt: 8 unless indent_size gives one; indent_size = tab means
		// tab_width.
		o.Indent = 8
		size := props["indent_size"]
		if size == "tab" {
			size = props["tab_width"]
		}
		if n, err := strconv.Atoi(size); err == nil && n > 0 {
			o.Indent = uint(n)
		}
	}
	flag := func(dst *bool, keys ...string) {
		for _, k := range keys {
			switch props[k] {
			case "true":
				*dst = true
				return
			case "false":
				*dst = false
				return
			}
		}
	}
	flag(&o.BinaryNextLine, "binary_next_line")
	flag(&o.SwitchCaseIndent, "case_indent", "switch_case_indent")
	flag(&o.SpaceRedirects, "space_redirects")
	flag(&o.FunctionNextLine, "function_next_line")
	flag(&o.Simplify, "simplify")
}

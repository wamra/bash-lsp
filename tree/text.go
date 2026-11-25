package tree

import (
	"sort"
	"unicode/utf8"
)

// lineStarts returns the byte offset of each line start, counting "\n" only:
// the line definition of compilers, shells, ShellCheck and grep, and so of
// Pos.Line and Pos.Col.
func lineStarts(src []byte) []int {
	starts := []int{0}
	for i, b := range src {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

// lspLineStarts is the line table of the Language Server Protocol, which
// breaks lines at "\r\n", "\n", and a "\r" not followed by "\n". It differs
// from lineStarts only in a text with a lone "\r". Only the UTF-16
// conversions use it.
func lspLineStarts(src []byte) []int {
	starts := []int{0}
	for i, b := range src {
		if b == '\n' || b == '\r' && (i+1 == len(src) || src[i+1] != '\n') {
			starts = append(starts, i+1)
		}
	}
	return starts
}

// lspLines returns the LSP line table, built on first use.
func (f *File) lspLines() []int {
	f.lspOnce.Do(func() { f.lsp = lspLineStarts(f.Src) })
	return f.lsp
}

// lineAt is the 0-based line of table t holding offset (already clamped).
func lineAt(t []int, offset int) int {
	return sort.Search(len(t), func(i int) bool { return t[i] > offset }) - 1
}

// contentEnd is the byte offset where the text of line (0-based) of table t
// ends, before its line break: "\n", "\r\n", or a lone "\r".
func (f *File) contentEnd(t []int, line int) int {
	if line+1 >= len(t) {
		return len(f.Src)
	}
	start, end := t[line], t[line+1]
	end-- // the "\n", or a lone "\r"
	if f.Src[end] == '\n' && end > start && f.Src[end-1] == '\r' {
		end--
	}
	return end
}

// PosAt returns the position of offset, clamped to [0, len(Src)].
func (f *File) PosAt(offset int) Pos {
	if offset < 0 {
		offset = 0
	}
	if offset > len(f.Src) {
		offset = len(f.Src)
	}
	i := sort.Search(len(f.lines), func(i int) bool { return f.lines[i] > offset }) - 1
	return Pos{Offset: offset, Line: i + 1, Col: offset - f.lines[i] + 1}
}

// LSPLine returns the 0-based LSP line holding offset, clamped to
// [0, len(Src)]. A line break belongs to the line it ends.
func (f *File) LSPLine(offset int) int {
	offset = min(max(offset, 0), len(f.Src))
	return lineAt(f.lspLines(), offset)
}

// units returns the UTF-16 length of the rune at the start of b and its byte
// size. Each invalid byte counts as one unit.
func units(b []byte) (u, size int) {
	r, size := utf8.DecodeRune(b)
	if r == utf8.RuneError && size <= 1 {
		return 1, 1
	}
	if r >= 0x10000 {
		return 2, size
	}
	return 1, size
}

// ToUTF16 converts p to a 0-based LSP line and a column in UTF-16 units.
func (f *File) ToUTF16(p Pos) (line, char int) { return f.toUTF16(f.lspLines(), p) }

// ToolUTF16 converts p to the 0-based line of Pos.Line (counted by "\n" only,
// as compilers, shells, ShellCheck and grep do) and a column in UTF-16 units
// from the start of that line. It is ToUTF16 for text that tools print.
func (f *File) ToolUTF16(p Pos) (line, char int) { return f.toUTF16(f.lines, p) }

func (f *File) toUTF16(t []int, p Pos) (line, char int) {
	p = f.PosAt(p.Offset)
	line = lineAt(t, p.Offset)
	for i := t[line]; i < p.Offset; {
		u, size := units(f.Src[i:p.Offset])
		char += u
		i += size
	}
	return line, char
}

// FromUTF16 converts a 0-based LSP line and UTF-16 column to a Pos. A column
// past the end of the line clamps to the line end (before its line break); a
// line past the last clamps to EOF.
func (f *File) FromUTF16(line, char int) Pos { return f.fromUTF16(f.lspLines(), line, char) }

// FromToolUTF16 is FromUTF16 for lines counted by "\n" only (see ToolUTF16).
func (f *File) FromToolUTF16(line, char int) Pos { return f.fromUTF16(f.lines, line, char) }

func (f *File) fromUTF16(t []int, line, char int) Pos {
	if line >= len(t) {
		return f.PosAt(len(f.Src))
	}
	if line < 0 {
		line = 0
	}
	if char < 0 {
		char = 0
	}
	start := t[line]
	end := f.contentEnd(t, line)
	off, n := start, 0
	for off < end && n < char {
		u, size := units(f.Src[off:end])
		if n+u > char { // column falls inside a surrogate pair
			break
		}
		n += u
		off += size
	}
	return f.PosAt(off)
}

// checkpointEvery is the byte spacing of a Converter's checkpoints on a long line.
const checkpointEvery = 1024

// A Converter does the work of File.ToUTF16 for many positions of one file.
// ToUTF16 rescans the line from its start on every call, which is quadratic
// when thousands of positions sit on one long line; a Converter scans each
// long line once, keeps a (byte offset, UTF-16 column) checkpoint every
// checkpointEvery bytes, and answers each position from the nearest
// checkpoint, in any order. Results equal File.ToUTF16's. Not safe for
// concurrent use; make one per request.
type Converter struct {
	f     *File
	t     []int                        // the line table: LSP lines, or "\n" lines for a tool converter
	lines map[int][]checkpoint         // by line index, for lines longer than checkpointEvery
	last  struct{ line, off, col int } // a rune boundary and its column, from the previous call
}

type checkpoint struct{ off, col int }

// NewConverter returns a Converter for f that gives LSP lines, like File.ToUTF16.
func (f *File) NewConverter() *Converter { return &Converter{f: f, t: f.lspLines()} }

// NewToolConverter returns a Converter for f that gives "\n" lines, like
// File.ToolUTF16, for text that tools print.
func (f *File) NewToolConverter() *Converter { return &Converter{f: f, t: f.lines} }

// ToUTF16 is File.ToUTF16 (File.ToolUTF16 for a tool converter).
func (c *Converter) ToUTF16(p Pos) (line, char int) {
	f, t := c.f, c.t
	p = f.PosAt(p.Offset)
	line = lineAt(t, p.Offset)
	i := t[line]
	if p.Offset-i <= checkpointEvery {
		return f.toUTF16(t, p)
	}
	cps, ok := c.lines[line]
	if !ok {
		cps = f.checkpoints(t, line)
		if c.lines == nil {
			c.lines = map[int][]checkpoint{}
		}
		c.lines[line] = cps
	}
	// the last checkpoint at or before p.Offset
	k := sort.Search(len(cps), func(k int) bool { return cps[k].off > p.Offset }) - 1
	i, char = cps[k].off, cps[k].col
	// positions usually come in source order: continue from the previous answer when it is closer
	if c.last.line == line && c.last.off <= p.Offset && c.last.off > i {
		i, char = c.last.off, c.last.col
	}
	end := len(f.Src)
	if line+1 < len(t) {
		end = t[line+1]
	}
	for i < p.Offset {
		u, size := units(f.Src[i:end])
		if i+size > p.Offset { // p is inside this rune: File.ToUTF16 cuts it, each byte one unit
			return line, char + p.Offset - i
		}
		char += u
		i += size
	}
	c.last.line, c.last.off, c.last.col = line, i, char
	return line, char
}

// checkpoints scans line once, recording a checkpoint at a rune boundary at
// least every checkpointEvery bytes; the first is the line start.
func (f *File) checkpoints(t []int, line int) []checkpoint {
	start := t[line]
	end := len(f.Src)
	if line+1 < len(t) {
		end = t[line+1]
	}
	cps := []checkpoint{{start, 0}}
	col, next := 0, start+checkpointEvery
	for i := start; i < end; {
		u, size := units(f.Src[i:end])
		col += u
		i += size
		if i >= next {
			cps = append(cps, checkpoint{i, col})
			next = i + checkpointEvery
		}
	}
	return cps
}

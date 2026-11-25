package tree

import (
	"sort"
	"unicode/utf8"
)

// lineStarts returns the byte offset of each line start. A line break is
// "\r\n", "\n", or a "\r" not followed by "\n" (LSP 3.17, Text Documents).
func lineStarts(src []byte) []int {
	starts := []int{0}
	for i, b := range src {
		if b == '\n' || b == '\r' && (i+1 == len(src) || src[i+1] != '\n') {
			starts = append(starts, i+1)
		}
	}
	return starts
}

// lineContentEnd is the byte offset where the text of line (0-based) ends,
// before its line break.
func (f *File) lineContentEnd(line int) int {
	if line+1 >= len(f.lines) {
		return len(f.Src)
	}
	start, end := f.lines[line], f.lines[line+1]
	end-- // the "\n", or a bare "\r"
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

// ToUTF16 converts p to a 0-based line and a column in UTF-16 units.
func (f *File) ToUTF16(p Pos) (line, char int) {
	p = f.PosAt(p.Offset)
	line = p.Line - 1
	for i := f.lines[line]; i < p.Offset; {
		u, size := units(f.Src[i:p.Offset])
		char += u
		i += size
	}
	return line, char
}

// FromUTF16 converts a 0-based line and UTF-16 column to a Pos. A column past
// the end of the line clamps to the line end (before its line break); a line past
// the last clamps to EOF.
func (f *File) FromUTF16(line, char int) Pos {
	if line >= len(f.lines) {
		return f.PosAt(len(f.Src))
	}
	if line < 0 {
		line = 0
	}
	if char < 0 {
		char = 0
	}
	start := f.lines[line]
	end := f.lineContentEnd(line)
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
	lines map[int][]checkpoint         // by line index, for lines longer than checkpointEvery
	last  struct{ line, off, col int } // a rune boundary and its column, from the previous call
}

type checkpoint struct{ off, col int }

// NewConverter returns a Converter for f.
func (f *File) NewConverter() *Converter { return &Converter{f: f} }

// ToUTF16 is File.ToUTF16.
func (c *Converter) ToUTF16(p Pos) (line, char int) {
	f := c.f
	p = f.PosAt(p.Offset)
	line = p.Line - 1
	i := f.lines[line]
	if p.Offset-i <= checkpointEvery {
		return f.ToUTF16(p)
	}
	cps, ok := c.lines[line]
	if !ok {
		cps = f.checkpoints(line)
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
	if line+1 < len(f.lines) {
		end = f.lines[line+1]
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
func (f *File) checkpoints(line int) []checkpoint {
	start := f.lines[line]
	end := len(f.Src)
	if line+1 < len(f.lines) {
		end = f.lines[line+1]
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

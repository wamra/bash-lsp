package tree

import (
	"sort"
	"unicode/utf8"
)

func lineStarts(src []byte) []int {
	starts := []int{0}
	for i, b := range src {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
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
// the end of the line clamps to the line end (before any \r\n); a line past
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
	end := len(f.Src)
	if line+1 < len(f.lines) {
		end = f.lines[line+1] - 1 // the \n
		if end > start && f.Src[end-1] == '\r' {
			end--
		}
	}
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

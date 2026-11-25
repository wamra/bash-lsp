package tree

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// lspLine is one line of a text split by the LSP rule: a break is "\r\n",
// "\n", or a "\r" not followed by "\n".
type lspLine struct{ start, end int } // content byte range, break excluded

func splitLSP(src string) []lspLine {
	var out []lspLine
	start := 0
	for i := 0; i < len(src); i++ {
		switch {
		case src[i] == '\r' && i+1 < len(src) && src[i+1] == '\n':
			out = append(out, lspLine{start, i})
			i++
			start = i + 1
		case src[i] == '\n' || src[i] == '\r':
			out = append(out, lspLine{start, i})
			start = i + 1
		}
	}
	return append(out, lspLine{start, len(src)})
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

func TestLineBreaksFollowLSP(t *testing.T) {
	long := strings.Repeat("x😀é", checkpointEvery) // a long line, past several checkpoints
	cases := map[string]string{
		"lf":            "a\nb\nc",
		"crlf":          "a\r\nb\r\nc",
		"bare cr":       "a\rb\rc",
		"mixed":         "a\nb\r\nc\rd",
		"cr at eof":     "abc\r",
		"crcrlf":        "a\r\r\nb",
		"crlf at eof":   "a\r\n",
		"only cr":       "\r",
		"crcr":          "\r\r",
		"empty":         "",
		"astral":        "😀\r😀a\n\U0001F600\r\nz",
		"long with cr":  long + "\r" + long + "\n" + long + "\r\n" + long,
		"long cr first": "\r" + long + "\r\r\n" + long + "\r",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			f := NewFile("/f.sh", []byte(text))
			c := f.NewConverter()
			lines := splitLSP(text)
			for li, l := range lines {
				// every rune boundary of the content, and the content end
				for off := l.start; off <= l.end; {
					wantChar := utf16Len(text[l.start:off])
					p := f.PosAt(off)
					if p.Line != li+1 {
						t.Fatalf("PosAt(%d).Line = %d, want %d", off, p.Line, li+1)
					}
					if p.Col != off-l.start+1 {
						t.Fatalf("PosAt(%d).Col = %d, want %d", off, p.Col, off-l.start+1)
					}
					if gl, gc := f.ToUTF16(p); gl != li || gc != wantChar {
						t.Fatalf("ToUTF16(%d) = (%d,%d), want (%d,%d)", off, gl, gc, li, wantChar)
					}
					if gl, gc := c.ToUTF16(p); gl != li || gc != wantChar {
						t.Fatalf("Converter.ToUTF16(%d) = (%d,%d), want (%d,%d)", off, gl, gc, li, wantChar)
					}
					if g := f.FromUTF16(li, wantChar); g.Offset != off || g.Line != li+1 {
						t.Fatalf("FromUTF16(%d,%d) = offset %d line %d, want offset %d line %d", li, wantChar, g.Offset, g.Line, off, li+1)
					}
					if off == l.end {
						break
					}
					_, size := utf8.DecodeRuneInString(text[off:l.end])
					off += size
				}
				// a column past the line end clamps to the content end, before the break
				if g := f.FromUTF16(li, 1<<20); g.Offset != l.end || g.Line != li+1 {
					t.Fatalf("FromUTF16(%d, huge) = offset %d line %d, want offset %d line %d", li, g.Offset, g.Line, l.end, li+1)
				}
			}
			// a line past the last clamps to EOF
			if g := f.FromUTF16(len(lines), 0); g.Offset != len(text) {
				t.Fatalf("FromUTF16(past last line) = %d, want %d", g.Offset, len(text))
			}
		})
	}
}

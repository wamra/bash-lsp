package tree

import (
	"bytes"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
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

func TestUTF16ConversionFollowsLSPLines(t *testing.T) {
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
					// Pos.Line and Pos.Col count "\n" only, whatever the LSP line is
					if wl := 1 + strings.Count(text[:off], "\n"); p.Line != wl {
						t.Fatalf("PosAt(%d).Line = %d, want %d", off, p.Line, wl)
					}
					if wc := off - (strings.LastIndexByte(text[:off], '\n') + 1) + 1; p.Col != wc {
						t.Fatalf("PosAt(%d).Col = %d, want %d", off, p.Col, wc)
					}
					if gl, gc := f.ToUTF16(p); gl != li || gc != wantChar {
						t.Fatalf("ToUTF16(%d) = (%d,%d), want (%d,%d)", off, gl, gc, li, wantChar)
					}
					if gl, gc := c.ToUTF16(p); gl != li || gc != wantChar {
						t.Fatalf("Converter.ToUTF16(%d) = (%d,%d), want (%d,%d)", off, gl, gc, li, wantChar)
					}
					if g := f.FromUTF16(li, wantChar); g.Offset != off {
						t.Fatalf("FromUTF16(%d,%d) = offset %d, want %d", li, wantChar, g.Offset, off)
					}
					if off == l.end {
						break
					}
					_, size := utf8.DecodeRuneInString(text[off:l.end])
					off += size
				}
				// a column past the line end clamps to the content end, before the break
				if g := f.FromUTF16(li, 1<<20); g.Offset != l.end {
					t.Fatalf("FromUTF16(%d, huge) = offset %d, want %d", li, g.Offset, l.end)
				}
			}
			// a line past the last clamps to EOF
			if g := f.FromUTF16(len(lines), 0); g.Offset != len(text) {
				t.Fatalf("FromUTF16(past last line) = %d, want %d", g.Offset, len(text))
			}
		})
	}
}

// Without a lone "\r" the two line tables are the same.
func TestLineTablesAgreeWithoutLoneCR(t *testing.T) {
	same := func(t *testing.T, name string, src []byte) {
		a, b := lineStarts(src), lspLineStarts(src)
		if len(a) != len(b) {
			t.Fatalf("%s: %d lines vs %d LSP lines", name, len(a), len(b))
		}
		for i := range a {
			if a[i] != b[i] {
				t.Fatalf("%s: line %d starts at %d vs %d", name, i, a[i], b[i])
			}
		}
	}
	rnd := rand.New(rand.NewSource(7))
	alphabet := []string{"a", " ", "é", "😀", "\n", "\r\n", "\n\n", "\xff"}
	for round := 0; round < 500; round++ {
		var sb strings.Builder
		for n := rnd.Intn(60); n > 0; n-- {
			sb.WriteString(alphabet[rnd.Intn(len(alphabet))])
		}
		same(t, "random", []byte(sb.String()))
	}
	n := 0
	filepath.WalkDir("../testdata", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".sh" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil || bytes.Contains(b, []byte("\r")) && hasLoneCR(b) {
			return nil
		}
		same(t, path, b)
		n++
		return nil
	})
	if n == 0 {
		t.Fatal("no shell files under ../testdata")
	}
}

func hasLoneCR(b []byte) bool {
	for i, c := range b {
		if c == '\r' && (i+1 == len(b) || b[i+1] != '\n') {
			return true
		}
	}
	return false
}

// LSPLine counts LF, CRLF and a lone CR as line breaks; a break belongs to the
// line it ends.
func TestLSPLine(t *testing.T) {
	src := "a\rb\r\n\r\r\nc\n"
	f := NewFile("x.sh", []byte(src))
	for off, want := range []int{0, 0, 1, 1, 1, 2, 3, 3, 4, 4} {
		if got := f.LSPLine(off); got != want {
			t.Errorf("offset %d (%q): line %d, want %d", off, src[off:off+1], got, want)
		}
	}
	if f.LSPLine(-3) != 0 || f.LSPLine(len(src)) != 5 || f.LSPLine(len(src)+9) != 5 {
		t.Errorf("clamping: %d %d %d", f.LSPLine(-3), f.LSPLine(len(src)), f.LSPLine(len(src)+9))
	}
}

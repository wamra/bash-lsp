package tree

import (
	"math/rand"
	"strings"
	"testing"
)

// The Converter must agree with File.ToUTF16 at every offset, in any order,
// including offsets inside a multibyte rune and invalid bytes.
func TestConverterMatchesToUTF16(t *testing.T) {
	alphabet := []string{"a", "b", " ", "é", "✓", "😀", "\xff", "\xe2\x82", "\r", "\n", "\r\n", "\x00"}
	rnd := rand.New(rand.NewSource(1))
	for round := 0; round < 40; round++ {
		var sb strings.Builder
		for sb.Len() < 3000+rnd.Intn(6000) {
			n := 1
			if rnd.Intn(50) == 0 {
				n = 50 + rnd.Intn(2000) // long newline-free runs, so that lines pass the checkpoint spacing
			}
			s := alphabet[rnd.Intn(len(alphabet)-4)] // no line breaks inside a run
			if rnd.Intn(8) == 0 {
				s = alphabet[rnd.Intn(len(alphabet))]
			}
			sb.WriteString(strings.Repeat(s, n))
		}
		f := NewFile("/f.sh", []byte(sb.String()))
		c, tc := f.NewConverter(), f.NewToolConverter()
		for k := 0; k < 3000; k++ {
			p := Pos{Offset: rnd.Intn(len(f.Src) + 20)}
			wl, wc := f.ToUTF16(p)
			gl, gc := c.ToUTF16(p)
			if wl != gl || wc != gc {
				t.Fatalf("round %d offset %d: converter (%d,%d), ToUTF16 (%d,%d)", round, p.Offset, gl, gc, wl, wc)
			}
			wl, wc = f.ToolUTF16(p)
			gl, gc = tc.ToUTF16(p)
			if wl != gl || wc != gc {
				t.Fatalf("round %d offset %d: tool converter (%d,%d), ToolUTF16 (%d,%d)", round, p.Offset, gl, gc, wl, wc)
			}
		}
	}
}

// In source order, the way references arrive, with every offset of a long
// mixed line: the continue-from-the-last-answer path.
func TestConverterSortedMatchesToUTF16(t *testing.T) {
	line := strings.Repeat("ab é✓😀\xff\xe2\x82 ", 900) + "\nsecond é line\n"
	f := NewFile("/f.sh", []byte(line))
	c := f.NewConverter()
	for off := 0; off <= len(f.Src); off++ {
		wl, wc := f.ToUTF16(Pos{Offset: off})
		gl, gc := c.ToUTF16(Pos{Offset: off})
		if wl != gl || wc != gc {
			t.Fatalf("offset %d: converter (%d,%d), ToUTF16 (%d,%d)", off, gl, gc, wl, wc)
		}
	}
}

// The tool converter on a long line that holds lone CRs, which end an LSP
// line but not a tool line: every offset, in source order and then back to
// front, agrees with ToolUTF16; the LSP converter agrees with ToUTF16.
func TestToolConverterLongLine(t *testing.T) {
	long := strings.Repeat("ab é✓😀\xff\xe2\x82 ", 900)
	src := long + "\r" + long + "\r\r" + long + "\nsecond é line\r\n" + long + "\r"
	f := NewFile("/f.sh", []byte(src))
	check := func(off int, tc, c *Converter) {
		wl, wc := f.ToolUTF16(Pos{Offset: off})
		gl, gc := tc.ToUTF16(Pos{Offset: off})
		if wl != gl || wc != gc {
			t.Fatalf("offset %d: tool converter (%d,%d), ToolUTF16 (%d,%d)", off, gl, gc, wl, wc)
		}
		wl, wc = f.ToUTF16(Pos{Offset: off})
		gl, gc = c.ToUTF16(Pos{Offset: off})
		if wl != gl || wc != gc {
			t.Fatalf("offset %d: converter (%d,%d), ToUTF16 (%d,%d)", off, gl, gc, wl, wc)
		}
	}
	tc, c := f.NewToolConverter(), f.NewConverter()
	for off := 0; off <= len(src); off++ {
		check(off, tc, c)
	}
	tc, c = f.NewToolConverter(), f.NewConverter()
	for off := len(src); off >= 0; off-- {
		check(off, tc, c)
	}
}

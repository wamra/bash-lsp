package tree

import (
	"bytes"
	"testing"
)

func TestUTF16RoundTrip(t *testing.T) {
	src := []byte("echo \"é😀\"; foo\r\nbar\n") // é = 2 bytes/1 unit, 😀 = 4 bytes/2 units
	f := NewFile("x.sh", src)
	off := bytes.Index(src, []byte("foo"))
	line, char := f.ToUTF16(f.PosAt(off))
	if line != 0 || char != 12 {
		t.Fatalf("got %d:%d", line, char)
	} // e,c,h,o,space,",é,😀(2),",;,space = 12
	if got := f.FromUTF16(0, 12).Offset; got != off {
		t.Fatalf("offset %d want %d", got, off)
	}
	if p := f.PosAt(bytes.Index(src, []byte("bar"))); p.Line != 2 || p.Col != 1 {
		t.Fatalf("%+v", p)
	}
}

func TestFromUTF16Clamps(t *testing.T) {
	f := NewFile("x.sh", []byte("ab\r\ncd"))
	if got := f.FromUTF16(0, 99).Offset; got != 2 {
		t.Fatalf("got %d", got)
	} // before the \r
	if got := f.FromUTF16(99, 0).Offset; got != 6 {
		t.Fatalf("got %d", got)
	} // EOF
}

func TestInvalidUTF8DoesNotPanic(t *testing.T) {
	f := NewFile("x.sh", []byte("echo \xff\xfe; foo\n"))
	_, c := f.ToUTF16(f.PosAt(12)) // each invalid byte counts as one UTF-16 unit
	if c != 12 {
		t.Fatalf("char %d", c)
	}
}

func TestPosAtClampsAndColumns(t *testing.T) {
	f := NewFile("x.sh", []byte("é\nab"))
	if p := f.PosAt(-5); p != (Pos{0, 1, 1}) {
		t.Fatalf("%+v", p)
	}
	if p := f.PosAt(99); p != (Pos{5, 2, 3}) {
		t.Fatalf("%+v", p)
	}
	if p := f.PosAt(2); p != (Pos{2, 1, 3}) { // the \n itself, col counts bytes
		t.Fatalf("%+v", p)
	}
}

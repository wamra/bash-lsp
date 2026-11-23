package tree

import "testing"

// build: File[0,20) > Call[2,12) > Lit[5,8) ; File > Comment[14,18)
func sample() (*File, *Node, *Node, *Node) {
	f := NewFile("x.sh", []byte("0123456789012345678\n"))
	call := &Node{Kind: KCall, Range: Range{Start: f.PosAt(2), End: f.PosAt(12)}, Parent: f.Root}
	lit := &Node{Kind: KLit, Range: Range{Start: f.PosAt(5), End: f.PosAt(8)}, Parent: call}
	com := &Node{Kind: KComment, Range: Range{Start: f.PosAt(14), End: f.PosAt(18)}, Parent: f.Root}
	call.Children = []*Node{lit}
	f.Root.Children = []*Node{call, com}
	return f, call, lit, com
}

func TestWalkOrder(t *testing.T) {
	f, _, _, _ := sample()
	var got []Kind
	f.Root.Walk(func(n *Node) bool { got = append(got, n.Kind); return true })
	want := []Kind{KFile, KCall, KLit, KComment}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestWalkSkipsChildren(t *testing.T) {
	f, _, _, _ := sample()
	var got []Kind
	f.Root.Walk(func(n *Node) bool { got = append(got, n.Kind); return n.Kind != KCall })
	want := []Kind{KFile, KCall, KComment}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestEnclosing(t *testing.T) {
	f, call, lit, com := sample()
	if lit.Enclosing(KCall) != call || lit.Enclosing(KFile) != f.Root {
		t.Fatal("lit ancestors")
	}
	if lit.Enclosing(KLit) != nil { // ancestors only, not self
		t.Fatal("self returned")
	}
	if com.Enclosing(KCall) != nil {
		t.Fatal("comment has no call ancestor")
	}
}

func TestNodeAt(t *testing.T) {
	f, call, lit, com := sample()
	cases := []struct {
		off  int
		want *Node
	}{{6, lit}, {3, call}, {8, call}, {15, com}, {0, f.Root}, {12, f.Root}, {99, f.Root}}
	for _, c := range cases {
		if got := f.NodeAt(c.off); got != c.want {
			t.Errorf("NodeAt(%d)=%v want %v", c.off, got.Kind, c.want.Kind)
		}
	}
}

func TestRangeContains(t *testing.T) {
	r := Range{Start: Pos{Offset: 2}, End: Pos{Offset: 5}}
	if !r.Contains(2) || !r.Contains(4) || r.Contains(5) || r.Contains(1) {
		t.Fatal("half-open")
	}
	z := Range{Start: Pos{Offset: 3}, End: Pos{Offset: 3}}
	if !z.Contains(3) || z.Contains(4) {
		t.Fatal("zero width")
	}
}

func TestKindString(t *testing.T) {
	if KCall.String() != "Call" || KFuncDecl.String() != "FuncDecl" || KComment.String() != "Comment" {
		t.Fatal("names")
	}
}

func TestNodeAtOverlappingSiblings(t *testing.T) {
	f := NewFile("o.sh", []byte("0123456789"))
	rg := func(a, b int) Range { return Range{Start: f.PosAt(a), End: f.PosAt(b)} }
	first := &Node{Kind: KStmt, Range: rg(0, 9), Parent: f.Root}
	second := &Node{Kind: KStmt, Range: rg(4, 6), Parent: f.Root}
	f.Root.Children = []*Node{first, second}
	if got := f.NodeAt(5); got != second {
		t.Errorf("offset 5: want the later-starting sibling")
	}
	if got := f.NodeAt(2); got != first {
		t.Errorf("offset 2: want first")
	}
	if got := f.NodeAt(8); got != first {
		t.Errorf("offset 8: want first")
	}
	tie := &Node{Kind: KStmt, Range: rg(4, 7), Parent: f.Root}
	f.Root.Children = append(f.Root.Children, tie)
	if got := f.NodeAt(5); got != tie {
		t.Errorf("tie on start: want the later child")
	}
}

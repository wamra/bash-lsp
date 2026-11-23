package tree

import (
	"math/rand"
	"testing"
)

// randomTree builds a tree whose sibling ranges are sometimes unsorted, nested,
// overlapping or zero-width, to hold Locator to NodeAt's exact semantics.
func randomTree(r *rand.Rand, size int) *File {
	src := make([]byte, size)
	f := NewFile("r.sh", src)
	var fill func(parent *Node, lo, hi, depth int)
	fill = func(parent *Node, lo, hi, depth int) {
		if hi <= lo {
			return
		}
		n := r.Intn(40)
		if depth > 3 || hi-lo < 4 {
			n = r.Intn(3)
		}
		for i := 0; i < n; i++ {
			s := lo + r.Intn(hi-lo)
			e := s + r.Intn(hi-s+1)
			if r.Intn(8) == 0 {
				e = s // zero width
			}
			c := &Node{Kind: KCall, Range: Range{Start: f.PosAt(s), End: f.PosAt(e)}, Parent: parent}
			parent.Children = append(parent.Children, c)
			fill(c, s, e, depth+1)
		}
		if r.Intn(3) != 0 { // mostly source order, like real trees
			for i := 1; i < len(parent.Children); i++ {
				for j := i; j > 0 && parent.Children[j].Range.Start.Offset < parent.Children[j-1].Range.Start.Offset; j-- {
					parent.Children[j], parent.Children[j-1] = parent.Children[j-1], parent.Children[j]
				}
			}
		}
	}
	fill(f.Root, 0, size, 0)
	return f
}

func TestLocatorMatchesNodeAt(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for round := 0; round < 200; round++ {
		f := randomTree(r, 60+r.Intn(200))
		loc := f.NewLocator()
		for off := 0; off <= len(f.Src); off++ {
			if got, want := loc.At(off), f.NodeAt(off); got != want {
				t.Fatalf("round %d offset %d: Locator %p, NodeAt %p", round, off, got, want)
			}
		}
	}
}

// A file of n sibling statements: locating every comment must be near linear.
func BenchmarkLocatorWide(b *testing.B) {
	const n = 4000
	f := NewFile("w.sh", make([]byte, n*10))
	for i := 0; i < n; i++ {
		f.Root.Children = append(f.Root.Children,
			&Node{Kind: KCall, Range: Range{Start: f.PosAt(i * 10), End: f.PosAt(i*10 + 6)}, Parent: f.Root})
	}
	b.Run("NodeAt", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			for k := 0; k < n; k++ {
				f.NodeAt(k*10 + 8)
			}
		}
	})
	b.Run("Locator", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			loc := f.NewLocator()
			for k := 0; k < n; k++ {
				loc.At(k*10 + 8)
			}
		}
	})
}

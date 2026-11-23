package tree

import "sort"

// Locator answers NodeAt queries in O(log n) per tree level instead of O(n),
// for callers that make many queries against one tree (every comment of a file).
// It returns exactly what File.NodeAt returns. The tree must not change while a
// Locator is in use.
type Locator struct {
	f   *File
	idx map[*Node]*levelIndex
}

// levelIndex describes one node's children. reach[i] is the largest end offset
// (a zero-width child counts as start+1) among children[0..i]. A nil
// levelIndex means the children are not sorted by start, so scan linearly.
type levelIndex struct {
	reach []int
}

// NewLocator returns a Locator for f.
func (f *File) NewLocator() *Locator { return &Locator{f: f, idx: map[*Node]*levelIndex{}} }

const linearBelow = 8 // narrower levels are scanned directly

func (l *Locator) level(n *Node) *levelIndex {
	if li, ok := l.idx[n]; ok {
		return li
	}
	var li *levelIndex
	if len(n.Children) >= linearBelow {
		reach := make([]int, len(n.Children))
		max, last := -1, -1
		li = &levelIndex{reach: reach}
		for i, c := range n.Children {
			s := c.Range.Start.Offset
			if s < last {
				li = nil
				break
			}
			last = s
			e := c.Range.End.Offset
			if e == s {
				e = s + 1
			}
			if e > max {
				max = e
			}
			reach[i] = max
		}
	}
	l.idx[n] = li
	return li
}

// At is File.NodeAt, accelerated.
func (l *Locator) At(offset int) *Node {
	n := l.f.Root
	for {
		var next *Node
		if li := l.level(n); li == nil {
			for _, c := range n.Children {
				if c.Range.Contains(offset) && (next == nil || c.Range.Start.Offset >= next.Range.Start.Offset) {
					next = c
				}
			}
		} else {
			// Children are sorted by start. Candidates start at or before
			// offset; walk back from the last of them until no earlier child
			// can reach offset. The first containing child found has the
			// greatest start, and on a tie the later index, as NodeAt picks.
			kids := n.Children
			hi := sort.Search(len(kids), func(i int) bool { return kids[i].Range.Start.Offset > offset })
			for i := hi - 1; i >= 0 && li.reach[i] > offset; i-- {
				if kids[i].Range.Contains(offset) {
					next = kids[i]
					break
				}
			}
		}
		if next == nil {
			return n
		}
		n = next
	}
}

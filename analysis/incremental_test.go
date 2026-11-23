package analysis

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wamra/bash-lsp/internal/front"
)

// sharesVisibility reports whether s uses the very visibility sets of p.
func (s *Snapshot) sharesVisibility(p *Snapshot) bool {
	return len(s.visible) > 0 && len(p.visible) > 0 && &s.visible[0] == &p.visible[0]
}

func memFiles(root string, srcs map[string]string) map[string]*FileFacts {
	files := map[string]*FileFacts{}
	for name, s := range srcs {
		p := filepath.Join(root, filepath.FromSlash(name))
		b := []byte(s)
		files[p] = Bind(front.Parse(p, b, front.Detect(p, b)))
	}
	return files
}

func visibleOf(s *Snapshot) map[string][]string {
	out := map[string][]string{}
	for _, p := range s.Paths() {
		out[p] = s.Visible(p)
	}
	return out
}

// An edit that leaves the source graph alone reuses the previous visibility;
// one that changes it recomputes. Either way the answers equal a build from scratch.
func TestBuildReusesVisibilityOnlyWhenGraphUnchanged(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ws")
	base := map[string]string{
		"a.sh": "source ./b.sh\nfa() { fb; }\n",
		"b.sh": "source ./c.sh\nfb() { fc; }\n",
		"c.sh": "fc() { :; }\n",
		"d.sh": "fd() { :; }\n",
	}
	opt := Options{Root: root}
	first := Build(memFiles(root, base), opt, 1)

	// Same graph, different body: visibility is shared with the previous snapshot.
	edited := map[string]string{"a.sh": base["a.sh"], "b.sh": "source ./c.sh\n# note\nfb() { fc; }\n", "c.sh": base["c.sh"], "d.sh": base["d.sh"]}
	opt.Prev = first
	second := Build(memFiles(root, edited), opt, 2)
	fresh := Build(memFiles(root, edited), Options{Root: root}, 2)
	if !second.sharesVisibility(first) {
		t.Fatal("graph unchanged but visibility was recomputed")
	}
	if !reflect.DeepEqual(visibleOf(second), visibleOf(fresh)) || !reflect.DeepEqual(second.incomplete, fresh.incomplete) {
		t.Fatal("reused visibility differs from a fresh build")
	}

	// New source edge: visibility must change.
	linked := map[string]string{"a.sh": base["a.sh"], "b.sh": base["b.sh"], "c.sh": base["c.sh"], "d.sh": "source ./a.sh\nfd() { :; }\n"}
	opt.Prev = second
	third := Build(memFiles(root, linked), opt, 3)
	fresh = Build(memFiles(root, linked), Options{Root: root}, 3)
	if third.sharesVisibility(second) {
		t.Fatal("graph changed but visibility was reused")
	}
	if !reflect.DeepEqual(visibleOf(third), visibleOf(fresh)) {
		t.Fatal("visibility after a new edge differs from a fresh build")
	}

	// An unresolvable source marks the file incomplete; that is part of the key.
	broken := map[string]string{"a.sh": base["a.sh"], "b.sh": base["b.sh"], "c.sh": "source \"$x\"\nfc() { :; }\n", "d.sh": base["d.sh"]}
	opt.Prev = first
	fourth := Build(memFiles(root, broken), opt, 4)
	fresh = Build(memFiles(root, broken), Options{Root: root}, 4)
	if fourth.sharesVisibility(first) {
		t.Fatal("a newly unresolved source reused the earlier visibility")
	}
	if !reflect.DeepEqual(fourth.incomplete, fresh.incomplete) || !fresh.incomplete[0] {
		t.Fatalf("incomplete flags %v, fresh %v", fourth.incomplete, fresh.incomplete)
	}

	// Files added or removed: nothing is reused.
	opt.Prev = first
	fewer := Build(memFiles(root, map[string]string{"a.sh": base["a.sh"], "b.sh": base["b.sh"]}), opt, 5)
	if fewer.sharesVisibility(first) {
		t.Fatal("file set changed but visibility was reused")
	}
}

package analysis

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/internal/front"
)

// A snapshot may hold files above the root it was built with (a root and a
// sibling folder of another workspace). Completion of a source path never
// offers them.
func TestCompleteSourcePathStaysInsideRoot(t *testing.T) {
	top := filepath.Join(t.TempDir(), "top")
	root := filepath.Join(top, "ws")
	mk := func(p, src string) *FileFacts {
		return Bind(front.Parse(p, []byte(src), front.Detect(p, []byte(src))))
	}
	main := filepath.Join(root, "main.sh")
	files := map[string]*FileFacts{
		main:                             mk(main, "source ../"),
		filepath.Join(top, "outside.sh"): mk(filepath.Join(top, "outside.sh"), "x=1\n"),
		filepath.Join(root, "inside.sh"): mk(filepath.Join(root, "inside.sh"), "y=1\n"),
	}
	s := Build(files, Options{Root: root}, 1)
	if ctx, _, items, _ := s.Completions(main, len("source ../")); ctx != CtxSourcePath || len(items) != 0 {
		t.Fatalf("../ offered %v (ctx %d)", labels(items), ctx)
	}
	files[main] = mk(main, "source ./")
	s = Build(files, Options{Root: root}, 1)
	_, _, items, _ := s.Completions(main, len("source ./"))
	if _, ok := has(items, "inside.sh"); !ok {
		t.Fatalf("inside.sh missing: %v", labels(items))
	}
	if _, ok := has(items, "outside.sh"); ok {
		t.Fatalf("a file above the root offered: %v", labels(items))
	}
}

// A snapshot that holds a "file" at a directory path would give the entries
// "." and "..": they are never offered.
func TestCompleteSourcePathNeverOffersDotEntries(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ws")
	mk := func(p, src string) *FileFacts {
		return Bind(front.Parse(p, []byte(src), front.Detect(p, []byte(src))))
	}
	main := filepath.Join(root, "main.sh")
	sub := filepath.Join(root, "sub")
	files := map[string]*FileFacts{
		main:                          mk(main, "source ./sub/"),
		sub:                           mk(sub, "x=1\n"), // a file where the folder is
		filepath.Join(sub, "real.sh"): mk(filepath.Join(sub, "real.sh"), "y=1\n"),
	}
	s := Build(files, Options{Root: root}, 1)
	_, _, items, _ := s.Completions(main, len("source ./sub/"))
	for _, it := range items {
		if it.Label == "." || it.Label == ".." || it.Label == "./" || it.Label == "../" {
			t.Fatalf("dot entry offered: %v", labels(items))
		}
	}
	if _, ok := has(items, "real.sh"); !ok {
		t.Fatalf("real.sh missing: %v", labels(items))
	}
	// The directory above: the root itself as a "file" of the snapshot.
	files[root] = mk(root, "z=1\n")
	files[main] = mk(main, "source ./")
	s = Build(files, Options{Root: root}, 1)
	_, _, items, _ = s.Completions(main, len("source ./"))
	for _, it := range items {
		if strings.HasPrefix(it.Label, ".") {
			t.Fatalf("dot entry offered: %v", labels(items))
		}
	}
}

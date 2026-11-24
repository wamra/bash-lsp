package bashlsp_test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp"
	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/parse"
	"github.com/wamra/bash-lsp/tree"
)

// The consumer program of docs/library.md ("Example consumer"), with the
// root as an argument instead of os.Args[1]. TestLibraryDocExample keeps the
// document's code and output equal to this file's.

func Example_consumer() {
	consumer(filepath.Join("testdata", "corpus"))
	// Output:
	// definition: lib/util.sh:4:1
	// dialect: bash
	// references: 7
	//   dyn.sh:4:1
	//   main.sh:3:10
	//   main.sh:6:3
	//   main.sh:7:3
	//   main.sh:8:13
	//   main.sh:9:3
	//   main.sh:10:9
}

func consumer(arg string) {
	root, err := filepath.Abs(arg)
	if err != nil {
		panic(err)
	}
	snap, err := bashlsp.Load(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	hits := snap.WorkspaceSymbols("build_release", 5)
	if len(hits) == 0 {
		fmt.Println("build_release not found")
		return
	}
	def := hits[0]
	fmt.Printf("definition: %s:%d:%d\n", rel(root, def.Path), def.Range.Start.Line, def.Range.Start.Col)

	// References are answered for a position, so find the name inside the
	// definition with the public parser, then ask the snapshot.
	src, err := os.ReadFile(def.Path)
	if err != nil {
		panic(err)
	}
	f := parse.File(def.Path, src)
	off := nameOffset(f, "build_release")
	fmt.Printf("dialect: %s\n", f.Dialect)
	show(root, "references", snap.References(def.Path, off, false))
}

func show(root, label string, locs []analysis.Location) {
	fmt.Printf("%s: %d\n", label, len(locs))
	for _, l := range locs {
		fmt.Printf("  %s:%d:%d\n", rel(root, l.Path), l.Range.Start.Line, l.Range.Start.Col)
	}
}

// nameOffset is the offset of the first function declaration named name.
func nameOffset(f *tree.File, name string) int {
	var walk func(n *tree.Node) int
	walk = func(n *tree.Node) int {
		if n.Kind == tree.KFuncDecl && n.Name == name {
			return n.Range.Start.Offset
		}
		for _, c := range n.Children {
			if o := walk(c); o >= 0 {
				return o
			}
		}
		return -1
	}
	return walk(f.Root)
}

func rel(root, p string) string {
	if r, err := filepath.Rel(root, p); err == nil {
		return filepath.ToSlash(r)
	}
	return p
}

// fenced returns the body of the first fenced block of md opening with
// fence that follows the line containing after.
func fenced(t *testing.T, md, after, fence string) string {
	t.Helper()
	i := strings.Index(md, after)
	if i < 0 {
		t.Fatalf("docs/library.md has no %q", after)
	}
	rest := md[i:]
	j := strings.Index(rest, "\n"+fence+"\n")
	if j < 0 {
		t.Fatalf("no %s block after %q", fence, after)
	}
	body := rest[j+len(fence)+2:]
	k := strings.Index(body, "\n```\n")
	if k < 0 {
		t.Fatalf("unclosed block after %q", after)
	}
	return body[:k+1]
}

// TestLibraryDocExample: the consumer's code in docs/library.md, from its
// first statement after loading, is in this file line for line, and the
// output the document shows is what that code prints.
func TestLibraryDocExample(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("docs", "library.md"))
	if err != nil {
		t.Fatal(err)
	}
	md := strings.ReplaceAll(string(b), "\r\n", "\n")
	code := fenced(t, md, "Its `main.go`:", "```go")
	self, err := os.ReadFile("example_test.go")
	if err != nil {
		t.Fatal(err)
	}
	from := strings.Index(code, "\thits := ")
	if from < 0 {
		t.Fatal("the document's main.go has no `hits :=` line")
	}
	src := strings.ReplaceAll(string(self), "\r\n", "\n")
	for _, line := range strings.Split(code[from:], "\n") {
		if strings.TrimSpace(line) != "" && !strings.Contains(src, line+"\n") {
			t.Errorf("document line not in example_test.go: %q", line)
		}
	}

	want := fenced(t, md, "Output of `./consumer testdata/corpus`", "```")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	done := make(chan []byte)
	go func() { out, _ := io.ReadAll(r); done <- out }()
	consumer(filepath.Join("testdata", "corpus"))
	os.Stdout = stdout
	w.Close()
	if got := <-done; !bytes.Equal(got, []byte(want)) {
		t.Errorf("the document shows:\n%s\nthe code prints:\n%s", want, got)
	}
}

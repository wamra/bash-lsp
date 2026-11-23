package parse_test

import (
	"testing"

	"github.com/wamra/bash-lsp/parse"
	"github.com/wamra/bash-lsp/tree"
)

func TestFileDetectsDialect(t *testing.T) {
	cases := []struct {
		path, src string
		want      tree.Dialect
	}{
		{"a.bats", "@test \"x\" {\n  true\n}\n", tree.Bats},
		{"run", "#!/bin/sh\necho hi\n", tree.POSIX},
		{"x.sh", "echo hi\n", tree.Bash},
	}
	for _, c := range cases {
		f := parse.File(c.path, []byte(c.src))
		if f == nil {
			t.Fatalf("%s: nil file", c.path)
		}
		if f.Dialect != c.want {
			t.Errorf("%s: Dialect = %v, want %v", c.path, f.Dialect, c.want)
		}
		if got := parse.Detect(c.path, []byte(c.src)); got != c.want {
			t.Errorf("%s: Detect = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestFileAsForcesDialect(t *testing.T) {
	f := parse.FileAs("a.sh", []byte("echo hi\n"), tree.Mksh)
	if f.Dialect != tree.Mksh || tree.Mksh.String() != "mksh" {
		t.Fatalf("Dialect = %v", f.Dialect)
	}
}

func TestFileSetsDialectWhenSkipped(t *testing.T) {
	f := parse.File("a.zsh", []byte("echo hi\n"))
	if f.Skipped == "" || f.Dialect != tree.Zsh {
		t.Fatalf("Skipped %q Dialect %v", f.Skipped, f.Dialect)
	}
}

func TestFragment(t *testing.T) {
	f := parse.Fragment([]byte(`log "a b"`), tree.Bash)
	if f.Path != "" || f.Dialect != tree.Bash || f.Root == nil || len(f.Root.Children) != 1 {
		t.Fatalf("Fragment = %+v", f)
	}
	if r := f.Root.Children[0].Range; r.Start.Offset != 0 || r.End.Offset != 9 {
		t.Fatalf("statement range %+v", r)
	}
}

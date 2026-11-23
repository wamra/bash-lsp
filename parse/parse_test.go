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

package front

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/internal/front/syntax"
)

// nodeKey names a node by type and position; Pos panics on some partial nodes.
func nodeKey(n syntax.Node) (k string) {
	defer func() {
		if recover() != nil {
			k = fmt.Sprintf("%T@?", n)
		}
	}()
	return fmt.Sprintf("%T@%d", n, n.Pos().Offset())
}

// visitAll must reach every node syntax.Walk reaches, so replacing Walk lost
// no comment or quote. A node type added by a parser upgrade and missed in
// visitAll's switch shows up here. The one difference allowed: comments Walk
// skips after the first one trailing a statement or case item.
func TestVisitAllMatchesWalk(t *testing.T) {
	var srcs [][]byte
	for _, root := range []string{"../../testdata/corpus", "../../testdata/corpus-annotated", "../../testdata/broken", "testdata/fuzz/FuzzParse"} {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				if b, rerr := os.ReadFile(p); rerr == nil {
					srcs = append(srcs, b)
				}
			}
			return nil
		})
	}
	srcs = append(srcs, []byte(`f() { local a=1 # c
  case $x in (a|b) echo "$((a ? 1 : 2))" ;; *) : ;; esac # c2
  arr=( [1]=x # c3
    y ); time coproc n { cat <<E
$(echo ${a[$i]:1:2} ${b/x/y} @(p) <(q))
E
  }; [[ ! -f x && ( y || z ) ]]; let i++ j--; for ((i=0;i<3;i++)); do :; done
  case $y in
    # c5
    a) ;; # c6
    # c7
  esac
  if a; then b # c4
  elif c; then d; else e; fi; while f; do g; done; select s in t; do :; done
}
`))
	if len(srcs) < 20 {
		t.Fatalf("only %d inputs", len(srcs))
	}
	total := 0
	for i, src := range srcs {
		af, err := syntax.NewParser(syntax.KeepComments(true), syntax.RecoverErrors(50)).Parse(bytes.NewReader(src), "x.sh")
		if af == nil || err != nil && len(af.Stmts) == 0 {
			continue
		}
		var walked, visited []string
		func() {
			defer func() { _ = recover() }() // partial trees may hold nil nodes
			syntax.Walk(af, func(n syntax.Node) bool {
				if n != nil {
					walked = append(walked, nodeKey(n))
				}
				return true
			})
		}()
		func() {
			defer func() { _ = recover() }()
			visitAll(af, func(n syntax.Node) { visited = append(visited, nodeKey(n)) })
		}()
		total += len(walked)
		seen := map[string]int{}
		for _, k := range walked {
			seen[k]++
		}
		for _, k := range visited {
			seen[k]--
		}
		for k, v := range seen {
			// Walk stops at the first comment that trails a statement or case
			// item and skips the rest; visitAll keeps them. Nothing else differs.
			if v > 0 || v < 0 && !strings.HasPrefix(k, "*syntax.Comment@") {
				t.Fatalf("input %d: %s seen %d more times by Walk than by visitAll\n%q", i, k, v, src)
			}
		}
	}
	if total < 500 {
		t.Fatalf("compared only %d nodes", total)
	}
}

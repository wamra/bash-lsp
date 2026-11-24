package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/graphgolden from the current output")

// goldenRuns is every graph command, in text and JSON, on the corpus and on
// the small temp workspace (tempFiles).
var goldenRuns = []struct {
	name string
	args []string
	temp bool
}{
	{"status", []string{"status"}, false},
	{"status-temp", []string{"status"}, true},
	{"callers-util", []string{"callers", "lib/util.sh:4:1"}, false},
	{"callers-util-d3", []string{"callers", "lib/util.sh", "--depth", "3"}, false},
	{"callers-ambiguous", []string{"callers", "build_release"}, false},
	{"callees-main", []string{"callees", "main.sh"}, false},
	{"callees-main-d3", []string{"callees", "main.sh", "--depth", "3"}, false},
	{"callees-a", []string{"callees", "a.sh"}, true},
	{"refs-util", []string{"refs", "lib/util.sh:4:1"}, false},
	{"refs-g", []string{"refs", "g"}, true},
	{"refs-file", []string{"refs", "main.sh"}, false},
	{"impact-util", []string{"impact", "lib/util.sh:4:1"}, false},
	{"impact-a", []string{"impact", "a.sh"}, true},
	{"trace-found", []string{"trace", "main.sh", "lib/util.sh:3:1"}, false},
	{"trace-none", []string{"trace", "orphan.sh", "main.sh"}, false},
	{"unresolved", []string{"unresolved"}, false},
	{"unresolved-temp", []string{"unresolved"}, true},
	{"unresolved-path", []string{"unresolved", "a.sh"}, true},
	{"cycles", []string{"cycles"}, false},
	{"cycles-temp", []string{"cycles"}, true},
	{"outside", []string{"callers", "../../etc/passwd"}, false},
	{"missing", []string{"callers", "no_such_function"}, false},
}

// TestCLIOutputUnchanged pins the output of every graph command (stdout,
// stderr and exit code, text and --json) to what testdata/graphgolden holds,
// written before the encoders moved to internal/query.
func TestCLIOutputUnchanged(t *testing.T) {
	tempRoot := writeRoot(t, tempFiles)
	for _, r := range goldenRuns {
		for _, js := range []bool{false, true} {
			name := r.name + ".txt"
			if js {
				name = r.name + ".json"
			}
			root := corpus
			if r.temp {
				root = tempRoot
			}
			args := append(append([]string(nil), r.args...), "--root", root)
			if js {
				args = append(args, "--json")
			}
			code, out, errs := runCode(t, args...)
			got := fmt.Sprintf("exit %d\n--- stdout\n%s--- stderr\n%s", code, out, errs)
			got = strings.ReplaceAll(got, tempRoot, "TEMPROOT")
			if abs, err := filepath.Abs(corpus); err == nil {
				got = strings.ReplaceAll(got, abs, "CORPUS")
			}
			path := filepath.Join("testdata", "graphgolden", name)
			if *updateGolden {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				continue
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("%s differs from golden:\n--- got\n%s\n--- want\n%s", name, got, want)
			}
		}
	}
}

package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

// scrubDir replaces every spelling of the directory dir in out with token: as
// given, made absolute, and with its links resolved (macOS reports a temp
// directory as /private/var/..., Windows as a long name where the test holds
// a short one), each also as it appears in JSON, where a backslash is doubled.
// The longest spelling goes first, so a spelling that ends in another is not
// left half replaced.
func scrubDir(out, dir, token string) string {
	forms := map[string]bool{dir: true}
	if abs, err := filepath.Abs(dir); err == nil {
		forms[abs] = true
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			forms[real] = true
		}
	}
	var list []string
	for f := range forms {
		list = append(list, f, strings.ReplaceAll(f, `\`, `\\`))
	}
	sort.Slice(list, func(i, j int) bool {
		return len(list[i]) > len(list[j]) || len(list[i]) == len(list[j]) && list[i] < list[j]
	})
	for _, f := range list {
		out = strings.ReplaceAll(out, f, token)
	}
	return out
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
			got = scrubDir(got, tempRoot, "TEMPROOT")
			got = scrubDir(got, corpus, "CORPUS")
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

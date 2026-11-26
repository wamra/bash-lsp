package main

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// jsonRun is one invocation whose --json output is checked against the
// README table.
type jsonRun struct {
	args []string
	temp bool // run on the small temp workspace instead of the corpus
}

// tempFiles has what the corpus lacks: unresolved and dynamic edges, a
// source cycle, a function defined twice.
var tempFiles = map[string]string{
	"a.sh": "source ./missing.sh\nrun() { $1; }\nsource ./b.sh\nf\ncmd=g\n\"$cmd\"\neval \"g\"\n",
	"b.sh": "source ./a.sh\nf() { :; }\n",
	"c.sh": "f() { :; }\ng() { :; }\nf\n",
}

// jsonCommands is every command the README table documents, with the runs
// that show its records.
var jsonCommands = map[string][]jsonRun{
	"status":     {{args: []string{"status"}}},
	"symbols":    {{args: []string{"symbols", corpus}}, {args: []string{"symbols"}, temp: true}},
	"callers":    {{args: []string{"callers", "lib/util.sh:4:1"}}, {args: []string{"callers", "lib/util.sh"}}},
	"callees":    {{args: []string{"callees", "main.sh"}}, {args: []string{"callees", "a.sh"}, temp: true}},
	"refs":       {{args: []string{"refs", "lib/util.sh:4:1"}}, {args: []string{"refs", "g"}, temp: true}},
	"impact":     {{args: []string{"impact", "lib/util.sh:4:1"}}, {args: []string{"impact", "a.sh"}, temp: true}},
	"trace":      {{args: []string{"trace", "main.sh", "lib/util.sh:3:1"}}, {args: []string{"trace", "orphan.sh", "main.sh"}}},
	"unresolved": {{args: []string{"unresolved"}}, {args: []string{"unresolved"}, temp: true}},
	"cycles":     {{args: []string{"cycles"}}, {args: []string{"cycles"}, temp: true}},
}

// readJSONDoc parses the table between the markers: command ->
// (wrapper keys, field -> optional).
func readJSONDoc(t *testing.T) map[string]docRow {
	t.Helper()
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	const begin, end = "<!-- json-records:begin -->", "<!-- json-records:end -->"
	i, j := strings.Index(text, begin), strings.Index(text, end)
	if i < 0 || j < i {
		t.Fatalf("README.md has no table between %s and %s", begin, end)
	}
	tick := regexp.MustCompile("`([a-z_]+)`(\\?)?")
	rows := map[string]docRow{}
	for _, line := range strings.Split(text[i+len(begin):j], "\n") {
		cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
		if len(cells) != 3 || !strings.HasPrefix(strings.TrimSpace(cells[0]), "`") {
			continue
		}
		name := strings.Trim(strings.TrimSpace(cells[0]), "`")
		row := docRow{fields: map[string]bool{}}
		if !strings.Contains(cells[1], "flat") {
			for _, m := range tick.FindAllStringSubmatch(cells[1], -1) {
				row.wrapper = append(row.wrapper, m[1])
			}
			sort.Strings(row.wrapper)
		}
		for _, m := range tick.FindAllStringSubmatch(cells[2], -1) {
			row.fields[m[1]] = m[2] == "?"
		}
		rows[name] = row
	}
	return rows
}

type docRow struct {
	wrapper []string        // nil: the record is the whole object (flat)
	fields  map[string]bool // field -> may be absent
}

// runJSON decodes the --json output of a run.
func runJSON(t *testing.T, r jsonRun, tempRoot string) map[string]any {
	t.Helper()
	args := append([]string(nil), r.args...)
	if args[0] != "symbols" {
		root := corpus
		if r.temp {
			root = tempRoot
		}
		args = append(args, "--root", root)
	} else if r.temp {
		args = append(args, tempRoot)
	}
	args = append(args, "--json")
	var top map[string]any
	if err := json.Unmarshal([]byte(runOK(t, args...)), &top); err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	return top
}

func TestJSONDocMatchesOutput(t *testing.T) {
	doc := readJSONDoc(t)
	tempRoot := writeRoot(t, tempFiles)
	for _, cmd := range []string{"status", "symbols", "callers", "callees", "refs", "impact", "trace", "unresolved", "cycles"} {
		row, ok := doc[cmd]
		if !ok {
			t.Errorf("README.md: no row for %s", cmd)
			continue
		}
		seen := map[string]int{} // field -> records that carry it
		records := 0
		for _, r := range jsonCommands[cmd] {
			top := runJSON(t, r, tempRoot)
			var recs []map[string]any
			if row.wrapper == nil {
				recs = []map[string]any{top}
			} else {
				keys := make([]string, 0, len(top))
				for k := range top {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				if !slices.Equal(keys, row.wrapper) {
					t.Errorf("%s %v: wrapper keys %v, README says %v", cmd, r.args, keys, row.wrapper)
				}
				list, _ := top["results"].([]any)
				if cmd == "symbols" {
					list, _ = top["symbols"].([]any)
				}
				for _, x := range list {
					recs = append(recs, x.(map[string]any))
				}
			}
			for _, rec := range recs {
				records++
				for k := range rec {
					seen[k]++
					if _, documented := row.fields[k]; !documented {
						t.Errorf("%s %v: record carries %q, which README.md does not list", cmd, r.args, k)
					}
				}
			}
		}
		if records == 0 {
			t.Errorf("%s: no record to check", cmd)
		}
		for f, optional := range row.fields {
			switch {
			case !optional && seen[f] != records:
				t.Errorf("%s: field %q is documented as always present but is in %d of %d records", cmd, f, seen[f], records)
			case optional && seen[f] == 0:
				t.Errorf("%s: optional field %q is never present in the runs", cmd, f)
			}
		}
	}
}

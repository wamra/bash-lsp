package analysis

import (
	"slices"
	"sort"
	"testing"
)

func TestCommandWordAt(t *testing.T) {
	src := "#!/bin/bash\nFOO=1 printf '%s' \"$(date)\" | tr a b\nf() { :; }\nf\nsource ./x.sh\necho \U0001F600 hi\n"
	q := buildMem(t, map[string]string{"x.sh": src})
	for _, tc := range []struct {
		needle string
		occ    int
		off    int
		want   string // "" = not a command word
	}{
		{"printf", 0, 0, "printf"},
		{"printf", 0, 5, "printf"}, // a cursor just after the name
		{"date", 0, 1, "date"},     // innermost call, inside a substitution
		{"tr a", 0, 1, "tr"},
		{"source", 0, 2, "source"}, // a builtin the binder records no use for
		{"echo", 0, 0, "echo"},
		{"'%s'", 0, 1, ""},   // an argument
		{"FOO=1", 0, 1, ""},  // an assignment
		{"hi", 0, 0, ""},     // after the emoji
		{"#!/bin", 0, 2, ""}, // the shebang comment
	} {
		p, o := q.at("x.sh", tc.needle, tc.occ, tc.off)
		name, rng, ok := q.s.CommandWordAt(p, o)
		if tc.want == "" {
			if ok {
				t.Errorf("%q+%d: got command word %q", tc.needle, tc.off, name)
			}
			continue
		}
		if !ok || name != tc.want || rng.End.Offset-rng.Start.Offset != len(tc.want) {
			t.Errorf("%q+%d: got %q %v %+v, want %q", tc.needle, tc.off, name, ok, rng, tc.want)
		}
	}
	if _, _, ok := q.s.CommandWordAt("/no/such/file.sh", 0); ok {
		t.Error("an unknown path answered")
	}
}

func TestBuiltinNamesSorted(t *testing.T) {
	names := BuiltinNames()
	if !sort.StringsAreSorted(names) || len(names) < 80 {
		t.Fatalf("%d names, sorted %v", len(names), sort.StringsAreSorted(names))
	}
	for _, n := range []string{"read", "printf", "[[", "coproc"} {
		if !slices.Contains(names, n) || !IsBuiltin(n) {
			t.Errorf("%q missing", n)
		}
	}
	for _, n := range names {
		if !IsBuiltin(n) {
			t.Errorf("%q listed but not a builtin", n)
		}
	}
}

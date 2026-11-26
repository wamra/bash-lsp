package analysis

import "testing"

// locals were keyed by {file, function name}, so a function defined
// twice in one file (once per branch of an if) shared one set of locals.
const twoBodies = `if x; then
f() { local a=1; echo "$a"; }
else
f() { a=2; echo "$a"; }
fi
a=top
`

func TestLocalsBelongToTheirBody(t *testing.T) {
	q := buildMem(t, map[string]string{"a.sh": twoBodies})
	// The first body's local stays on its own.
	wantEq(t, q.refs("a.sh", "a=1", 0, 0), "a.sh:2", "a.sh:2")
	wantEq(t, q.refs("a.sh", `"$a"`, 0, 2), "a.sh:2", "a.sh:2")
	// The second body writes the global: it groups with the top-level a=top.
	wantEq(t, q.refs("a.sh", "a=2", 0, 0), "a.sh:4", "a.sh:4", "a.sh:6")
	wantEq(t, q.refs("a.sh", "a=top", 0, 0), "a.sh:4", "a.sh:4", "a.sh:6")
}

// The second same-file definition of a function kept no locals in
// the outline. Each definition is its own entry, holding its own locals.
func TestEachDefinitionKeepsItsLocals(t *testing.T) {
	src := "f() { local one=1; }\nf() { local two=2; }\n"
	q := buildMem(t, map[string]string{"a.sh": src})
	p, _ := q.at("a.sh", "f", 0, 0)
	ds := q.s.DocumentSymbols(p)
	if len(ds) != 2 || ds[0].Name != "f" || ds[1].Name != "f" {
		t.Fatalf("%+v", ds)
	}
	if len(ds[0].Children) != 1 || ds[0].Children[0].Name != "one" || len(ds[1].Children) != 1 || ds[1].Children[0].Name != "two" {
		t.Fatalf("children %+v / %+v", ds[0].Children, ds[1].Children)
	}
	for _, d := range ds {
		c := d.Children[0]
		if c.Range.Start.Offset < d.Range.Start.Offset || c.Range.End.Offset > d.Range.End.Offset {
			t.Fatalf("local %s lies outside its function %+v", c.Name, d.Range)
		}
	}
}

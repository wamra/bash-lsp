package analysis

import (
	"sort"
	"strings"
	"testing"
)

// reasonAt returns the reason of the single binding of the use at needle.
func (q *qworld) reasonAt(file, needle string, occ, off int) string {
	q.t.Helper()
	p, o := q.at(file, needle, occ, off)
	bs := q.s.hitBindings(p, o)
	if len(bs) != 1 {
		q.t.Fatalf("%s %q: %d bindings", file, needle, len(bs))
	}
	return bs[0].Reason
}

func TestSubshellAssignmentDoesNotEscape(t *testing.T) {
	q := buildMem(t, map[string]string{"m.sh": "( y=2 )\necho \"$y\"\n"})
	wantEq(t, q.def("m.sh", "$y", 0, 1))
	wantEq(t, q.refs("m.sh", "$y", 0, 1), "m.sh:2")
	if r := q.reasonAt("m.sh", "$y", 0, 1); r != ReasonEnvironment {
		t.Fatalf("reason %q", r)
	}
	// The assignment keeps its own symbol.
	wantEq(t, q.refs("m.sh", "y=2", 0, 0), "m.sh:1")
}

func TestInnerReadBindsInnerOuterReadBindsOuter(t *testing.T) {
	q := buildMem(t, map[string]string{"m.sh": "y=1\n( y=2\necho \"$y\" )\necho \"$y\"\n"})
	wantEq(t, q.def("m.sh", "$y", 0, 1), "m.sh:2")
	wantEq(t, q.def("m.sh", "$y", 1, 1), "m.sh:1")
	wantEq(t, q.refs("m.sh", "y=2", 0, 0), "m.sh:2", "m.sh:3")
	wantEq(t, q.refs("m.sh", "y=1", 0, 0), "m.sh:1", "m.sh:4")
}

func TestInnerReadSeesOuterDeclaration(t *testing.T) {
	q := buildMem(t, map[string]string{"m.sh": "y=1\n( echo \"$y\" )\nv=$(echo \"$y\")\necho a | echo \"$y\"\n"})
	for occ := 0; occ < 3; occ++ {
		wantEq(t, q.def("m.sh", "$y", occ, 1), "m.sh:1")
	}
}

func TestPipelineStageAssignmentDoesNotEscape(t *testing.T) {
	q := buildMem(t, map[string]string{"m.sh": "echo hi | read z\necho \"$z\"\n"})
	wantEq(t, q.def("m.sh", "$z", 0, 1))
	if r := q.reasonAt("m.sh", "$z", 0, 1); r != ReasonEnvironment {
		t.Fatalf("reason %q", r)
	}
}

func TestLastPipeStageIsAScopeToo(t *testing.T) {
	q := buildMem(t, map[string]string{"m.sh": "cmd | while read x; do\n echo \"$x\"\ndone\necho \"$x\"\n"})
	wantEq(t, q.def("m.sh", "$x", 0, 1), "m.sh:1")
	wantEq(t, q.def("m.sh", "$x", 1, 1))
}

func TestSingleCommandIsNoPipelineScope(t *testing.T) {
	q := buildMem(t, map[string]string{"m.sh": "read z\necho \"$z\"\nif true; then w=1; fi\n{ u=1; }\necho \"$w$u\"\n"})
	wantEq(t, q.def("m.sh", "$z", 0, 1), "m.sh:1")
	wantEq(t, q.def("m.sh", "$w", 0, 1), "m.sh:3")
	wantEq(t, q.def("m.sh", "$u", 0, 1), "m.sh:4")
}

func TestCommandSubstitutionScope(t *testing.T) {
	q := buildMem(t, map[string]string{"m.sh": "v=$(w=3; echo \"$w\")\necho \"$w\"\n"})
	wantEq(t, q.def("m.sh", "$w", 0, 1), "m.sh:1")
	wantEq(t, q.def("m.sh", "$w", 1, 1))
	wantEq(t, q.refs("m.sh", "w=3", 0, 0), "m.sh:1", "m.sh:1")
	// The assignment of v itself is in the outer scope.
	wantEq(t, q.refs("m.sh", "v=", 0, 0), "m.sh:1")
}

func TestLocalInSubshellScopedToSubshell(t *testing.T) {
	q := buildMem(t, map[string]string{"m.sh": "f() { local a; ( a=5 ); echo \"$a\"; }\n"})
	wantEq(t, q.def("m.sh", "\"$a\"", 0, 2), "m.sh:1")
	p, o := q.at("m.sh", "\"$a\"", 0, 2)
	d := q.s.Definition(p, o)
	if len(d) != 1 || d[0].Range.Start.Offset != strings.Index("f() { local a; ( a=5 ); echo \"$a\"; }\n", "local a")+len("local ") {
		t.Fatalf("definition %+v", d)
	}
	// a=5 is not the local.
	wantEq(t, q.refs("m.sh", "a=5", 0, 0), "m.sh:1")
	// local inside a subshell inside a function does not make the function's a local.
	q = buildMem(t, map[string]string{"m.sh": "f() { ( local b=1 ); b=2; }\necho \"$b\"\n"})
	wantEq(t, q.def("m.sh", "$b", 0, 1), "m.sh:1")
	wantEq(t, q.refs("m.sh", "b=1", 0, 0), "m.sh:1")
}

func TestExportInSubshellScopedToSubshell(t *testing.T) {
	q := buildMem(t, map[string]string{"m.sh": "( export e=1 )\necho \"$e\"\n"})
	wantEq(t, q.def("m.sh", "$e", 0, 1))
}

func TestProcessSubstitutionAndCoprocFollowS1(t *testing.T) {
	q := buildMem(t, map[string]string{"m.sh": "cat <(p=1; echo \"$p\")\necho \"$p\"\ncoproc C { c=1; echo \"$c\"; }\necho \"$c\"\n"})
	wantEq(t, q.def("m.sh", "$p", 0, 1), "m.sh:1")
	wantEq(t, q.def("m.sh", "$p", 1, 1))
	wantEq(t, q.def("m.sh", "$c", 0, 1), "m.sh:3")
	wantEq(t, q.def("m.sh", "$c", 1, 1))
}

func TestNestedSubshellsBindInnermost(t *testing.T) {
	q := buildMem(t, map[string]string{"m.sh": "( y=1\n( y=2; echo \"$y\" )\necho \"$y\" )\n"})
	wantEq(t, q.def("m.sh", "$y", 0, 1), "m.sh:2")
	wantEq(t, q.def("m.sh", "$y", 1, 1), "m.sh:1")
}

func TestFunctionInSubshellStaysVisible(t *testing.T) {
	q := buildMem(t, map[string]string{"m.sh": "( g() { :; } )\ng\n"})
	wantEq(t, q.def("m.sh", "g\n", 0, 0), "m.sh:1")
}

func TestFunctionBodyInSubshellWritesGlobal(t *testing.T) {
	// a function defined inside a subshell runs wherever it is called: its writes are global
	q := buildMem(t, map[string]string{"m.sh": "( f() { g=1; } )\necho \"$g\"\n"})
	wantEq(t, q.def("m.sh", "$g", 0, 1), "m.sh:1")
}

func TestSubshellSymbolsStayOutOfOutline(t *testing.T) {
	q := buildMem(t, map[string]string{"m.sh": "( top=1 )\nkeep=2\nf() { ( inner=1 ); }\n"})
	p, _ := q.at("m.sh", "keep", 0, 0)
	var names []string
	for _, d := range q.s.DocumentSymbols(p) {
		names = append(names, d.Name)
		for _, c := range d.Children {
			names = append(names, d.Name+"/"+c.Name)
		}
	}
	sort.Strings(names)
	wantEq(t, names, "keep", "f", "f/inner")
	for _, s := range q.s.WorkspaceSymbols("top", 0) {
		t.Fatalf("workspace symbols list a subshell variable: %+v", s)
	}
	if got := q.s.WorkspaceSymbols("inner", 0); len(got) != 0 {
		t.Fatalf("workspace symbols list %+v", got)
	}
	if got := q.s.WorkspaceSymbols("keep", 0); len(got) != 1 {
		t.Fatalf("workspace symbols %+v", got)
	}
}

func TestSubshellDeclarationInOtherFileStaysApart(t *testing.T) {
	q := buildMem(t, map[string]string{
		"a.sh": "source ./b.sh\necho \"$s\"\n",
		"b.sh": "( s=1 )\n",
	})
	wantEq(t, q.def("a.sh", "$s", 0, 1))
}

func TestEnvironmentReadHover(t *testing.T) {
	q := buildMem(t, map[string]string{"m.sh": "echo \"$HOME\"\n"})
	if r := q.reasonAt("m.sh", "$HOME", 0, 1); r != ReasonEnvironment {
		t.Fatalf("reason %q", r)
	}
	md, ok := q.hover("m.sh", "$HOME", 0, 2)
	if !ok || !strings.Contains(md, "Not assigned in this workspace; read from the environment.") {
		t.Fatalf("%q", md)
	}
}

func TestCallsKeepNotFoundAndReasonConstants(t *testing.T) {
	if ReasonDirect != "direct" || ReasonAlias != "alias" || ReasonOutsideClosure != "name-match-outside-closure" ||
		ReasonNotFound != "not-found" || ReasonEnvironment != "environment" {
		t.Fatal("reason constants changed")
	}
}

func TestHoverOnSubshellVariable(t *testing.T) {
	q := buildMem(t, map[string]string{"m.sh": "( s=1 )\nf() { echo x | read r; }\n"})
	md, ok := q.hover("m.sh", "s=1", 0, 0)
	if !ok || !strings.Contains(md, "Visible only inside its subshell in `m.sh`") || strings.Contains(md, "Global") {
		t.Fatalf("%q", md)
	}
	md, _ = q.hover("m.sh", " r;", 0, 1)
	if !strings.Contains(md, "Visible only inside its pipeline stage") {
		t.Fatalf("%q", md)
	}
}

func TestFunctionInSubshellReadsSubshellVariable(t *testing.T) {
	q := buildMem(t, map[string]string{"m.sh": "( x=1; f() { echo \"$x\"; }; f )\n"})
	wantEq(t, q.def("m.sh", "$x", 0, 1), "m.sh:1")
	// the function's own locals are still invisible to a function defined inside it
	q = buildMem(t, map[string]string{"m.sh": "f() { local x=1; g() { echo \"$x\"; }; }\n"})
	if r := q.reasonAt("m.sh", "$x", 0, 1); r != ReasonEnvironment {
		t.Fatalf("a nested function must not see the enclosing function's local: %q", r)
	}
	// a subshell inside the function that sits in a subshell still resolves innermost first
	q = buildMem(t, map[string]string{"m.sh": "( x=1; f() { ( x=2; echo \"$x\" ); echo \"$x\"; } )\n"})
	wantEq(t, q.def("m.sh", "$x", 0, 1), "m.sh:1")
	wantEq(t, q.def("m.sh", "$x", 1, 1), "m.sh:1")
}

func TestCommandSubstitutionAssignmentPins(t *testing.T) {
	q := buildMem(t, map[string]string{"m.sh": "x=1\nx=$(\n  x=2\n  echo \"$x\"\n)\necho \"$x\"\n"})
	// the inner read binds the inner x=2 only
	wantEq(t, q.def("m.sh", "$x", 0, 1), "m.sh:3")
	// the outer read binds x=1 and the outer x=$(...) assignment, not the inner one
	wantEq(t, q.def("m.sh", "$x", 1, 1), "m.sh:1", "m.sh:2")
}

func TestPipelineBraceGroupReadPin(t *testing.T) {
	q := buildMem(t, map[string]string{"m.sh": "cmd | { read a; echo \"$a\"; }\necho \"$a\"\n"})
	wantEq(t, q.def("m.sh", "$a", 0, 1), "m.sh:1")
	wantEq(t, q.def("m.sh", "$a", 1, 1))
}

func TestHeredocBodySubstitutionIsNotADeclaration(t *testing.T) {
	q := buildMem(t, map[string]string{"m.sh": "cat <<EOF\n$(y=1)\nEOF\necho \"$y\"\n"})
	wantEq(t, q.def("m.sh", "$y", 0, 1))
}

// A delimiter quoted in only one part (<<"E"F) makes the body literal, so a
// substitution in it declares nothing and is no use of a variable.
func TestHeredocPartlyQuotedDelimiterBodyIsLiteral(t *testing.T) {
	q := buildMem(t, map[string]string{"m.sh": "cat <<\"E\"F\n$(y=1)\nEF\necho \"$y\"\n"})
	wantEq(t, q.def("m.sh", "$y", 0, 1))
	q = buildMem(t, map[string]string{"m.sh": "y=1\ncat <<\"E\"F\n$y\nEF\n"})
	wantEq(t, q.def("m.sh", "$y", 0, 1))
}

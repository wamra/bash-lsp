package analysis

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/internal/front"
	"github.com/wamra/bash-lsp/tree"
)

func (q *qworld) editStrs(p *RenamePlan) []string {
	out := []string{}
	for _, e := range p.Edits {
		out = append(out, fmt.Sprintf("%s:%d", q.rel(e.Path), e.Range.Start.Line))
	}
	return out
}

func (q *qworld) exclStrs(p *RenamePlan) []string {
	out := []string{}
	for _, x := range p.Excluded {
		out = append(out, fmt.Sprintf("%s:%d %s/%s", q.rel(x.Loc.Path), x.Loc.Range.Start.Line, x.Res, x.Reason))
	}
	return out
}

// refusal requires err to be a *RenameRefusal with the given code.
func refusal(t *testing.T, err error, code string) *RenameRefusal {
	t.Helper()
	var r *RenameRefusal
	if !errors.As(err, &r) {
		t.Fatalf("err = %v, want a *RenameRefusal %q", err, code)
	}
	if r.Code != code {
		t.Fatalf("refusal %q (%s), want %q", r.Code, r.Reason, code)
	}
	return r
}

func (q *qworld) plan(file, needle string, occ, off int, newName string) (*RenamePlan, error) {
	q.t.Helper()
	p, o := q.at(file, needle, occ, off)
	return q.s.PlanRename(p, o, newName)
}

func (q *qworld) refuses(code, file, needle string, occ, off int, newName string) *RenameRefusal {
	q.t.Helper()
	plan, err := q.plan(file, needle, occ, off, newName)
	r := refusal(q.t, err, code)
	if plan != nil {
		q.t.Fatalf("refusal %q comes with a plan", code)
	}
	return r
}

func TestAC19RenamePlan(t *testing.T) {
	q := buildCorpus(t)
	p, err := q.s.PlanRenameByName("build_release", "ship")
	if err != nil {
		t.Fatal(err)
	}
	wantSeq(t, q.editStrs(p), "dyn.sh:4", "lib/util.sh:4", "main.sh:3", "main.sh:6")
	wantSeq(t, q.exclStrs(p),
		"main.sh:8 candidate/variable-constant",
		"main.sh:9 candidate/variable-constant",
		"main.sh:10 candidate/string-eval")
	wantSeq(t, q.strs(p.Others), "orphan.sh:2")
	if p.Old != "build_release" || p.New != "ship" || p.Symbol == nil || p.Symbol.Name != "build_release" {
		t.Fatalf("plan header %+v", p)
	}
	for _, e := range p.Edits {
		if e.NewText != "ship" {
			t.Errorf("edit %v: NewText %q", e, e.NewText)
		}
		src := q.s.Facts(e.Path).File.Src
		if got := string(src[e.Range.Start.Offset:e.Range.End.Offset]); got != "build_release" {
			t.Errorf("edit at %s:%d covers %q", q.rel(e.Path), e.Range.Start.Line, got)
		}
	}
	for _, x := range p.Excluded {
		if x.Text == "" {
			t.Errorf("exclusion %s:%d has no text", q.rel(x.Loc.Path), x.Loc.Range.Start.Line)
		}
	}
	// main.sh:7 (br "$name") is in neither list.
	for _, l := range append(q.editStrs(p), q.exclStrs(p)...) {
		if strings.HasPrefix(l, "main.sh:7") {
			t.Errorf("%s is in the plan", l)
		}
	}
	if len(p.Warnings) != 0 {
		t.Errorf("warnings %v", p.Warnings)
	}
}

func TestRenameByPositionEqualsByName(t *testing.T) {
	q := buildCorpus(t)
	byName, err := q.s.PlanRenameByName("build_release", "ship")
	if err != nil {
		t.Fatal(err)
	}
	byPos, err := q.plan("main.sh", `  build_release "$name"`, 0, 4, "ship")
	if err != nil {
		t.Fatal(err)
	}
	wantSeq(t, q.editStrs(byPos), q.editStrs(byName)...)
	wantSeq(t, q.exclStrs(byPos), q.exclStrs(byName)...)
}

func TestRenameAliasAtCallWord(t *testing.T) {
	q := buildCorpus(t)
	plan, err := q.plan("main.sh", `  br "$name"`, 0, 2, "fast")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Old != "br" || plan.Symbol.Kind != DeclAlias {
		t.Fatalf("renames %s (kind %d), want the alias br", plan.Old, plan.Symbol.Kind)
	}
	wantSeq(t, q.editStrs(plan), "main.sh:3", "main.sh:7")
	for _, e := range plan.Edits {
		src := q.s.Facts(e.Path).File.Src
		if got := string(src[e.Range.Start.Offset:e.Range.End.Offset]); got != "br" {
			t.Errorf("edit at line %d covers %q", e.Range.Start.Line, got)
		}
	}
	if len(plan.Excluded) != 0 {
		t.Errorf("exclusions %v", q.exclStrs(plan))
	}
	p, off := q.at("main.sh", `  br "$name"`, 0, 2)
	rng, placeholder, err := q.s.PrepareRename(p, off)
	if err != nil {
		t.Fatal(err)
	}
	src := q.src("main.sh")
	if got := string(src[rng.Start.Offset:rng.End.Offset]); got != "br" || placeholder != "br" || rng.Start.Line != 7 {
		t.Fatalf("PrepareRename = %q (line %d), placeholder %q", got, rng.Start.Line, placeholder)
	}
	// On the alias name itself: the same plan.
	again, err := q.plan("main.sh", "alias br=", 0, 6, "fast")
	if err != nil {
		t.Fatal(err)
	}
	wantSeq(t, q.editStrs(again), "main.sh:3", "main.sh:7")
}

func TestRenameConditionalDefinitionExcluded(t *testing.T) {
	q := buildMem(t, map[string]string{"a.sh": "if true; then\n  f() { :; }\nelse\n  f() { :; }\nfi\nf\n"})
	plan, err := q.plan("a.sh", "f()", 0, 0, "g")
	if err != nil {
		t.Fatal(err)
	}
	wantSeq(t, q.editStrs(plan), "a.sh:2", "a.sh:4")
	wantSeq(t, q.exclStrs(plan), "a.sh:6 candidate/conditional-definition")
}

func TestRenameInexactNameExcluded(t *testing.T) {
	q := buildMem(t, map[string]string{"a.sh": "f() { :; }\neval \\f\nf\n"})
	plan, err := q.plan("a.sh", "f()", 0, 0, "g")
	if err != nil {
		t.Fatal(err)
	}
	wantSeq(t, q.editStrs(plan), "a.sh:1", "a.sh:3")
	if len(plan.Excluded) != 1 || plan.Excluded[0].Loc.Range.Start.Line != 2 {
		t.Fatalf("exclusions %v, want the eval site", q.exclStrs(plan))
	}
	for _, e := range plan.Edits {
		if e.Range.Start.Line == 2 {
			t.Fatalf("edit at the inexact site: %v", e)
		}
	}
}

func TestRenameLocalStaysInFunction(t *testing.T) {
	q := buildCorpus(t)
	plan, err := q.plan("main.sh", `local name="$1"`, 0, 6, "label")
	if err != nil {
		t.Fatal(err)
	}
	wantSeq(t, q.editStrs(plan), "main.sh:5", "main.sh:6", "main.sh:7")
	for _, e := range plan.Edits {
		if q.rel(e.Path) != "main.sh" {
			t.Errorf("edit outside main.sh: %s", q.rel(e.Path))
		}
	}
}

func TestRenameNotASymbol(t *testing.T) {
	q := buildMem(t, map[string]string{"a.sh": "echo hi\nls -l\necho \"$HOME\"\nf() { echo \"$1\"; }\nif true; then :; fi\n"})
	for _, c := range []struct {
		name, needle string
		off          int
	}{
		{"builtin", "echo hi", 1},
		{"external command", "ls -l", 0},
		{"environment read", "$HOME", 2},
		{"positional parameter", "$1", 1},
		{"keyword", "if true", 0},
		{"nothing there", "-l", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			q.t = t
			q.refuses(RefuseNotSymbol, "a.sh", c.needle, 0, c.off, "zzz")
			p, off := q.at("a.sh", c.needle, 0, c.off)
			if _, _, err := q.s.PrepareRename(p, off); err == nil {
				t.Fatal("PrepareRename succeeds")
			} else {
				refusal(t, err, RefuseNotSymbol)
			}
		})
	}
	if _, err := q.s.PlanRenameByName("nosuch", "x"); err == nil {
		t.Fatal("unknown function renames")
	} else {
		refusal(t, err, RefuseNotSymbol)
	}
}

func TestRenameCandidateSite(t *testing.T) {
	q := buildCorpus(t)
	for _, c := range []struct {
		name, needle string
		off          int
	}{
		{"string eval", `eval "build_release y"`, 7},
		{"variable literal", "cmd=build_release", 6},
	} {
		t.Run(c.name, func(t *testing.T) {
			q.t = t
			r := q.refuses(RefuseCandidateSite, "main.sh", c.needle, 0, c.off, "ship")
			if len(r.Sites) != 1 {
				t.Errorf("sites %v", r.Sites)
			}
			p, off := q.at("main.sh", c.needle, 0, c.off)
			_, _, err := q.s.PrepareRename(p, off)
			refusal(t, err, RefuseCandidateSite)
		})
	}
}

func TestRenameAmbiguousSymbol(t *testing.T) {
	// Two files that cannot see each other declare f; c.sh sees neither, so its call
	// matches both by name.
	q := buildMem(t, map[string]string{"a.sh": "f() { :; }\n", "b.sh": "f() { :; }\n", "c.sh": "f\n"})
	r := q.refuses(RefuseAmbiguous, "c.sh", "f\n", 0, 0, "h")
	wantSeq(t, q.strs(r.Sites), "a.sh:1", "b.sh:1")
	p, off := q.at("c.sh", "f\n", 0, 0)
	_, _, err := q.s.PrepareRename(p, off)
	refusal(t, err, RefuseAmbiguous)
	// By name, equally visible functions are a tie.
	_, err = q.s.PlanRenameByName("f", "h")
	r = refusal(t, err, RefuseAmbiguous)
	wantSeq(t, q.strs(r.Sites), "a.sh:1", "b.sh:1")
}

func TestRenameInvalidName(t *testing.T) {
	q := buildCorpus(t)
	for _, n := range []string{"", "1a", "a-b", "a b", "a.b", "a$b", "é", "a;b"} {
		r := q.refuses(RefuseInvalidName, "lib/util.sh", "build_release()", 0, 2, n)
		if r.Reason == "" {
			t.Errorf("%q: no reason", n)
		}
	}
	if _, err := q.plan("lib/util.sh", "build_release()", 0, 2, "_ok9"); err != nil {
		t.Fatalf("a valid name refused: %v", err)
	}
}

func TestRenameReservedName(t *testing.T) {
	q := buildCorpus(t)
	for _, n := range []string{"echo", "cd", "eval"} {
		q.refuses(RefuseReserved, "lib/util.sh", "build_release()", 0, 2, n)
	}
}

func TestRenameNameConflict(t *testing.T) {
	q := buildCorpus(t)
	r := q.refuses(RefuseConflict, "lib/util.sh", "log()", 0, 1, "build_release")
	if len(r.Sites) == 0 {
		t.Fatal("no conflicting sites listed")
	}
	// A name that is an alias of the closure conflicts with a function rename too.
	q2 := buildMem(t, map[string]string{"a.sh": "alias g=:\nf() { :; }\nf\n"})
	q2.refuses(RefuseConflict, "a.sh", "f()", 0, 0, "g")
	// A local that is renamed onto another local of the same function.
	q3 := buildMem(t, map[string]string{"a.sh": "f() {\n  local a=1 b=2\n  echo \"$a\"\n}\n"})
	q3.refuses(RefuseConflict, "a.sh", "a=1", 0, 0, "b")
	// A local renamed onto a name the function reads from elsewhere would capture it.
	q4 := buildMem(t, map[string]string{"a.sh": "f() {\n  local a=1\n  echo \"$a $other\"\n}\n"})
	q4.refuses(RefuseConflict, "a.sh", "a=1", 0, 0, "other")
	// A global renamed onto a local of a function that reads it.
	q5 := buildMem(t, map[string]string{"a.sh": "g=1\nf() {\n  local h=2\n  echo \"$g $h\"\n}\n"})
	q5.refuses(RefuseConflict, "a.sh", "g=1", 0, 0, "h")
	// Another function's local of that name is no conflict.
	q6 := buildMem(t, map[string]string{"a.sh": "f() {\n  local a=1\n  echo \"$a\"\n}\ng() {\n  local b=1\n  echo \"$b\"\n}\n"})
	if _, err := q6.plan("a.sh", "a=1", 0, 0, "b"); err != nil {
		t.Fatalf("sibling local conflicts: %v", err)
	}
}

func TestRenameRecoveredRegion(t *testing.T) {
	q := buildMem(t, map[string]string{"a.sh": "f() { :; }\nif true; then\n  f\n"})
	var ifNode *tree.Node
	q.s.Facts(q.abs("a.sh")).File.Root.Walk(func(n *tree.Node) bool {
		if n.Kind == tree.KIf {
			ifNode = n
		}
		return true
	})
	if ifNode == nil || !ifNode.Recovered {
		t.Fatalf("the if is not Recovered: %+v", ifNode)
	}
	r := q.refuses(RefuseRecovered, "a.sh", "f() {", 0, 0, "g")
	if len(r.Sites) != 1 || r.Sites[0].Range.Start.Line != 3 {
		t.Fatalf("sites %v, want the call on line 3", r.Sites)
	}
}

func TestRenameSkippedFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "w")
	big, main := filepath.Join(root, "big.sh"), filepath.Join(root, "m.sh")
	src := "source ./big.sh\nf() { :; }\nf\n"
	files := map[string]*FileFacts{main: Bind(front.Parse(main, []byte(src), front.Detect(main, []byte(src))))}
	s := Build(files, Options{Root: root, Oversize: func(p string) bool { return p == big }}, 1)
	off := strings.Index(src, "f()")
	plan, err := s.PlanRename(main, off, "g")
	r := refusal(t, err, RefuseSkippedFile)
	if plan != nil {
		t.Fatal("a plan with a refusal")
	}
	if len(r.Sites) != 1 || r.Sites[0].Range.Start.Line != 1 {
		t.Fatalf("sites %v, want the source command", r.Sites)
	}
}

// A function that a file in view removes with `unset -f` is not renamed: the
// unset names it where no use is found, so the rename is refused rather than
// left half done.
func TestRenameUnsetInViewIsRefused(t *testing.T) {
	q := buildMem(t, map[string]string{
		"lib.sh":  "f(){ :; }; unset -f f\n",
		"main.sh": ". ./lib.sh\nf\n",
	})
	_, err := q.plan("lib.sh", "f()", 0, 0, "g")
	r := refusal(t, err, RefuseNameReference)
	wantSeq(t, q.strs(r.Sites), "lib.sh:1")
	// The same unset in the sourcing file is in view too.
	q = buildMem(t, map[string]string{
		"lib.sh":  "f(){ :; }\n",
		"main.sh": ". ./lib.sh\nunset -f f\nf\n",
	})
	_, err = q.plan("lib.sh", "f()", 0, 0, "g")
	r = refusal(t, err, RefuseNameReference)
	wantSeq(t, q.strs(r.Sites), "main.sh:2")
}

func TestRenameUTF16(t *testing.T) {
	src := "echo \"😀\"; x=1; echo \"$x\"\n"
	q := buildMem(t, map[string]string{"a.sh": src})
	plan, err := q.plan("a.sh", "x=1", 0, 0, "y")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Edits) != 2 {
		t.Fatalf("edits %v", q.editStrs(plan))
	}
	f := q.s.Facts(q.abs("a.sh")).File
	for _, e := range plan.Edits {
		if got := string(f.Src[e.Range.Start.Offset:e.Range.End.Offset]); got != "x" {
			t.Errorf("edit covers %q", got)
		}
	}
	// Positions come in as UTF-16: the emoji is two units wide.
	path, off, ok, err := q.s.Position(q.abs("a.sh") + ":1:12")
	if err != nil || !ok {
		t.Fatalf("Position: %v %v", ok, err)
	}
	if string(f.Src[off]) != "x" {
		t.Fatalf("Position lands on %q", f.Src[off])
	}
	plan2, err := q.s.PlanRename(path, off, "y")
	if err != nil || len(plan2.Edits) != 2 {
		t.Fatalf("plan at UTF-16 position: %v %v", plan2, err)
	}
}

func TestPositionShapes(t *testing.T) {
	q := buildCorpus(t)
	if _, _, ok, err := q.s.Position("main.sh"); ok || err != nil {
		t.Errorf("a bare path: ok=%v err=%v", ok, err)
	}
	if _, _, ok, err := q.s.Position("build_release"); ok || err != nil {
		t.Errorf("a name: ok=%v err=%v", ok, err)
	}
	if _, _, ok, err := q.s.Position("nosuch.sh:1:1"); ok || err != nil {
		t.Errorf("an unknown file: ok=%v err=%v", ok, err)
	}
	if _, _, ok, err := q.s.Position(q.abs("main.sh") + ":99:1"); !ok || err == nil {
		t.Errorf("a line past the end: ok=%v err=%v", ok, err)
	}
	p, off, ok, err := q.s.Position("main.sh:6:3")
	if err != nil || !ok || p != q.abs("main.sh") || !strings.HasPrefix(string(q.src("main.sh")[off:]), "build_release") {
		t.Errorf("main.sh:6:3 = %q %d %v %v", p, off, ok, err)
	}
}

func TestRenameEditsStayInClosure(t *testing.T) {
	q := buildCorpus(t)
	plan, err := q.s.PlanRenameByName("build_release", "ship")
	if err != nil {
		t.Fatal(err)
	}
	var declaring []string
	for _, d := range plan.Symbol.Decls {
		declaring = append(declaring, q.s.paths[q.s.declFile[d]])
	}
	seen := map[string]bool{}
	for _, d := range declaring {
		for _, v := range q.s.Visible(d) {
			seen[v] = true
		}
	}
	for _, e := range plan.Edits {
		if !seen[e.Path] {
			t.Errorf("edit in %s, outside the closure of %v", q.rel(e.Path), declaring)
		}
		if q.rel(e.Path) == "orphan.sh" {
			t.Errorf("edit in orphan.sh")
		}
	}
}

func TestRenameWarnings(t *testing.T) {
	q2 := buildMem(t, map[string]string{"a.sh": ". \"$X\"\nf() { :; }\nf\nexport V=1\n"})
	plan, err := q2.plan("a.sh", "f()", 0, 0, "g")
	if err != nil {
		t.Fatal(err)
	}
	wantSeq(t, plan.Warnings, "graph incomplete: a dynamic source may call f")
	plan, err = q2.plan("a.sh", "V=1", 0, 0, "W")
	if err != nil {
		t.Fatal(err)
	}
	wantSeq(t, plan.Warnings,
		"graph incomplete: a dynamic source may call V",
		"exported: processes started from these scripts may read V")
}

func TestRenameSitesCapped(t *testing.T) {
	gen := func(calls int) *qworld {
		var b strings.Builder
		b.WriteString("f() { :; }\n")
		for i := 0; i < calls; i++ {
			b.WriteString("f\n")
		}
		return buildMem(t, map[string]string{"a.sh": b.String()})
	}
	q := gen(maxRenameSites + 1)
	p, off := q.at("a.sh", "f()", 0, 0)
	sym, _ := q.s.SymbolAt(p, off)
	if got := len(q.s.BindingsOf(sym)); got != maxRenameSites+1 {
		t.Fatalf("generated %d call sites, want %d", got, maxRenameSites+1)
	}
	plan, err := q.s.PlanRename(p, off, "g")
	refusal(t, err, RefuseTooMany)
	if plan != nil {
		t.Fatal("a plan with a refusal")
	}
	// One fewer call is the largest plan: the declaration and the calls are maxRenameSites.
	q = gen(maxRenameSites - 1)
	p, off = q.at("a.sh", "f()", 0, 0)
	plan, err = q.s.PlanRename(p, off, "g")
	if err != nil {
		t.Fatalf("a plan of exactly %d sites: %v", maxRenameSites, err)
	}
	if len(plan.Edits) != maxRenameSites {
		t.Fatalf("%d edits, want %d", len(plan.Edits), maxRenameSites)
	}
	for i := 1; i < len(plan.Edits); i++ {
		if plan.Edits[i-1].Range.Start.Offset >= plan.Edits[i].Range.Start.Offset {
			t.Fatalf("edits %d and %d out of order", i-1, i)
		}
	}
}

func TestRenameOthersAreCapped(t *testing.T) {
	srcs := map[string]string{"a.sh": "f() { :; }\n"}
	for i := 0; i < maxRenameOthers+5; i++ {
		srcs[fmt.Sprintf("o%04d.sh", i)] = "f() { :; }\n"
	}
	q := buildMem(t, srcs)
	plan, err := q.plan("a.sh", "f()", 0, 0, "g")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Others) != maxRenameOthers {
		t.Fatalf("%d others, want %d", len(plan.Others), maxRenameOthers)
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "others") {
		t.Fatalf("warnings %v", plan.Warnings)
	}
}

func TestRenameElsewhereInBrokenFile(t *testing.T) {
	q := buildCorpus(t)
	plan, err := q.plan("broken_exp.sh", "before_exp()", 0, 0, "first")
	if err != nil {
		t.Fatalf("a site before the damage is refused: %v", err)
	}
	wantSeq(t, q.editStrs(plan), "broken_exp.sh:2")
}

func TestRenameGlobalVariable(t *testing.T) {
	q := buildMem(t, map[string]string{
		"a.sh": "G=1\n. ./b.sh\n",
		"b.sh": "echo \"$G\"\nG=2\n",
		"c.sh": "G=3\necho \"$G\"\n",
	})
	plan, err := q.plan("a.sh", "G=1", 0, 0, "H")
	if err != nil {
		t.Fatal(err)
	}
	wantSeq(t, q.editStrs(plan), "a.sh:1", "b.sh:1", "b.sh:2")
	wantSeq(t, q.strs(plan.Others), "c.sh:1")
	// An environment read of the new name in view would join the variable.
	q2 := buildMem(t, map[string]string{"a.sh": "G=1\necho \"$PATH_X\"\n"})
	q2.refuses(RefuseConflict, "a.sh", "G=1", 0, 0, "PATH_X")
}

// A command word written with an escape or quotes calls f, but its text is not
// exactly the name: it is never an edit, whatever the binding says.
func TestRenameInexactCommandWordsExcluded(t *testing.T) {
	q := buildMem(t, map[string]string{"a.sh": "f() { :; }\n\\f\n\"f\"\nf\n"})
	plan, err := q.plan("a.sh", "f()", 0, 0, "g")
	if err != nil {
		t.Fatal(err)
	}
	wantSeq(t, q.editStrs(plan), "a.sh:1", "a.sh:4")
	wantSeq(t, q.exclStrs(plan), "a.sh:2 resolved/inexact-name", "a.sh:3 resolved/inexact-name")
}

// The lexer reads a lone CR as a blank, the shell as a character: a rename
// refuses when a file it would edit has one outside a comment.
func TestRenameRefusesLoneCR(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"repro", "foo=1\rbar=2\necho $bar\n"},
		{"CR at the end", "bar=2\necho $bar\r"},
		{"CR CR LF", "bar=2\r\r\necho $bar\n"},
		{"CR in a string", "bar=2\necho \"a\rb $bar\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := buildMem(t, map[string]string{"a.sh": tc.src})
			r := q.refuses(RefuseLineBreaks, "a.sh", "bar", 1, 0, "zzz")
			if !strings.Contains(r.Reason, "a.sh") || !r.NameIndependent {
				t.Fatalf("refusal %+v", r)
			}
			p, o := q.at("a.sh", "bar", 1, 0)
			_, _, err := q.s.PrepareRename(p, o)
			refusal(t, err, RefuseLineBreaks)
		})
	}
}

func TestRenameLoneCRHarmlessCases(t *testing.T) {
	// CRLF only, and a lone CR inside a comment (a comment ends at LF in both readings).
	for name, src := range map[string]string{
		"CRLF":            "bar=2\r\necho $bar\r\n",
		"CR in a comment": "# a\rb\nbar=2\necho $bar\n",
	} {
		q := buildMem(t, map[string]string{"a.sh": src})
		if _, err := q.plan("a.sh", "bar", 1, 0, "zzz"); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A file with a lone CR that receives no edit blocks a rename only when it
// mentions the old or the new name: a use or a definition read around the CR
// could be hidden from the rename or invented by it.
func TestRenameLoneCRInAnUntouchedFile(t *testing.T) {
	q := buildMem(t, map[string]string{"a.sh": "bar=2\necho $bar\n", "b.sh": "x=1\ry=2\n"})
	if _, err := q.plan("a.sh", "bar", 1, 0, "zzz"); err != nil {
		t.Fatalf("unrelated file blocked the rename: %v", err)
	}
	q = buildMem(t, map[string]string{"a.sh": "bar=2\necho $bar\n", "b.sh": "x=1\recho $bar\n"})
	r := q.refuses(RefuseLineBreaks, "a.sh", "bar", 1, 0, "zzz")
	if !strings.Contains(r.Reason, "b.sh") {
		t.Fatalf("reason %q does not name b.sh", r.Reason)
	}
	q = buildMem(t, map[string]string{"a.sh": "bar=2\necho $bar\n", "b.sh": "x=1\rzzz=3\n"})
	q.refuses(RefuseLineBreaks, "a.sh", "bar", 1, 0, "zzz")
}

// A file the workspace never read (too large, over the file count, unreadable)
// is in no file's view, yet it may call the function: the rename is refused
// and the refusal names the files.
func TestRenameRefusesWhenAFileWasNeverRead(t *testing.T) {
	root := filepath.Join(t.TempDir(), "w")
	lib, main := filepath.Join(root, "lib.sh"), filepath.Join(root, "main.sh")
	src := map[string]string{lib: "f() { :; }\n", main: "source ./lib.sh\nf\n"}
	files := map[string]*FileFacts{}
	for p, s := range src {
		files[p] = Bind(front.Parse(p, []byte(s), front.Detect(p, []byte(s))))
	}
	off := strings.Index(src[lib], "f()")
	unread := []Unanalysed{{filepath.Join(root, "z.sh"), "unreadable"}, {filepath.Join(root, "big.sh"), "too large"}}
	s := Build(files, Options{Root: root, Unanalysed: unread}, 1)
	plan, err := s.PlanRename(lib, off, "g")
	r := refusal(t, err, RefuseSkippedFile)
	if plan != nil {
		t.Fatal("a plan with a refusal")
	}
	if len(r.Sites) != 2 || r.Sites[0].Path != unread[1].Path || r.Sites[1].Path != unread[0].Path || r.Sites[0].Why != "too large" || r.Sites[1].Why != "unreadable" {
		t.Fatalf("sites %v, want big.sh and z.sh", r.Sites)
	}
	if !r.NameIndependent {
		t.Error("not name independent")
	}
	if _, err := s.PlanRenameByName("f", "g"); err == nil {
		t.Error("rename by name accepted")
	}
	// the same snapshot without the report renames
	if _, err := Build(files, Options{Root: root}, 1).PlanRename(lib, off, "g"); err != nil {
		t.Fatalf("without unread files: %v", err)
	}
	// a file of the snapshot that carries Skipped (an open document over the
	// size limit) refuses too
	big := filepath.Join(root, "big.sh")
	files[big] = Bind(front.Parse(big, nil, front.Detect(big, nil)))
	files[big].File.Skipped = "too big"
	if _, err := Build(files, Options{Root: root}, 1).PlanRename(lib, off, "g"); refusal(t, err, RefuseSkippedFile) == nil {
		t.Fatal("no refusal for a skipped file of the snapshot")
	}
}

// The refusal lists at most maxRefusalSites files.
func TestRenameRefusalListsAtMostMaxSitesUnreadFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "w")
	lib := filepath.Join(root, "lib.sh")
	text := "f() { :; }\n"
	files := map[string]*FileFacts{lib: Bind(front.Parse(lib, []byte(text), front.Detect(lib, []byte(text))))}
	var unread []Unanalysed
	for i := 0; i < maxRefusalSites+50; i++ {
		unread = append(unread, Unanalysed{Path: filepath.Join(root, fmt.Sprintf("u%04d.sh", i)), Reason: "unreadable"})
	}
	s := Build(files, Options{Root: root, Unanalysed: unread}, 1)
	_, err := s.PlanRename(lib, 0, "g")
	r := refusal(t, err, RefuseSkippedFile)
	if len(r.Sites) != maxRefusalSites || !strings.Contains(r.Reason, "of 150") {
		t.Fatalf("%d sites, reason %q", len(r.Sites), r.Reason)
	}
}

package analysis

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// refusesMem builds srcs and requires PlanRename at the needle to be refused
// with code; it returns the refusal.
func refusesMem(t *testing.T, srcs map[string]string, code, file, needle string, off int, newName string) *RenameRefusal {
	t.Helper()
	return buildMem(t, srcs).refuses(code, file, needle, 0, off, newName)
}

// applyRename returns src with the edits of path applied.
func applyRename(src []byte, path string, edits []RenameEdit) []byte {
	var out []byte
	at := 0
	for _, e := range edits {
		if e.Path != path {
			continue
		}
		out = append(out, src[at:e.Range.Start.Offset]...)
		out = append(out, e.NewText...)
		at = e.Range.End.Offset
	}
	return append(out, src[at:]...)
}

func TestRenameRefusesHalfTypedBuffers(t *testing.T) {
	for _, c := range []struct {
		name string
		srcs map[string]string
		file string
	}{
		{"unterminated heredoc", map[string]string{"a.sh": "f() { :; }\nf\ncat <<EOF\nf\n"}, "a.sh"},
		{"unterminated double quote", map[string]string{"a.sh": "f() { :; }\necho \"oops\nf\n"}, "a.sh"},
		{"unterminated single quote", map[string]string{"a.sh": "f() { :; }\necho 'abc\nf\n"}, "a.sh"},
		{"gap that mentions the name", map[string]string{"a.sh": "f() { :; }\nf\necho \"oops f\n"}, "a.sh"},
		{"gap in a sourced file", map[string]string{
			"lib.sh":  "f() { :; }\n",
			"main.sh": ". ./lib.sh\necho \"oops f\n",
		}, "lib.sh"},
		{"gap in the sourcing file", map[string]string{
			"lib.sh":  "f() { :; }\n. ./main.sh\n",
			"main.sh": "echo 'abc f\n",
		}, "lib.sh"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := refusesMem(t, c.srcs, RefuseRecovered, c.file, "f()", 0, "g")
			if !strings.HasPrefix(r.Reason, reasonGapAfterEdit) && !strings.HasPrefix(r.Reason, reasonGapMentionsName) {
				t.Fatalf("reason %q", r.Reason)
			}
		})
	}
	// A gap that neither precedes an edit nor mentions the name does not stop a rename.
	q := buildMem(t, map[string]string{"a.sh": "f() { :; }\nf\necho \"oops\n"})
	if _, err := q.plan("a.sh", "f()", 0, 0, "g"); err != nil {
		t.Fatalf("a gap after every edit: %v", err)
	}
}

func TestRenameRefusesDynamicScope(t *testing.T) {
	for _, c := range []struct {
		name, src, needle string
		off               int
	}{
		{"global read by a callee of a function with a local of that name",
			"v=0\ng() {\n  echo \"$v\"\n}\nf() {\n  local v=1\n  g\n}\nf\n", "v=0", 0},
		{"local assigned by a callee",
			"g() {\n  v=5\n}\nf() {\n  local v=1\n  g\n  echo \"$v\"\n}\nf\n", "local v=1", 6},
		{"prefix assignment read by the command it prefixes",
			"x=1\nfoo() { echo $x; }\nx=3 foo\n", "x=1", 0},
		{"local read by a callee two calls down",
			"h() { echo \"$v\"; }\ng() { h; }\nf() {\n  local v=1\n  g\n}\nf\n", "local v=1", 6},
		{"callee reached through a sourced file",
			"f() {\n  local v=1\n  . ./lib.sh\n}\n", "local v=1", 6},
	} {
		t.Run(c.name, func(t *testing.T) {
			srcs := map[string]string{"a.sh": c.src}
			if strings.Contains(c.src, "./lib.sh") {
				srcs["lib.sh"] = "echo \"$v\"\n"
			}
			r := refusesMem(t, srcs, RefuseDynamicScope, "a.sh", c.needle, c.off, "w")
			if r.Reason == "" {
				t.Fatal("no reason")
			}
		})
	}
	// A callee that shadows the name with its own local does not read the caller's.
	q := buildMem(t, map[string]string{"a.sh": "g() {\n  local v=2\n  echo \"$v\"\n}\nf() {\n  local v=1\n  g\n  echo \"$v\"\n}\nf\n"})
	if _, err := q.plan("a.sh", "local v=1", 0, 6, "w"); err != nil {
		t.Fatalf("a shadowing callee refuses: %v", err)
	}
	// A function with a local of that name that reaches no use of the global.
	q = buildMem(t, map[string]string{"a.sh": "v=0\nf() {\n  local v=1\n  echo \"$v\"\n}\nf\necho \"$v\"\n"})
	if _, err := q.plan("a.sh", "v=0", 0, 0, "w"); err != nil {
		t.Fatalf("an unreached global refuses: %v", err)
	}
}

func TestRenameRefusesNameReferences(t *testing.T) {
	for _, c := range []struct{ name, src, needle string }{
		{"unset variable", "x=1\nunset x\n", "x=1"},
		{"unset function", "f() { :; }\nunset -f f\n", "f()"},
		{"test -v in brackets", "x=1\n[[ -v x ]]\n", "x=1"},
		{"test -v", "x=1\ntest -v x\n", "x=1"},
		{"declare -n target", "x=1\ndeclare -n r=x\n", "x=1"},
		{"local -n target", "x=1\nf() {\n  local -n r=x\n  echo \"$r\"\n}\n", "x=1"},
		{"export -f", "f() { :; }\nexport -f f\n", "f()"},
		{"declare -f", "f() { :; }\ndeclare -f f\n", "f()"},
		{"typeset -F", "f() { :; }\ntypeset -F f\n", "f()"},
		{"type", "f() { :; }\ntype f\n", "f()"},
		{"command -v", "f() { :; }\ncommand -v f\n", "f()"},
		{"trap string", "x=1\ntrap 'echo $x' EXIT\n", "x=1"},
		{"eval string", "x=1\neval 'echo $x'\n", "x=1"},
		{"indirect expansion of a variable named by value", "x=1\nn=x\necho \"${!n}\"\n", "x=1"},
		{"nameref target by value", "x=1\nn=x\ndeclare -n r=$n\n", "x=1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := refusesMem(t, map[string]string{"a.sh": c.src}, RefuseNameReference, "a.sh", c.needle, 0, "y")
			if len(r.Sites) == 0 || r.Reason == "" {
				t.Fatalf("refusal without sites: %+v", r)
			}
		})
	}
	// Mentions the net must not trip on.
	for _, src := range []string{
		"x=1\n# unset x\necho \"$x\"\n",
		"x=1\nunset z\n",
		"x=1\nexport x\n",
		"x=1\ndeclare -a arr=(x)\n",
		"f() { :; }\ntype ff\n",
	} {
		q := buildMem(t, map[string]string{"a.sh": src})
		needle := "x=1"
		if strings.HasPrefix(src, "f()") {
			needle = "f()"
		}
		if _, err := q.plan("a.sh", needle, 0, 0, "y"); err != nil {
			t.Errorf("%q: %v", src, err)
		}
	}
	// The sites are listed, each once.
	q := buildMem(t, map[string]string{"a.sh": "x=1\nunset x\n[[ -v x ]]\n"})
	_, err := q.plan("a.sh", "x=1", 0, 0, "y")
	r := refusal(t, err, RefuseNameReference)
	wantSeq(t, q.strs(r.Sites), "a.sh:2", "a.sh:3")
}

func TestRenameConflictsExternalSpecialAndCapture(t *testing.T) {
	t.Run("function onto an external command", func(t *testing.T) {
		refusesMem(t, map[string]string{"a.sh": "f() { :; }\nf\nls -l\n"}, RefuseConflict, "a.sh", "f()", 0, "ls")
	})
	t.Run("function onto an external command called in a sourcing file", func(t *testing.T) {
		refusesMem(t, map[string]string{"lib.sh": "f() { :; }\n", "main.sh": ". ./lib.sh\nf\nls -l\n"}, RefuseConflict, "lib.sh", "f()", 0, "ls")
	})
	t.Run("function onto a name nothing calls", func(t *testing.T) {
		q := buildMem(t, map[string]string{"a.sh": "f() { :; }\nf\nls -l\n"})
		if _, err := q.plan("a.sh", "f()", 0, 0, "lsx"); err != nil {
			t.Fatal(err)
		}
	})
	for _, name := range []string{"IFS", "PATH", "BASH_VERSION", "_", "RANDOM", "LC_ALL"} {
		t.Run("variable onto "+name, func(t *testing.T) {
			r := refusesMem(t, map[string]string{"a.sh": "x=1\necho \"$x\"\n"}, RefuseConflict, "a.sh", "x=1", 0, name)
			if !strings.Contains(r.Reason, "special") {
				t.Fatalf("reason %q", r.Reason)
			}
		})
	}
	t.Run("local onto a global read by a callee", func(t *testing.T) {
		refusesMem(t, map[string]string{"a.sh": "y=0\nh() { echo \"$y\"; }\nf() { local x=1; h; }\nf\n"},
			RefuseConflict, "a.sh", "local x", 6, "y")
	})
	t.Run("global onto a local of a function that reaches its uses", func(t *testing.T) {
		refusesMem(t, map[string]string{"a.sh": "x=1\ng() { local y=2; h; }\nh() { echo \"$x\"; }\ng\n"},
			RefuseConflict, "a.sh", "x=1", 0, "y")
	})
	t.Run("global onto a global", func(t *testing.T) {
		refusesMem(t, map[string]string{"a.sh": "a=1\nb=2\necho \"$a $b\"\n"}, RefuseConflict, "a.sh", "a=1", 0, "b")
	})
	t.Run("private variable onto another use in its function", func(t *testing.T) {
		refusesMem(t, map[string]string{"a.sh": "f() {\n  ( x=1; echo \"$x $other\" )\n}\n"}, RefuseConflict, "a.sh", "x=1", 0, "other")
	})
	t.Run("local onto a global used only in another function", func(t *testing.T) {
		q := buildMem(t, map[string]string{"a.sh": "other=1\ng() { echo \"$other\"; }\nf() {\n  local x=1\n  echo \"$x\"\n}\nf\n"})
		plan, err := q.plan("a.sh", "local x", 0, 6, "other")
		if err != nil {
			t.Fatal(err)
		}
		wantSeq(t, q.editStrs(plan), "a.sh:4", "a.sh:5")
	})
	t.Run("private variable at file level", func(t *testing.T) {
		refusesMem(t, map[string]string{"a.sh": "( x=1; echo \"$x $other\" )\n"}, RefuseConflict, "a.sh", "x=1", 0, "other")
		q := buildMem(t, map[string]string{"a.sh": "( x=1; echo \"$x\" )\n"})
		plan, err := q.plan("a.sh", "x=1", 0, 0, "z")
		if err != nil || len(plan.Edits) != 2 {
			t.Fatalf("plan %v, %v", plan, err)
		}
	})
}

func TestRenameToSameNameIsRefused(t *testing.T) {
	q := buildCorpus(t)
	q.refuses(RefuseNoOp, "lib/util.sh", "build_release()", 0, 2, "build_release")
	if _, err := q.s.PlanRenameByName("build_release", "build_release"); err == nil {
		t.Fatal("by name")
	} else {
		refusal(t, err, RefuseNoOp)
	}
}

func TestRenameCursorJustAfterName(t *testing.T) {
	q := buildMem(t, map[string]string{"a.sh": "f() { :; }\nf\n"})
	inside, err := q.plan("a.sh", "f\n", 0, 0, "g")
	if err != nil {
		t.Fatal(err)
	}
	after, err := q.plan("a.sh", "f\n", 0, 1, "g") // on the newline that follows the name
	if err != nil {
		t.Fatal(err)
	}
	wantSeq(t, q.editStrs(after), q.editStrs(inside)...)
	p, off := q.at("a.sh", "f() { :; }", 0, 0)
	if _, _, err := q.s.PrepareRename(p, off+1); err != nil {
		t.Fatalf("PrepareRename just after the name: %v", err)
	}
}

func TestRenameAliasCallSites(t *testing.T) {
	t.Run("an escaped call is no alias use", func(t *testing.T) {
		q := buildMem(t, map[string]string{"a.sh": "br() { :; }\nalias br=:\nbr\n\\br\n"})
		plan, err := q.plan("a.sh", "alias br", 0, 6, "zz")
		if err != nil {
			t.Fatal(err)
		}
		wantSeq(t, q.editStrs(plan), "a.sh:2", "a.sh:3")
		wantSeq(t, q.exclStrs(plan))
	})
	t.Run("a call inside a string is an exclusion", func(t *testing.T) {
		q := buildMem(t, map[string]string{"a.sh": "f() { :; }\nalias br=f\nbr\neval \"br\"\n"})
		plan, err := q.plan("a.sh", "alias br", 0, 6, "zz")
		if err != nil {
			t.Fatal(err)
		}
		wantSeq(t, q.editStrs(plan), "a.sh:2", "a.sh:3")
		wantSeq(t, q.exclStrs(plan), "a.sh:4 candidate/string-eval")
	})
	t.Run("an alias that leads nowhere is renamed with its calls", func(t *testing.T) {
		q := buildMem(t, map[string]string{"a.sh": "alias ll='ls -l'\nll\nll\n"})
		plan, err := q.plan("a.sh", "ll\n", 0, 0, "lg")
		if err != nil {
			t.Fatal(err)
		}
		wantSeq(t, q.editStrs(plan), "a.sh:1", "a.sh:2", "a.sh:3")
	})
	t.Run("two aliases of one name that a third file sees", func(t *testing.T) {
		// b.sh and c.sh each define g and cannot see each other; a.sh, which both
		// source, calls g and belongs to the first one.
		q := buildMem(t, map[string]string{
			"a.sh": "g\n",
			"b.sh": ". ./a.sh\nalias g=:\ng\n",
			"c.sh": ". ./a.sh\nalias g=:\ng\n",
		})
		plan, err := q.plan("c.sh", "alias g", 0, 6, "h")
		if err != nil {
			t.Fatal(err)
		}
		wantSeq(t, q.editStrs(plan), "c.sh:2", "c.sh:3")
		plan, err = q.plan("b.sh", "alias g", 0, 6, "h")
		if err != nil {
			t.Fatal(err)
		}
		wantSeq(t, q.editStrs(plan), "a.sh:1", "b.sh:2", "b.sh:3")
	})
	t.Run("a file outside the alias view is left alone", func(t *testing.T) {
		q := buildMem(t, map[string]string{"a.sh": "alias g=:\ng\n", "b.sh": "alias g=:\ng\n"})
		plan, err := q.plan("a.sh", "alias g", 0, 6, "h")
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range plan.Edits {
			if q.rel(e.Path) == "b.sh" {
				t.Fatalf("edit in b.sh: %v", q.editStrs(plan))
			}
		}
		wantSeq(t, q.editStrs(plan), "a.sh:1", "a.sh:2")
	})
}

func TestRenameInexactNameDecidedByText(t *testing.T) {
	// A word with a line continuation inside its name is read as the name by the
	// binder but its text is not the name: it is excluded, never edited.
	q := buildMem(t, map[string]string{"a.sh": "f() { :; }\nf\nf\\\n\nf\n"})
	plan, err := q.plan("a.sh", "f()", 0, 0, "g")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range plan.Edits {
		got := string(q.src("a.sh")[e.Range.Start.Offset:e.Range.End.Offset])
		if got != "f" {
			t.Fatalf("edit covers %q", got)
		}
	}
	for _, x := range plan.Excluded {
		if x.Text == "f" {
			t.Fatalf("exclusion of an exact name: %+v", x)
		}
	}
}

func TestRenameCRLFCallWords(t *testing.T) {
	q := buildMem(t, map[string]string{"a.sh": "f() { :; }\r\nf\r\nf\r\n"})
	plan, err := q.plan("a.sh", "f()", 0, 0, "g")
	if err != nil {
		t.Fatal(err)
	}
	wantSeq(t, q.editStrs(plan), "a.sh:1", "a.sh:2", "a.sh:3")
	wantSeq(t, q.exclStrs(plan))
	got := string(applyRename(q.src("a.sh"), q.abs("a.sh"), plan.Edits))
	if got != "g() { :; }\r\ng\r\ng\r\n" {
		t.Fatalf("applied: %q", got)
	}
}

func TestRenameBOMBeforeDefinition(t *testing.T) {
	src := "\xef\xbb\xbff() { :; }\nf\n"
	q := buildMem(t, map[string]string{"a.sh": src})
	plan, err := q.plan("a.sh", "f\n", 0, 0, "g")
	if err != nil {
		t.Fatal(err)
	}
	wantSeq(t, q.editStrs(plan), "a.sh:1", "a.sh:2")
	if got := string(applyRename(q.src("a.sh"), q.abs("a.sh"), plan.Edits)); got != "\xef\xbb\xbfg() { :; }\ng\n" {
		t.Fatalf("applied: %q", got)
	}
}

func TestRenameWarnsOfMissingStaticSource(t *testing.T) {
	q := buildMem(t, map[string]string{"a.sh": "source ./missing.sh\nf() { :; }\nf\n"})
	plan, err := q.plan("a.sh", "f()", 0, 0, "g")
	if err != nil {
		t.Fatal(err)
	}
	wantSeq(t, plan.Warnings, "graph incomplete: the sourced file ./missing.sh was not found and may call f")
	q = buildMem(t, map[string]string{"a.sh": "source \"$X\"\nf() { :; }\nf\n"})
	plan, err = q.plan("a.sh", "f()", 0, 0, "g")
	if err != nil {
		t.Fatal(err)
	}
	wantSeq(t, plan.Warnings, "graph incomplete: a dynamic source may call f")
}

func TestRenameCapsReportLimits(t *testing.T) {
	hit := func(s *Snapshot, what string) bool {
		for _, l := range s.LimitsHit() {
			if strings.Contains(l, what) {
				return true
			}
		}
		return false
	}
	t.Run("others", func(t *testing.T) {
		srcs := map[string]string{"a.sh": "f() { :; }\n"}
		for i := 0; i < maxRenameOthers+5; i++ {
			srcs[fmt.Sprintf("o%04d.sh", i)] = "f() { :; }\n"
		}
		q := buildMem(t, srcs)
		if _, err := q.plan("a.sh", "f()", 0, 0, "g"); err != nil {
			t.Fatal(err)
		}
		if !hit(q.s, "rename others cap") {
			t.Fatalf("LimitsHit %q", q.s.LimitsHit())
		}
	})
	t.Run("refusal sites", func(t *testing.T) {
		src := "f() { :; }\nif true; then\n" + strings.Repeat("  f\n", maxRefusalSites+50)
		q := buildMem(t, map[string]string{"a.sh": src})
		r := q.refuses(RefuseRecovered, "a.sh", "f()", 0, 0, "g")
		if len(r.Sites) != maxRefusalSites || !strings.Contains(r.Reason, fmt.Sprintf("first %d of %d", maxRefusalSites, maxRefusalSites+50)) {
			t.Fatalf("%d sites, reason %q", len(r.Sites), r.Reason)
		}
		if !hit(q.s, "rename sites cap") {
			t.Fatalf("LimitsHit %q", q.s.LimitsHit())
		}
	})
	t.Run("excluded text", func(t *testing.T) {
		src := "f() { :; }\nf" + strings.Repeat("\\\n", maxExcludedText/2+100) + "\n"
		q := buildMem(t, map[string]string{"a.sh": src})
		plan, err := q.plan("a.sh", "f()", 0, 0, "g")
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Excluded) != 1 || len(plan.Excluded[0].Text) > maxExcludedText || !utf8.ValidString(plan.Excluded[0].Text) {
			t.Fatalf("exclusions %+v", plan.Excluded)
		}
		if !hit(q.s, "rename text cap") {
			t.Fatalf("LimitsHit %q", q.s.LimitsHit())
		}
	})
	t.Run("excluded text is cut on a rune boundary", func(t *testing.T) {
		long := strings.Repeat("é", maxExcludedText) // two bytes each: the cut falls inside one when odd
		src := "f() { :; }\nf" + strings.Repeat("\\\n", maxExcludedText/2) + "é" + long + "\n"
		q := buildMem(t, map[string]string{"a.sh": src})
		plan, _ := q.plan("a.sh", "f()", 0, 0, "g")
		for _, x := range plan.Excluded {
			if len(x.Text) > maxExcludedText || !utf8.ValidString(x.Text) {
				t.Fatalf("text of %d bytes, valid %v", len(x.Text), utf8.ValidString(x.Text))
			}
		}
	})
	t.Run("same-named functions by name", func(t *testing.T) {
		srcs := map[string]string{}
		for i := 0; i < maxRenameByName+5; i++ {
			srcs[fmt.Sprintf("o%04d.sh", i)] = "f() { :; }\n"
		}
		q := buildMem(t, srcs)
		_, err := q.s.PlanRenameByName("f", "g")
		r := refusal(t, err, RefuseAmbiguous)
		if len(r.Sites) != maxRefusalSites {
			t.Fatalf("%d sites", len(r.Sites))
		}
		if !hit(q.s, "rename by-name cap") {
			t.Fatalf("LimitsHit %q", q.s.LimitsHit())
		}
	})
}

// renameTime is the best of tries runs of PlanRename of the function f of a.sh.
func renameTime(t *testing.T, src, needle string, tries int) time.Duration {
	t.Helper()
	q := buildMem(t, map[string]string{"a.sh": src})
	p, off := q.at("a.sh", needle, 0, 0)
	best := time.Duration(0)
	for i := 0; i < tries; i++ {
		start := time.Now()
		if _, err := q.s.PlanRename(p, off, "g"); err != nil {
			var r *RenameRefusal
			if !errors.As(err, &r) {
				t.Fatal(err)
			}
		}
		if d := time.Since(start); best == 0 || d < best {
			best = d
		}
	}
	return best
}

// Rename work grows with the sites: the best of three runs, the mean of three
// doublings, under x2.5.
func TestRenameGrowth(t *testing.T) {
	type shape struct {
		needle string
		gen    func(n int) string
	}
	shapes := map[string]shape{
		"call sites": {"f() {", func(n int) string {
			return "f() { :; }\n" + strings.Repeat("f\n", n)
		}},
		"chain with locals": {"v=0", func(n int) string {
			var b strings.Builder
			b.WriteString("v=0\nf() { :; }\n")
			for i := 0; i < n; i++ {
				fmt.Fprintf(&b, "g%d() {\n  local v=%d\n  f\n  g%d\n}\n", i, i, i+1)
			}
			fmt.Fprintf(&b, "g%d() { echo \"$v\"; }\n", n)
			return b.String()
		}},
	}
	sizes := []int{2500, 5000, 10000, 20000}
	if raceBuild {
		sizes = []int{500, 1000, 2000, 4000}
	}
	for name, sh := range shapes {
		t.Run(name, func(t *testing.T) {
			var times []time.Duration
			done := withinBound(t, name, 60*time.Second, func() {
				for _, n := range sizes {
					times = append(times, renameTime(t, sh.gen(n), sh.needle, 3))
				}
			})
			if !done {
				return
			}
			var sum float64
			for i := 1; i < len(times); i++ {
				sum += float64(times[i]) / float64(times[i-1])
			}
			mean := sum / float64(len(times)-1)
			t.Logf("%v, mean x%.2f per doubling", times, mean)
			if mean > 2.5 {
				t.Fatalf("grew x%.2f per doubling over %v", mean, times)
			}
		})
	}
}

func TestRenameDynamicScopeSeesImplicitDeclarations(t *testing.T) {
	for _, body := range []string{"read v", "mapfile v", "printf -v v x", "for v in 1; do :; done", "getopts a v", "(( v++ ))", "let v=1"} {
		t.Run(body, func(t *testing.T) {
			srcs := map[string]string{"a.sh": "g() {\n  " + body + "\n}\nf() {\n  local v=1\n  g\n}\nf\n"}
			refusesMem(t, srcs, RefuseDynamicScope, "a.sh", "local v", 6, "w")
		})
	}
}

func TestRenameOthersAreSorted(t *testing.T) {
	q := buildMem(t, map[string]string{
		"a.sh": ". ./d.sh\nf() { :; }\n",
		"d.sh": "f() { :; }\n",
		"b.sh": ". ./c.sh\nf() { :; }\n",
		"c.sh": "f() { :; }\n",
		"z.sh": "f() { :; }\nf\n",
	})
	plan, err := q.plan("z.sh", "f()", 0, 0, "g")
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, l := range plan.Others {
		got = append(got, fmt.Sprintf("%s:%d", q.rel(l.Path), l.Range.Start.Line))
	}
	wantSeq(t, got, "a.sh:2", "b.sh:2", "c.sh:1", "d.sh:1")
}

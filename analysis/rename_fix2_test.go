package analysis

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wamra/bash-lsp/internal/front"
)

// planOK requires that PlanRename of the needle of a.sh to newName is not refused.
func planOK(t *testing.T, srcs map[string]string, file, needle string, off int, newName string) *RenamePlan {
	t.Helper()
	p, err := buildMem(t, srcs).plan(file, needle, 0, off, newName)
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	return p
}

func TestRenameRefusesNamePassedToFunction(t *testing.T) {
	for _, c := range []struct {
		name, src, needle string
		off               int
	}{
		{"nameref parameter", "x=1\nset_it() {\n  local -n ref=$1\n  ref=2\n}\nset_it x\n", "x=1", 0},
		{"indirect parameter", "x=1\nget_it() {\n  echo \"${!1}\"\n}\nget_it x\n", "x=1", 0},
		{"printf -v parameter", "x=1\nput_it() {\n  printf -v \"$1\" %s 2\n}\nput_it x\n", "x=1", 0},
		{"quoted argument", "x=1\nset_it() { :; }\nset_it \"x\"\n", "x=1", 0},
		{"local variable", "set_it() { :; }\nf() {\n  local x=1\n  set_it x\n}\n", "local x=1", 6},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := refusesMem(t, map[string]string{"a.sh": c.src}, RefuseNameReference, "a.sh", c.needle, c.off, "w")
			if len(r.Sites) == 0 {
				t.Fatal("no sites")
			}
		})
	}
	// An argument of an external command, a builtin or another name is no reference.
	for _, src := range []string{
		"x=1\ngrep x file\n",
		"x=1\necho x\n",
		"x=1\nfoo() { :; }\nfoo y\n",
		"x=1\nfoo() { :; }\nfoo \"x y\"\n",
	} {
		planOK(t, map[string]string{"a.sh": src}, "a.sh", "x=1", 0, "w")
	}
	// A function passed by name is not read as a variable argument.
	planOK(t, map[string]string{"a.sh": "x=1\nf() {\n  local x=2\n  echo $x\n}\nf\ng() { :; }\ng x\n"}, "a.sh", "local x=2", 6, "w")
}

func TestRenameDynamicScopeTextualShadowing(t *testing.T) {
	for _, c := range []struct {
		name, src, needle string
		off               int
	}{
		{"callee reads before its own local, rename the caller's local",
			"g() {\n  echo \"$v\"\n  local v=2\n}\nf() {\n  local v=1\n  g\n}\nf\n", "local v=1", 6},
		{"callee reads before its own local, rename the global",
			"v=0\ng() {\n  echo \"$v\"\n  local v=2\n}\nf() {\n  local v=1\n  g\n}\nf\n", "v=0", 0},
		{"callee shadows only behind a condition",
			"g() {\n  if true; then\n    local v=2\n  fi\n  echo \"$v\"\n}\nf() {\n  local v=1\n  g\n}\nf\n", "local v=1", 6},
		{"callee shadows behind a short-circuit",
			"g() {\n  true && local v=2\n  echo \"$v\"\n}\nf() {\n  local v=1\n  g\n}\nf\n", "local v=1", 6},
	} {
		t.Run(c.name, func(t *testing.T) {
			refusesMem(t, map[string]string{"a.sh": c.src}, RefuseDynamicScope, "a.sh", c.needle, c.off, "w")
		})
	}
	// A local at the top level of the callee shadows the uses after it, and its initializer
	// is the only use before it that the caller's variable does not reach.
	planOK(t, map[string]string{"a.sh": "g() {\n  local v=2\n  echo \"$v\"\n}\nf() {\n  local v=1\n  g\n}\nf\n"}, "a.sh", "local v=1", 6, "w")
}

func TestRenameDynamicScopeSubshellsAndDynamicCalls(t *testing.T) {
	for _, c := range []struct {
		name, src, needle string
		off               int
		code              string
	}{
		{"subshell in a function", "g() { echo \"$v\"; }\nf() { ( v=1; g ); }\nf\n", "v=1", 0, RefuseDynamicScope},
		{"subshell at file level", "g() { echo \"$v\"; }\n( v=1; g )\n", "v=1", 0, RefuseDynamicScope},
		{"command substitution", "g() { echo \"$v\"; }\nout=$(v=1; g)\n", "v=1", 0, RefuseDynamicScope},
		{"local in a subshell", "g() { echo \"$v\"; }\nf() {\n  ( local v=1; g )\n}\nf\n", "v=1", 0, RefuseDynamicScope},
		{"callee writes the subshell variable", "g() { v=5; }\n( v=1; g; echo \"$v\" )\n", "v=1", 0, RefuseDynamicScope},
		{"global renamed while a subshell holds the name", "v=0\ng() { echo \"$v\"; }\nf() { ( v=1; g ); }\nf\n", "v=0", 0, RefuseDynamicScope},
		{"dynamic call through $@", "retry() {\n  local n=3\n  \"$@\"\n}\ng() { echo \"$n\"; }\nretry g\n", "local n=3", 6, RefuseDynamicScope},
		{"dynamic call through a variable", "retry() {\n  local n=3\n  $cb\n}\ng() { echo \"$n\"; }\n", "local n=3", 6, RefuseDynamicScope},
		{"dynamic call through eval", "retry() {\n  local n=3\n  eval \"$cmd\"\n}\ng() { echo \"$n\"; }\n", "local n=3", 6, RefuseDynamicScope},
		{"dynamic call through command", "retry() {\n  local n=3\n  command $cb\n}\ng() { echo \"$n\"; }\n", "local n=3", 6, RefuseDynamicScope},
		{"global renamed while a declarer calls dynamically", "n=0\nretry() {\n  local n=3\n  \"$@\"\n}\n", "n=0", 0, RefuseDynamicScope},
		{"dynamic call in a subshell scope", "g() { echo \"$v\"; }\n( v=1; \"$@\" )\n", "v=1", 0, RefuseDynamicScope},
	} {
		t.Run(c.name, func(t *testing.T) {
			refusesMem(t, map[string]string{"a.sh": c.src}, c.code, "a.sh", c.needle, c.off, "w")
		})
	}
	// Subshell variables nothing reaches, functions that call nothing dynamically, and
	// dynamic calls where no other code reads the name.
	for _, c := range []struct {
		src, needle string
		off         int
	}{
		{"( v=1; echo \"$v\" )\n", "v=1", 0},
		{"g() { echo hi; }\nf() { ( v=1; g; echo \"$v\" ); }\nf\n", "v=1", 0},
		{"f() {\n  local n=3\n  echo \"$n\"\n}\n", "local n=3", 6},
		{"n=0\nf() {\n  local m=3\n  \"$@\"\n}\n", "n=0", 0},
		{"f() {\n  local n=3\n  $cb\n}\n", "local n=3", 6},
		{"f() {\n  local n=3\n  command $cb\n}\n", "local n=3", 6},
	} {
		planOK(t, map[string]string{"a.sh": c.src}, "a.sh", c.needle, c.off, "w")
	}
}

func TestRenameReachCapIsEnforced(t *testing.T) {
	old := maxRenameReach
	maxRenameReach = 50
	defer func() { maxRenameReach = old }()
	chain := func(n int) string {
		var b strings.Builder
		b.WriteString("v=0\nf() {\n  local v=1\n  g0\n}\n")
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "g%d() { g%d; }\n", i, i+1)
		}
		fmt.Fprintf(&b, "g%d() { :; }\n", n)
		return b.String()
	}
	q := buildMem(t, map[string]string{"a.sh": chain(maxRenameReach + 20)})
	r := q.refuses(RefuseDynamicScope, "a.sh", "v=0", 0, 0, "w")
	if r.Reason != reasonReachCap {
		t.Fatalf("reason %q", r.Reason)
	}
	found := false
	for _, l := range q.s.LimitsHit() {
		found = found || strings.Contains(l, "rename reach cap") && strings.Contains(l, fmt.Sprint(maxRenameReach))
	}
	if !found {
		t.Fatalf("LimitsHit %q", q.s.LimitsHit())
	}
	// A chain inside the bound is checked in full and passes.
	planOK(t, map[string]string{"a.sh": chain(maxRenameReach - 10)}, "a.sh", "v=0", 0, "w")
	// The same bound holds for the prefix-assignment and the local checks.
	var b strings.Builder
	b.WriteString("x=1\nfoo() { g0; }\n")
	for i := 0; i < maxRenameReach+20; i++ {
		fmt.Fprintf(&b, "g%d() { g%d; }\n", i, i+1)
	}
	fmt.Fprintf(&b, "g%d() { echo \"$x\"; }\n", maxRenameReach+20)
	b.WriteString("x=3 foo\n")
	r = refusesMem(t, map[string]string{"a.sh": b.String()}, RefuseDynamicScope, "a.sh", "x=1", 0, "w")
	if r.Reason != reasonReachCap {
		t.Fatalf("prefix assignment reason %q", r.Reason)
	}
}

func TestRenameNameReferencesOfWrappedAndRegisteredNames(t *testing.T) {
	for _, c := range []struct{ name, src, needle string }{
		{"builtin unset", "x=1\nbuiltin unset x\n", "x=1"},
		{"command unset", "x=1\ncommand unset x\n", "x=1"},
		{"unset -v", "x=1\nunset -v x\n", "x=1"},
		{"complete -F", "f() { :; }\ncomplete -F f cmd\n", "f()"},
		{"prompt command", "f() { :; }\nPROMPT_COMMAND=f\n", "f()"},
		{"prompt command with arguments", "f() { :; }\nPROMPT_COMMAND=\"f; true\"\n", "f()"},
		{"builtin unset -f", "f() { :; }\nbuiltin unset -f f\n", "f()"},
	} {
		t.Run(c.name, func(t *testing.T) {
			refusesMem(t, map[string]string{"a.sh": c.src}, RefuseNameReference, "a.sh", c.needle, 0, "y")
		})
	}
}

func TestRenameNameReferencesRespectKindAndScope(t *testing.T) {
	// A variable form blocks only variables; a function form only functions.
	planOK(t, map[string]string{"a.sh": "f() { :; }\nunset x\n[[ -v f ]]\ndeclare -n r=f\necho \"${!n}\"\n"}, "a.sh", "f()", 0, "g")
	planOK(t, map[string]string{"a.sh": "jq=1\ntype -P jq\ncommand -v jq\nexport -f jq\ndeclare -f jq\ncomplete -F jq cmd\necho \"$jq\"\n"}, "a.sh", "jq=1", 0, "w")
	// A local is blocked only by references in its function or in what that function reaches.
	planOK(t, map[string]string{"a.sh": "f() {\n  local file=1\n  echo \"$file\"\n}\nunset file\n"}, "a.sh", "local file=1", 6, "w")
	planOK(t, map[string]string{"a.sh": "f() {\n  local file=1\n  echo \"$file\"\n}\nh() { unset file; }\n"}, "a.sh", "local file=1", 6, "w")
	refusesMem(t, map[string]string{"a.sh": "f() {\n  local file=1\n  unset file\n}\n"}, RefuseNameReference, "a.sh", "local file=1", 6, "w")
	refusesMem(t, map[string]string{"a.sh": "f() {\n  local file=1\n  g\n}\ng() { unset file; }\n"}, RefuseNameReference, "a.sh", "local file=1", 6, "w")
	// A name inside a path, an option or a label of a trap or eval string is another word.
	for _, src := range []string{
		"git() { :; }\ntrap 'rm -rf .git' EXIT\n",
		"git() { :; }\ntrap 'cd /tmp/git' EXIT\n",
		"git() { :; }\neval 'echo my-git'\n",
		"git() { :; }\neval 'echo git:x'\n",
		"git() { :; }\neval 'echo --git-dir'\n",
	} {
		planOK(t, map[string]string{"a.sh": src}, "a.sh", "git()", 0, "vcs")
	}
	refusesMem(t, map[string]string{"a.sh": "git() { :; }\ntrap 'echo git' EXIT\n"}, RefuseNameReference, "a.sh", "git()", 0, "vcs")
}

func TestRenameRefusesRenamingASpecialVariable(t *testing.T) {
	for _, c := range []struct{ name, src, needle string }{
		{"IFS", "IFS=,\nread -r a b\n", "IFS=,"},
		{"PATH", "PATH=/x\n", "PATH=/x"},
		{"local IFS", "f() {\n  local IFS=,\n  read -r a\n}\n", "IFS=,"},
		{"RANDOM", "RANDOM=5\n", "RANDOM=5"},
		{"environment variable", "HOME=/x\n", "HOME=/x"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := refusesMem(t, map[string]string{"a.sh": c.src}, RefuseConflict, "a.sh", c.needle, 0, "mine")
			if !strings.Contains(r.Reason, "renaming a shell-special variable") {
				t.Fatalf("reason %q", r.Reason)
			}
		})
	}
}

func TestRenameRefusesNewNameInEvalOrTrapStrings(t *testing.T) {
	for _, c := range []struct{ name, src string }{
		{"eval at file level", "x=1\neval 'echo $y'\n"},
		{"trap in the editing function", "f() {\n  x=1\n  trap 'echo $y' EXIT\n}\n"},
		{"eval in a callee of the editing function", "g() { eval 'echo $y'; }\nf() {\n  x=1\n  g\n}\n"},
		{"new name as a bare word", "x=1\neval 'y=2'\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := refusesMem(t, map[string]string{"a.sh": c.src}, RefuseConflict, "a.sh", "x=1", 0, "y")
			if !strings.Contains(r.Reason, "eval or trap") {
				t.Fatalf("reason %q", r.Reason)
			}
		})
	}
	// An eval string the edits do not reach, or one that holds the name inside another word.
	planOK(t, map[string]string{"a.sh": "f() { x=1; }\ng() { eval 'echo $y'; }\n"}, "a.sh", "x=1", 0, "y")
	planOK(t, map[string]string{"a.sh": "x=1\neval 'echo $yes'\n"}, "a.sh", "x=1", 0, "y")
}

func TestRenameEdgeCasesOfTheBounds(t *testing.T) {
	// A symbol whose only site is a call renamed to a called command reports the called-command reason.
	r := refusesMem(t, map[string]string{"a.sh": "f() { :; }\nf\nls -l\n"}, RefuseConflict, "a.sh", "f()", 0, "ls")
	if r.Reason != reasonCalledCommand {
		t.Fatalf("reason %q", r.Reason)
	}
}

// Rename time on the shapes that were quadratic: prefix assignments, many edit
// sites, a long chain of functions.
func TestRenameGrowthOfCheckedShapes(t *testing.T) {
	type shape struct {
		needle string
		gen    func(n int) string
	}
	shapes := map[string]shape{
		"prefix assignments": {"x=1", func(n int) string {
			var b strings.Builder
			b.WriteString("x=1\n")
			for i := 0; i < n; i++ {
				fmt.Fprintf(&b, "foo%d() { echo $x; }\nx=3 foo%d\n", i, i)
			}
			return b.String()
		}},
		"many edit sites": {"v=0", func(n int) string {
			return "v=0\nf() {\n  local v=1\n  echo $v\n}\n" + strings.Repeat("echo $v\n", n)
		}},
		"chain of functions": {"v=0", func(n int) string {
			var b strings.Builder
			b.WriteString("v=0\nf() {\n  local v=1\n  g0\n}\n")
			for i := 0; i < n; i++ {
				fmt.Fprintf(&b, "g%d() { g%d; }\n", i, i+1)
			}
			fmt.Fprintf(&b, "g%d() { echo \"$v\"; }\n", n)
			return b.String()
		}},
	}
	sizes := []int{2000, 4000, 8000, 16000}
	if raceBuild {
		sizes = []int{250, 500, 1000, 2000}
	}
	for name, sh := range shapes {
		t.Run(name, func(t *testing.T) {
			var times []time.Duration
			done := withinBound(t, name, 60*time.Second, func() {
				for _, n := range sizes {
					times = append(times, renameTime(t, sh.gen(n), sh.needle, 5))
				}
			})
			if !done {
				return
			}
			var sum float64
			for i := 1; i < len(times); i++ {
				sum += float64(times[i]) / float64(max(times[i-1], time.Millisecond))
			}
			mean := sum / float64(len(times)-1)
			t.Logf("%s: %v, mean x%.2f per doubling", name, times, mean)
			if mean > 2.5 {
				t.Fatalf("grew x%.2f per doubling over %v", mean, times)
			}
		})
	}
}

func buildMemOpts(t *testing.T, srcs map[string]string, opts Options) *qworld {
	t.Helper()
	root := filepath.Join(t.TempDir(), "ws")
	files := map[string]*FileFacts{}
	for name, s := range srcs {
		p := filepath.Join(root, filepath.FromSlash(name))
		b := []byte(s)
		files[p] = Bind(front.Parse(p, b, front.Detect(p, b)))
	}
	opts.Root = root
	return &qworld{t: t, root: root, s: Build(files, opts, 1)}
}

func TestRenameRefusesNameGivenToAnUnknownCommand(t *testing.T) {
	opts := Options{Commands: fakeProfile("grep")}
	// A command that is no function of the workspace and not in the PATH profile may be a
	// function defined elsewhere: a name it is given may be read indirectly.
	q := buildMemOpts(t, map[string]string{"a.sh": "x=1\n_get_words -w x\n"}, opts)
	q.refuses(RefuseNameReference, "a.sh", "x=1", 0, 0, "w")
	// A command of the profile is external: its arguments are plain words.
	q = buildMemOpts(t, map[string]string{"a.sh": "x=1\ngrep x file\n"}, opts)
	if _, err := q.plan("a.sh", "x=1", 0, 0, "w"); err != nil {
		t.Fatal(err)
	}
}

func TestRenameShadowingByALocalInTheSameBlock(t *testing.T) {
	// A local shadows the uses after it in its own block, conditional or not.
	planOK(t, map[string]string{"a.sh": "g() {\n  if true; then\n    local v=2\n    echo \"$v\"\n  fi\n}\nf() {\n  local v=1\n  g\n}\nf\n"}, "a.sh", "local v=1", 6, "w")
	// A use in the initializer of the local is still the caller's.
	refusesMem(t, map[string]string{"a.sh": "g() {\n  local v=\"$v\"\n}\nf() {\n  local v=1\n  g\n}\nf\n"}, RefuseDynamicScope, "a.sh", "local v=1", 6, "w")
}

func TestRenameRefusesVariableInEvalStringBeforeJoiners(t *testing.T) {
	// A variable ends at a dot or a dash, so a name before one is a use.
	for _, s := range []string{"x=1\neval 'echo $x.txt'\n", "x=1\neval 'echo ${x}-1'\n", "x=1\ntrap 'cat $x/dir' EXIT\n"} {
		refusesMem(t, map[string]string{"a.sh": s}, RefuseNameReference, "a.sh", "x=1", 0, "y")
	}
}

func TestRenameConfinedVariablesIgnoreTheirOwnFunction(t *testing.T) {
	// The function around a subshell variable reads other variables of that name itself.
	planOK(t, map[string]string{"a.sh": "v=0\nf() {\n  ( v=1; echo \"$v\" )\n  echo \"$v\"\n}\n"}, "a.sh", "v=1", 0, "w")
	// A local declared under a condition is edited with its function; the uses after
	// the condition are the function's own.
	planOK(t, map[string]string{"a.sh": "f() {\n  if true; then\n    local v=1\n  fi\n  echo \"$v\"\n}\n"}, "a.sh", "local v=1", 6, "w")
	// A function's own uses of a name do not make `type -P` or a nameref to an element a hazard.
	planOK(t, map[string]string{"a.sh": "jq() { :; }\ntype -P jq\n"}, "a.sh", "jq()", 0, "w")
	refusesMem(t, map[string]string{"a.sh": "x=(1)\ndeclare -n r='x[0]'\n"}, RefuseNameReference, "a.sh", "x=(1)", 0, "y")
}

// A byte order mark before a command word is part of that word for the shell, so
// a call at the start of a BOM file names another command and is not renamed; a
// definition is read leniently (the mark is not part of its name), so that
// editing it never rewrites the mark.
func TestRenameBOMBeforeCallIsNotACall(t *testing.T) {
	q := buildMem(t, map[string]string{"a.sh": "\xef\xbb\xbff\nf() { :; }\nf\n"})
	plan, err := q.plan("a.sh", "f() {", 0, 0, "g")
	if err != nil {
		t.Fatal(err)
	}
	wantSeq(t, q.editStrs(plan), "a.sh:2", "a.sh:3")
}

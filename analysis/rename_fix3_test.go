package analysis

import (
	"fmt"
	"strings"
	"testing"
)

func TestRenameRefusesNameGivenToADynamicallyNamedFunction(t *testing.T) {
	for _, c := range []struct{ name, src string }{
		{"command word from a variable", "x=1\nset_it() {\n  local -n r=$1\n  r=2\n}\ncb=set_it\n$cb x\n"},
		{"alias of a function", "x=1\nset_it() { :; }\nshopt -s expand_aliases\nalias si=set_it\nsi x\n"},
		{"function named by a parameter", "x=1\nset_it() { :; }\nrun() {\n  local h=$1\n  \"$h\" x\n}\nrun set_it\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			refusesMem(t, map[string]string{"a.sh": c.src}, RefuseNameReference, "a.sh", "x=1", 0, "y")
		})
	}
	// An external command stays a plain word, also when it is spelled through a variable-free name.
	opts := Options{Commands: fakeProfile("grep")}
	q := buildMemOpts(t, map[string]string{"a.sh": "x=1\ngrep x file\n"}, opts)
	if _, err := q.plan("a.sh", "x=1", 0, 0, "y"); err != nil {
		t.Fatal(err)
	}
}

func TestRenameDynamicCallObservers(t *testing.T) {
	for _, c := range []struct{ name, src string }{
		{"callee evals the name", "retry() {\n  local n=3\n  \"$@\"\n}\ng() { eval 'echo $n'; }\nretry g\n"},
		{"callee tests the name", "retry() {\n  local n=3\n  \"$@\"\n}\ng() { [[ -v n ]]; }\nretry g\n"},
		{"callee expands the name indirectly", "retry() {\n  local n=3\n  \"$@\"\n}\ng() { echo \"${!1}\"; }\nretry g n\n"},
		{"callee unsets the name", "retry() {\n  local n=3\n  \"$@\"\n  echo \"$n\"\n}\ng() { unset n; }\nretry g\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			refusesMem(t, map[string]string{"a.sh": c.src}, RefuseDynamicScope, "a.sh", "local n=3", 6, "w")
		})
	}
	// Nothing in view mentions the name: nothing a dynamic call could observe.
	planOK(t, map[string]string{"a.sh": "retry() {\n  local n=3\n  \"$@\"\n}\ng() { echo hi; }\nretry g\n"}, "a.sh", "local n=3", 6, "w")
}

func TestRenameRefusesDynamicSourceInADeclarer(t *testing.T) {
	for _, src := range []string{
		"f() {\n  local n=3\n  . \"$1\"\n}\nf lib.sh\n",
		"f() {\n  local n=3\n  source $f\n}\n",
	} {
		refusesMem(t, map[string]string{"a.sh": src, "lib.sh": "echo \"$n\"\n"}, RefuseDynamicScope, "a.sh", "local n=3", 6, "w")
	}
	// The same through a global whose name a function declares local.
	refusesMem(t, map[string]string{"a.sh": "n=0\nf() {\n  local n=3\n  . \"$1\"\n}\n"}, RefuseDynamicScope, "a.sh", "n=0", 0, "w")
}

func TestRenameSpecialVariablesBothWays(t *testing.T) {
	for _, name := range []string{"REPLY", "COMPREPLY", "SECONDS", "FUNCNEST", "BASH_XTRACEFD", "OPTARG", "BASH_REMATCH", "PIPESTATUS", "LINENO", "FUNCNAME"} {
		t.Run(name, func(t *testing.T) {
			r := refusesMem(t, map[string]string{"a.sh": name + "=1\n"}, RefuseConflict, "a.sh", name+"=1", 0, "mine")
			if !strings.Contains(r.Reason, "renaming a shell-special variable") {
				t.Fatalf("from: %q", r.Reason)
			}
			r = refusesMem(t, map[string]string{"a.sh": "mine=1\n"}, RefuseConflict, "a.sh", "mine=1", 0, name)
			if r.Reason != reasonSpecialVar {
				t.Fatalf("onto: %q", r.Reason)
			}
		})
	}
}

func TestRenameSearchSurvivors(t *testing.T) {
	// A local declared after the function's own read of the global: the search from
	// the declarer is inclusive, so the earlier read counts.
	refusesMem(t, map[string]string{"a.sh": "x=0\nf() {\n  echo \"$x\"\n  local x=1\n  echo \"$x\"\n}\nf\n"}, RefuseDynamicScope, "a.sh", "x=0", 0, "y")
	// A subshell variable searched from the function around it, which calls through "$@".
	refusesMem(t, map[string]string{"a.sh": "run() {\n  ( v=1; \"$@\" )\n}\ng() { echo \"$v\"; }\nrun g\n"}, RefuseDynamicScope, "a.sh", "v=1", 0, "w")
	// The reach bound holds for the eval and trap check.
	old := maxRenameReach
	maxRenameReach = 50
	defer func() { maxRenameReach = old }()
	chain := func(n int) string {
		var b strings.Builder
		b.WriteString("f() {\n  x=1\n  g0\n}\n")
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "g%d() { g%d; }\n", i, i+1)
		}
		fmt.Fprintf(&b, "g%d() { eval 'echo $y'; }\n", n)
		return b.String()
	}
	q := buildMem(t, map[string]string{"a.sh": chain(maxRenameReach + 20)})
	r := q.refuses(RefuseDynamicScope, "a.sh", "x=1", 0, 0, "y")
	if r.Reason != reasonReachCap {
		t.Fatalf("reason %q", r.Reason)
	}
	found := false
	for _, l := range q.s.LimitsHit() {
		found = found || strings.Contains(l, "rename reach cap")
	}
	if !found {
		t.Fatalf("LimitsHit %q", q.s.LimitsHit())
	}
	refusesMem(t, map[string]string{"a.sh": chain(maxRenameReach - 10)}, RefuseConflict, "a.sh", "x=1", 0, "y")
}

func TestRenameDynamicSourceNeedsNoLiteralTail(t *testing.T) {
	// A wholly dynamic path is refused; one with a literal final component is only a warning.
	for _, tail := range []string{`"${dir}/$name"`, `"$1"`, "$f"} {
		refusesMem(t, map[string]string{"a.sh": "f() {\n  local n=3\n  . " + tail + "\n}\n"}, RefuseDynamicScope, "a.sh", "local n=3", 6, "w")
	}
	for _, tail := range []string{`"${HOMEBREW_LIBRARY}/Homebrew/utils.sh"`, `"$d/x.sh"`, `"${d}"/x.sh`} {
		p := planOK(t, map[string]string{"a.sh": "f() {\n  local n=3\n  . " + tail + "\n}\n"}, "a.sh", "local n=3", 6, "w")
		found := false
		for _, w := range p.Warnings {
			found = found || strings.Contains(w, "dynamic source")
		}
		if !found {
			t.Fatalf("%s: warnings %q", tail, p.Warnings)
		}
	}
}

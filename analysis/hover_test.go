package analysis

import (
	"fmt"
	"strings"
	"testing"
)

// wantLines requires each want to appear in some line of md.
func wantLines(t *testing.T, md string, want ...string) {
	t.Helper()
	for _, w := range want {
		found := false
		for _, l := range strings.Split(md, "\n") {
			if strings.Contains(l, w) {
				found = true
			}
		}
		if !found {
			t.Fatalf("no line contains %q in:\n%s", w, md)
		}
	}
}

func (q *qworld) hover(file, needle string, occ, off int) (string, bool) {
	q.t.Helper()
	p, o := q.at(file, needle, occ, off)
	md, _, ok := q.s.Hover(p, o)
	return md, ok
}

func TestHoverFunction(t *testing.T) { // AC-08, C14, C14b
	q := buildCorpus(t)
	md, ok := q.hover("lib/util.sh", "build_release() {", 0, 2)
	if !ok {
		t.Fatal("no hover at definition")
	}
	wantLines(t, md, "build_release <name>", "Defined in `lib/util.sh:4`", "Called from")
	md, _ = q.hover("lib/util.sh", "log() {", 0, 1)
	wantLines(t, md, "log [args...]", "Log a message to stderr.")
	md, _ = q.hover("main.sh", `  br "$name"`, 0, 2) // hover on an alias call shows the function and says how
	wantLines(t, md, "build_release <name>", "calls `build_release` through alias `br` (resolved/alias)")
	noLine(t, md, "Reached through") // the label line says strictly more (ruling R50)
}

// noLine fails when a line of md contains bad.
func noLine(t *testing.T, md, bad string) {
	t.Helper()
	if strings.Contains(md, bad) {
		t.Fatalf("a line contains %q in:\n%s", bad, md)
	}
}

func TestHoverFunctionExactText(t *testing.T) {
	q := buildCorpus(t)
	md, ok := q.hover("lib/util.sh", "build_release() {", 0, 2)
	if !ok {
		t.Fatal()
	}
	want := "```bash\nbuild_release <name>\n```\n\n" +
		"Defined in `lib/util.sh:4`\n" +
		"Called from 5 sites: `dyn.sh:4`, `main.sh:6` (deploy), `main.sh:7` (deploy), `main.sh:9` (deploy), `main.sh:10` (deploy)\n"
	if md != want {
		t.Fatalf("got:\n%q\nwant:\n%q", md, want)
	}
	md, _ = q.hover("lib/util.sh", "log() {", 0, 1)
	if !strings.HasPrefix(md, "```bash\nlog [args...]\n```\nLog a message to stderr.\n\nDefined in `lib/util.sh:3`\nCalled from") {
		t.Fatalf("%q", md)
	}
}

func TestHoverRange(t *testing.T) {
	q := buildCorpus(t)
	p, o := q.at("lib/util.sh", "log() {", 0, 1)
	_, r, ok := q.s.Hover(p, o)
	if !ok || r.Start.Line != 3 || r.Start.Col != 1 || r.End.Line != 3 || r.End.Col != 34 {
		t.Fatalf("%+v", r)
	}
	p, o = q.at("main.sh", `  br "$name"`, 0, 2)
	_, r, _ = q.s.Hover(p, o)
	if r.Start.Line != 7 || r.End.Col-r.Start.Col != 2 {
		t.Fatalf("use range %+v", r)
	}
}

func TestHoverVariable(t *testing.T) {
	q := buildCorpus(t)
	// On the "$cmd" call word the call wins (it resolves to build_release);
	// the variable's hover is on its declaration.
	md, ok := q.hover("main.sh", `"$cmd" x`, 0, 2)
	if !ok {
		t.Fatal()
	}
	wantLines(t, md, "build_release <name>", "calls `build_release` through variable `cmd` (candidate/variable-constant)")
	noLine(t, md, "Reached through")
	md, ok = q.hover("main.sh", `local cmd=build_release`, 0, 6)
	if !ok {
		t.Fatal()
	}
	wantLines(t, md, "local cmd=build_release", "Local to `deploy` in `main.sh`", "Value: `build_release`")
	want := "```bash\nlocal cmd=build_release\n```\nLocal to `deploy` in `main.sh`\nValue: `build_release`\n"
	if md != want {
		t.Fatalf("%q", md)
	}
}

func TestHoverNothing(t *testing.T) {
	q := buildCorpus(t)
	p, _ := q.at("main.sh", "source ./lib/util.sh", 0, 0)
	if _, _, ok := q.s.Hover(p, 0); ok {
		t.Fatal("hover on a shebang")
	}
	if _, _, ok := q.s.Hover(q.abs("nope.sh"), 0); ok {
		t.Fatal("unknown path")
	}
}

func TestHoverSignatures(t *testing.T) {
	q := buildMem(t, map[string]string{"a.sh": `# Doc line one
# Doc line two
f() {
  local src="${1:-dflt}"
  dst=$2
  echo "$3" "$@"
  exit 1
}
g() { :; }
h() { local a="$1"; local b="${1#x}"; echo "$a $b"; }
f
f
gg() { local x="$1" y=$3; echo "$x" "$2" ; }
`})
	md, ok := q.hover("a.sh", "f() {", 0, 0)
	if !ok {
		t.Fatal()
	}
	wantLines(t, md, "f <src> <dst> <$3> [args...]", "Doc line one", "Doc line two", "Calls `exit`", "Writes globals: `dst`")
	if strings.Contains(md, "Reads globals") {
		t.Fatal(md)
	}
	wantLines(t, md, "Called from 2 sites: `a.sh:11`, `a.sh:12`")
	md, _ = q.hover("a.sh", "g() {", 0, 0)
	if !strings.HasPrefix(md, "```bash\ng\n```\n") || strings.Contains(md, "Called from") {
		t.Fatalf("%q", md)
	}
	md, _ = q.hover("a.sh", "h() {", 0, 0)
	if !strings.HasPrefix(md, "```bash\nh <a>\n```\n") { // `${1#x}` is not a plain default
		t.Fatalf("%q", md)
	}
	md, _ = q.hover("a.sh", "gg() {", 0, 0)
	if !strings.HasPrefix(md, "```bash\ngg <x> <$2> <y>\n```\n") {
		t.Fatalf("%q", md)
	}
}

func TestHoverGlobalsAndManyCallers(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("f() {\n")
	for _, n := range []string{"R9", "R1", "R2", "R3", "R4", "R5", "R6", "R7", "R8"} {
		sb.WriteString("  echo \"$" + n + "\"\n")
	}
	sb.WriteString("  W=1\n  local L=1\n  echo $L\n}\n")
	for i := 0; i < 7; i++ {
		sb.WriteString("f\n")
	}
	q := buildMem(t, map[string]string{"a.sh": sb.String()})
	md, _ := q.hover("a.sh", "f() {", 0, 0)
	wantLines(t, md, "Reads globals: `R1`, `R2`, `R3`, `R4`, `R5`, `R6`, `R7`, `R8`", "Writes globals: `W`")
	if strings.Contains(md, "R9") || strings.Contains(md, "`L`") {
		t.Fatal(md)
	}
	wantLines(t, md, "Called from 7 sites: `a.sh:15`, `a.sh:16`, `a.sh:17`, `a.sh:18`, `a.sh:19`, and 2 more")
}

func TestHoverSeveralDefinitions(t *testing.T) {
	q := buildMem(t, map[string]string{
		"main.sh": "source ./lib.sh\nf() { :; }\nf\n",
		"lib.sh":  "f() { :; }\n",
	})
	md, ok := q.hover("main.sh", "f\n", 0, 0)
	if !ok {
		t.Fatal()
	}
	wantLines(t, md, "2 definitions", "Defined in `lib.sh:1`, `main.sh:2`")
	if !strings.HasPrefix(md, "```bash\nf\n```\n2 definitions\n") {
		t.Fatalf("%q", md)
	}
}

func TestHoverVariableForms(t *testing.T) {
	q := buildMem(t, map[string]string{
		"main.sh": "source ./conf.sh\necho \"$CFG\" \"$HOME\" \"$MULTI\"\n",
		"conf.sh": "declare -rx CFG=1\nMULTI=a\nMULTI=b\n",
	})
	md, ok := q.hover("main.sh", "$CFG", 0, 2)
	if !ok {
		t.Fatal()
	}
	wantLines(t, md, "declare -rx CFG=1", "Global, declared in `conf.sh:1`", "Attributes: exported, readonly", "Value: `1`")
	md, _ = q.hover("main.sh", "$HOME", 0, 2)
	if !strings.Contains(md, "Not assigned in this workspace; read from the environment.") || strings.Contains(md, "Value:") {
		t.Fatalf("%q", md)
	}
	md, _ = q.hover("main.sh", "$MULTI", 0, 2)
	if strings.Contains(md, "Value:") || !strings.Contains(md, "Global, declared in `conf.sh:2`, `conf.sh:3`") {
		t.Fatalf("%q", md)
	}
}

func TestHoverLongDeclarationCut(t *testing.T) {
	long := strings.Repeat("x", 200)
	q := buildMem(t, map[string]string{"a.sh": "V=" + long + "\necho $V\n"})
	md, _ := q.hover("a.sh", "$V", 0, 1)
	first := strings.Split(md, "\n")[1]
	if len([]rune(first)) != 120 {
		t.Fatalf("%d: %q", len(first), first)
	}
}

// A read bound only through dynamic scope is labelled as such (FDS-006) and
// lists every candidate caller local, up to maxCallSites with a count of the
// rest; it is not presented as the caller's local for certain.
func TestHoverDynamicScopeRead(t *testing.T) {
	q := buildMem(t, map[string]string{
		"m.sh": "outer() { local v=1; inner; }\n" +
			"other() { local v=2; inner; }\n" +
			"inner() { echo \"$v\"; }\n",
	})
	md, ok := q.hover("m.sh", "$v", 0, 1)
	if !ok {
		t.Fatal("no hover")
	}
	wantLines(t, md, "candidate/dynamic-scope", "`local v=1` in `outer` at `m.sh:1`", "`local v=2` in `other` at `m.sh:2`")
	noLine(t, md, "Local to `outer`")
	noLine(t, md, "Value: `1`")

	var b strings.Builder
	for i := 0; i < maxCallSites+3; i++ {
		b.WriteString(fmt.Sprintf("c%d() { local w=%d; g; }\n", i, i))
	}
	b.WriteString("g() { echo \"$w\"; }\n")
	q = buildMem(t, map[string]string{"n.sh": b.String()})
	md, _ = q.hover("n.sh", "$w", 0, 1)
	if got := strings.Count(md, "`local w="); got != maxCallSites {
		t.Fatalf("%d caller locals shown, want %d:\n%s", got, maxCallSites, md)
	}
	wantLines(t, md, "8 candidate caller locals", "and 3 more")
}

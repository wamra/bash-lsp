package analysis

import (
	"os"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/internal/front"
)

func bindSrc(t *testing.T, src string) *FileFacts { return bindNamed(t, "t.sh", src) }

func bindNamed(t *testing.T, path, src string) *FileFacts {
	t.Helper()
	b := []byte(src)
	return Bind(front.Parse(path, b, front.Detect(path, b)))
}

func bindFile(t *testing.T, path string) *FileFacts {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return Bind(front.Parse(path, b, front.Detect(path, b)))
}

func eq(got []string, want ...string) bool {
	return strings.Join(got, "\x00") == strings.Join(want, "\x00")
}

func declNames(ff *FileFacts, k DeclKind) []string {
	var out []string
	for _, d := range ff.Decls {
		if d.Kind == k {
			out = append(out, d.Name)
		}
	}
	return out
}

func decl(ff *FileFacts, k DeclKind, name string) *Decl {
	for _, d := range ff.Decls {
		if d.Kind == k && d.Name == name {
			return d
		}
	}
	return nil
}

func declIn(ff *FileFacts, name, fn string) *Decl {
	for _, d := range ff.Decls {
		if d.Kind == DeclVariable && d.Name == name && d.Scope.Func == fn {
			return d
		}
	}
	return nil
}

func useAt(ff *FileFacts, line int, name string) *Use {
	for _, u := range ff.Uses {
		if u.Name == name && u.Range.Start.Line == line {
			return u
		}
	}
	return nil
}

func lineOf(t *testing.T, ff *FileFacts, sub string) int {
	t.Helper()
	i := strings.Index(string(ff.File.Src), sub)
	if i < 0 {
		t.Fatalf("%q not in source", sub)
	}
	return ff.File.PosAt(i).Line
}

func TestBindMain(t *testing.T) {
	ff := bindFile(t, "../testdata/corpus/main.sh")
	if got := declNames(ff, DeclFunction); !eq(got, "deploy", "other", "unquoted") {
		t.Fatal(got)
	}
	a := decl(ff, DeclAlias, "br")
	if a == nil || a.AliasTo != "build_release" {
		t.Fatalf("%+v", a)
	}
	if u := useAt(ff, lineOf(t, ff, "alias br="), "build_release"); u == nil || !u.ViaAlias {
		t.Fatal("alias target use missing")
	}
	var localsInDeploy []string
	for _, d := range ff.Decls {
		if d.Kind == DeclVariable && d.Attrs.Local && d.Scope.Func == "deploy" {
			localsInDeploy = append(localsInDeploy, d.Name)
		}
	}
	if !eq(localsInDeploy, "name", "cmd") {
		t.Fatal(localsInDeploy)
	}
	if d := declIn(ff, "cmd", "deploy"); d.Literal != "build_release" {
		t.Fatalf("%q", d.Literal)
	}
	reads := 0
	for _, u := range ff.Uses {
		if u.Kind == UseVarRead && u.Name == "name" && u.Scope.Func == "deploy" {
			reads++
		}
	}
	if reads != 2 {
		t.Fatal(reads)
	}
	if len(ff.Sources) != 1 {
		t.Fatal(len(ff.Sources))
	}
	for _, u := range ff.Uses {
		if u.Kind == UseCall && (u.Name == "source" || u.Name == "alias") {
			t.Fatalf("builtin %s recorded as call use", u.Name)
		}
	}
}

func TestDocComment(t *testing.T) {
	ff := bindFile(t, "../testdata/corpus/lib/util.sh")
	if d := decl(ff, DeclFunction, "log"); d.Doc != "Log a message to stderr." {
		t.Fatalf("%q", d.Doc)
	}
	if d := decl(ff, DeclFunction, "build_release"); d.Doc != "" {
		t.Fatalf("%q", d.Doc) // a non-comment line separates it
	}
}

func TestDocCommentMultiLineAndShebang(t *testing.T) {
	ff := bindSrc(t, "#!/bin/bash\n# first\n# second\nf() { :; }\n\n# detached\n\ng() { :; }\nx=1 # trailing\nh() { :; }\n")
	if d := decl(ff, DeclFunction, "f"); d.Doc != "first\nsecond" {
		t.Errorf("f: %q", d.Doc)
	}
	if d := decl(ff, DeclFunction, "g"); d.Doc != "" {
		t.Errorf("g: %q", d.Doc)
	}
	if d := decl(ff, DeclFunction, "h"); d.Doc != "" {
		t.Errorf("h: %q (trailing comment is not a doc line)", d.Doc)
	}
	ff = bindSrc(t, "#!/bin/bash\nf() { :; }\n")
	if d := decl(ff, DeclFunction, "f"); d.Doc != "" {
		t.Errorf("shebang leaked into doc: %q", d.Doc)
	}
}

func TestImplicitDecls(t *testing.T) {
	ff := bindSrc(t, "read -r a b\nmapfile -t lines < f\ngetopts ab: opt\nprintf -v out '%s' x\nfor i in 1 2; do :; done\ndeclare -A m\nf() { declare -g G=1; local -n ref=a; }\n")
	for name, impl := range map[string]string{"a": "read", "b": "read", "lines": "mapfile", "opt": "getopts", "out": "printf -v", "i": "for"} {
		if d := decl(ff, DeclVariable, name); d == nil || d.Implicit != impl {
			t.Errorf("%s: %+v", name, d)
		}
	}
	if !decl(ff, DeclVariable, "m").Attrs.Assoc {
		t.Error("m not assoc")
	}
	if g := decl(ff, DeclVariable, "G"); !g.Attrs.Global || g.Attrs.Local {
		t.Errorf("%+v", g.Attrs)
	}
	if r := decl(ff, DeclVariable, "ref"); !r.Attrs.Nameref || !r.Attrs.Local {
		t.Errorf("%+v", r.Attrs)
	}
}

func TestImplicitDeclVariants(t *testing.T) {
	ff := bindSrc(t, "read -p 'x? ' -a arr\nread -rn1 ch\nselect s in a b; do :; done\nreadarray -d '' rl\nfor w; do :; done\n")
	for name, impl := range map[string]string{"arr": "read", "ch": "read", "s": "select", "rl": "readarray", "w": "for"} {
		if d := decl(ff, DeclVariable, name); d == nil || d.Implicit != impl {
			t.Errorf("%s: %+v", name, d)
		}
	}
	if decl(ff, DeclVariable, "x? ") != nil || decl(ff, DeclVariable, "") != nil {
		t.Error("option argument taken as a name")
	}
}

func TestAttrFlags(t *testing.T) {
	ff := bindSrc(t, "export -n E=1\nexport X0=1\nreadonly R=2\ndeclare -xi N=3\ndeclare -a A=(1 2)\nf() { declare L=1; typeset -g T=1; local -r K=1; }\n")
	if d := decl(ff, DeclVariable, "R"); !d.Attrs.Readonly {
		t.Errorf("R %+v", d.Attrs)
	}
	if d := decl(ff, DeclVariable, "E"); d.Attrs.Exported || d.Attrs.Nameref {
		t.Errorf("E (export -n) %+v", d.Attrs)
	}
	if d := decl(ff, DeclVariable, "X0"); !d.Attrs.Exported || d.Attrs.Nameref {
		t.Errorf("X0 %+v", d.Attrs)
	}
	if d := decl(ff, DeclVariable, "N"); !d.Attrs.Exported || !d.Attrs.Integer || d.Attrs.Local {
		t.Errorf("N %+v", d.Attrs)
	}
	if d := decl(ff, DeclVariable, "A"); !d.Attrs.Array {
		t.Errorf("A %+v", d.Attrs)
	}
	if d := decl(ff, DeclVariable, "L"); !d.Attrs.Local {
		t.Errorf("L %+v", d.Attrs)
	}
	if d := decl(ff, DeclVariable, "T"); d.Attrs.Local || !d.Attrs.Global {
		t.Errorf("T %+v", d.Attrs)
	}
	if d := decl(ff, DeclVariable, "K"); !d.Attrs.Local || !d.Attrs.Readonly {
		t.Errorf("K %+v", d.Attrs)
	}
	for _, name := range []string{"E", "R", "N", "L", "K"} {
		found := false
		for _, u := range ff.Uses {
			if u.Kind == UseVarWrite && u.Name == name {
				found = true
			}
		}
		if !found {
			t.Errorf("no UseVarWrite for %s", name)
		}
	}
}

func TestScopes(t *testing.T) {
	ff := bindSrc(t, "x=1\n( y=2 )\nz=$(w=3; echo)\na | b=4 c\n")
	want := map[string]ScopeKind{"x": ScopeFile, "y": ScopeSubshell, "w": ScopeCmdSubst, "z": ScopeFile, "b": ScopePipeline}
	for n, k := range want {
		if d := decl(ff, DeclVariable, n); d == nil || d.Scope.Kind != k {
			t.Errorf("%s: %+v", n, d)
		}
	}
	if d := decl(ff, DeclVariable, "y"); d.Scope.Parent != ff.Root {
		t.Error("subshell scope parent is not the root")
	}
}

func TestFunctionNesting(t *testing.T) {
	ff := bindSrc(t, "outer() {\n  inner() { :; }\n  ( sub() { :; } )\n  v=1\n}\n")
	for _, n := range []string{"outer", "inner", "sub"} {
		if d := decl(ff, DeclFunction, n); d == nil || d.Scope != ff.Root {
			t.Errorf("%s: %+v", n, d)
		}
	}
	if d := decl(ff, DeclVariable, "v"); d.Scope.Kind != ScopeFunction || d.Scope.Func != "outer" {
		t.Errorf("%+v", d.Scope)
	}
}

func TestUses(t *testing.T) {
	ff := bindSrc(t, "f() { :; }\nf a\n\"$cmd\" x\n$cmd y\necho \"$1 $@ ${#} ${v:-d} ${arr[0]}\"\nX=1 g\n")
	if useAt(ff, 2, "f") == nil || useAt(ff, 2, "f").Kind != UseCall {
		t.Error("call of f missing")
	}
	for _, u := range ff.Uses { // "$cmd" and $cmd are variable call sites, resolved by Build
		if u.Kind == UseCall && u.Range.Start.Line >= 3 && u.Range.Start.Line <= 4 && (u.Via != ViaVariable || u.Name != "cmd") {
			t.Errorf("dynamic command produced call use %+v", u)
		}
	}
	reads := map[string]bool{}
	for _, u := range ff.Uses {
		if u.Kind == UseVarRead {
			reads[u.Name] = true
		}
	}
	for _, n := range []string{"cmd", "v", "arr"} {
		if !reads[n] {
			t.Errorf("no read of %s", n)
		}
	}
	for _, n := range []string{"1", "@", "#"} {
		if reads[n] {
			t.Errorf("special parameter %s recorded", n)
		}
	}
	if u := useAt(ff, 6, "g"); u == nil || u.Kind != UseCall {
		t.Error("call g after env assignment missing")
	}
	if decl(ff, DeclVariable, "X") == nil {
		t.Error("env assignment not declared")
	}
}

func TestLiteral(t *testing.T) {
	ff := bindSrc(t, "a=plain\nb=\"quoted lit\"\nc='sq'\nd=\"$x\"\ne=$(cmd)\nf=\n")
	want := map[string]string{"a": "plain", "b": "quoted lit", "c": "sq", "d": "", "e": "", "f": ""}
	for n, w := range want {
		if d := decl(ff, DeclVariable, n); d == nil || d.Literal != w {
			t.Errorf("%s: %+v", n, d)
		}
	}
}

func TestSources(t *testing.T) {
	ff := bindSrc(t, "# shellcheck source=lib/a.sh\n. \"$DIR/a.sh\"\nsource b.sh\nload c\n")
	if len(ff.Sources) != 2 {
		t.Fatalf("%d sources", len(ff.Sources))
	}
	if ff.Sources[0].Directive != "lib/a.sh" || ff.Sources[1].Directive != "" {
		t.Errorf("%+v %+v", ff.Sources[0], ff.Sources[1])
	}
	if ff.Sources[1].Word == nil || ff.Sources[1].Range.Start.Line != 3 {
		t.Errorf("%+v", ff.Sources[1])
	}
	for _, u := range ff.Uses {
		if u.Kind == UseCall && (u.Name == "." || u.Name == "source") {
			t.Errorf("source recorded as call: %+v", u)
		}
	}
	if useAt(ff, 4, "load") == nil {
		t.Error("load is an ordinary call outside bats")
	}
	bf := bindNamed(t, "t.bats", "load helper\n@test \"adds\" {\n  run add 1 2\n}\n")
	if len(bf.Sources) != 1 {
		t.Fatalf("bats sources: %d", len(bf.Sources))
	}
	if useAt(bf, 1, "load") != nil {
		t.Error("bats load recorded as call")
	}
	if d := decl(bf, DeclFunction, "adds"); d == nil {
		t.Error("bats test not declared")
	}
}

func TestAliasQuoted(t *testing.T) {
	ff := bindSrc(t, "alias ll='ls -l' gs=\"git status\"\nalias q\n")
	if d := decl(ff, DeclAlias, "ll"); d == nil || d.AliasTo != "ls" {
		t.Errorf("%+v", d)
	}
	if d := decl(ff, DeclAlias, "gs"); d == nil || d.AliasTo != "git" {
		t.Errorf("%+v", d)
	}
	u := useAt(ff, 1, "ls")
	if u == nil || !u.ViaAlias || string(ff.File.Src[u.Range.Start.Offset:u.Range.End.Offset]) != "ls" {
		t.Errorf("%+v", u)
	}
	if decl(ff, DeclAlias, "q") != nil {
		t.Error("alias query declared")
	}
}

func TestBrokenFile(t *testing.T) {
	ff := bindFile(t, "../testdata/corpus/broken_fi.sh")
	if ff.File == nil || ff.Root == nil {
		t.Fatal("no facts")
	}
}

func TestTempEnvAssignment(t *testing.T) {
	ff := bindSrc(t, "X=1 cmd\nY=2\n")
	if d := decl(ff, DeclVariable, "X"); d == nil || !d.Temp {
		t.Errorf("X: %+v", d)
	}
	if d := decl(ff, DeclVariable, "Y"); d == nil || d.Temp {
		t.Errorf("Y: %+v", d)
	}
	found := false
	for _, u := range ff.Uses {
		if u.Kind == UseVarWrite && u.Name == "X" {
			found = true
		}
	}
	if !found {
		t.Error("UseVarWrite for X dropped")
	}
}

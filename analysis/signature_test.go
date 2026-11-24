package analysis

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// sigAt builds src (the cursor is the ▮ mark, removed) as one file and asks
// for the signature help there.
func sigAt(t *testing.T, src string) (FuncSignature, int, bool) {
	t.Helper()
	off := strings.Index(src, "▮")
	if off < 0 {
		t.Fatal("no cursor mark")
	}
	q := buildMem(t, map[string]string{"a.sh": strings.Replace(src, "▮", "", 1)})
	return q.s.SignatureAt(filepath.Join(q.root, "a.sh"), off)
}

func TestSignatureNamedParams(t *testing.T) {
	sig, active, ok := sigAt(t, "# Copy a file.\ncp2() { local src=\"$1\" dst=\"$2\"; }\ncp2 a ▮")
	if !ok {
		t.Fatal("no signature")
	}
	if sig.Label != "cp2 src dst" || active != 1 || sig.Doc != "Copy a file." || sig.Variadic {
		t.Fatalf("got %+v active %d", sig, active)
	}
	want := []Param{{1, "src"}, {2, "dst"}}
	if !reflect.DeepEqual(sig.Params, want) {
		t.Fatalf("params %+v, want %+v", sig.Params, want)
	}
}

func TestSignatureActiveIndex(t *testing.T) {
	for _, c := range []struct {
		src    string
		active int
	}{
		{"f() { echo \"$1\" \"$2\"; }\nf ▮", 0},
		{"f() { echo \"$1\" \"$2\"; }\nf a▮", 0},
		{"f() { echo \"$1\" \"$2\"; }\nf a ▮", 1},
		{"f() { echo \"$1\" \"$2\"; }\nf a ▮ c", 1},
		{"f() { echo \"$1\" \"$2\"; }\nf   a   b   ▮", 1}, // past the last parameter: clamped
	} {
		sig, active, ok := sigAt(t, c.src)
		if !ok || active != c.active || sig.Label != "f $1 $2" {
			t.Errorf("%q: %+v active %d ok %v, want active %d", c.src, sig, active, ok, c.active)
		}
	}
}

func TestSignatureVariadicClamped(t *testing.T) {
	sig, active, ok := sigAt(t, "f() { echo \"$1\" \"$@\"; }; f a b c ▮")
	if !ok || !sig.Variadic || sig.Label != "f $1 [args...]" {
		t.Fatalf("got %+v ok %v", sig, ok)
	}
	if active != 1 { // the [args...] position
		t.Fatalf("active %d, want 1", active)
	}
	sig, active, ok = sigAt(t, "g() { echo \"$@\"; }\ng ▮")
	if !ok || sig.Label != "g [args...]" || active != 0 || len(sig.Params) != 0 {
		t.Fatalf("got %+v active %d ok %v", sig, active, ok)
	}
}

func TestSignatureGetopts(t *testing.T) {
	sig, _, ok := sigAt(t, "opts() { while getopts \"ab:\" o; do :; done; }\nopts ▮")
	if !ok || sig.Getopts != "-a -b ARG" {
		t.Fatalf("got %+v ok %v", sig, ok)
	}
	sig, _, _ = sigAt(t, "o2() { while getopts ':xy:z' o; do :; done; }\no2 ▮")
	if sig.Getopts != "-x -y ARG -z" {
		t.Fatalf("silent form: %q", sig.Getopts)
	}
	sig, _, _ = sigAt(t, "o3() { while getopts \"$spec\" o; do :; done; }\no3 ▮")
	if sig.Getopts != "" {
		t.Fatalf("non-literal string: %q", sig.Getopts)
	}
	sig, _, _ = sigAt(t, "o4() { inner() { getopts \"q\" o; }; }\no4 ▮")
	if sig.Getopts != "" {
		t.Fatalf("nested function's getopts counted: %q", sig.Getopts)
	}
}

func TestSignatureNone(t *testing.T) {
	for name, src := range map[string]string{
		"on the name":       "f() { :; }\nf▮ a",
		"inside the name":   "foo() { :; }\nfo▮o a",
		"before the name":   "f() { :; }\n▮f a",
		"builtin":           "echo ▮",
		"unbound command":   "nosuch ▮",
		"after a newline":   "f() { :; }\nf a\n▮",
		"after a semicolon": "f() { :; }\nf a; ▮",
		"inner builtin":     "f() { :; }\nf $(echo ▮)",
		"definition":        "f() { :; }▮",
	} {
		if sig, _, ok := sigAt(t, src); ok {
			t.Errorf("%s: got %+v", name, sig)
		}
	}
}

func TestSignatureInnermostCall(t *testing.T) {
	sig, active, ok := sigAt(t, "f() { echo \"$1\"; }\ng() { echo \"$1\" \"$2\"; }\nf $(g x ▮)")
	if !ok || sig.Name != "g" || active != 1 {
		t.Fatalf("got %+v active %d ok %v", sig, active, ok)
	}
}

func TestSignatureCorpusDeploy(t *testing.T) {
	q := buildCorpus(t)
	p, o := q.at("main.sh", "deploy app", 0, len("deploy app"))
	sig, active, ok := q.s.SignatureAt(p, o)
	if !ok || sig.Label != "deploy name" || active != 0 {
		t.Fatalf("got %+v active %d ok %v", sig, active, ok)
	}
}

func TestSignatureOfSymbol(t *testing.T) {
	q := buildCorpus(t)
	p, o := q.at("lib/util.sh", "build_release() {", 0, 2)
	sym, ok := q.s.SymbolAt(p, o)
	if !ok {
		t.Fatal("no symbol")
	}
	sig, ok := q.s.Signature(sym)
	if !ok || sig.Label != "build_release name" {
		t.Fatalf("got %+v", sig)
	}
	p, o = q.at("main.sh", "alias br=", 0, 6)
	if sym, ok := q.s.SymbolAt(p, o); ok {
		if _, ok := q.s.Signature(sym); ok {
			t.Fatal("an alias has no signature")
		}
	}
	if _, ok := q.s.Signature(nil); ok {
		t.Fatal("nil symbol has a signature")
	}
}

func TestSignatureOverflowClamped(t *testing.T) {
	sig, active, ok := sigAt(t, "f() { echo \"$1\" \"$2\"; }\nf a b c ▮")
	if !ok || sig.Variadic || active != 1 {
		t.Fatalf("got %+v active %d ok %v", sig, active, ok)
	}
	if _, active, ok := sigAt(t, "z() { :; }\nz a b ▮"); !ok || active != 2 { // no parameters to clamp to: left as counted
		t.Fatalf("no parameters: active %d ok %v", active, ok)
	}
}

func TestSignatureThroughAliasIsNone(t *testing.T) {
	for name, src := range map[string]string{
		"preset arguments": "shopt -s expand_aliases\nf() { echo \"$1\" \"$2\" \"$3\"; }\nalias h='f x'\nh a ▮",
		"plain alias":      "shopt -s expand_aliases\nf() { echo \"$1\"; }\nalias h=f\nh ▮",
		"alias same name":  "shopt -s expand_aliases\nf() { echo \"$1\"; }\nalias f='f x'\nf ▮",
	} {
		if sig, _, ok := sigAt(t, src); ok {
			t.Errorf("%s: got %+v", name, sig)
		}
	}
	q := buildCorpus(t)
	p, o := q.at("main.sh", `  br "$name"`, 0, len(`  br `))
	if sig, _, ok := q.s.SignatureAt(p, o); ok {
		t.Fatalf("corpus alias call: %+v", sig)
	}
}

func TestSignatureHugeParameterIndexBounded(t *testing.T) {
	src := "f() { echo \"${99999999}\" \"${2147483647}\" \"$1\"; }\nf ▮"
	sig, _, ok := sigAt(t, src)
	if !ok || len(sig.Params) != maxParams || !sig.Variadic || len(sig.Label) > 64*10+64 {
		t.Fatalf("params %d variadic %v label %d bytes ok %v", len(sig.Params), sig.Variadic, len(sig.Label), ok)
	}
	q := buildMem(t, map[string]string{"a.sh": "f() { echo \"${99999999}\"; }\nf x\n"})
	p := filepath.Join(q.root, "a.sh")
	md, _, ok := q.s.Hover(p, strings.Index("f() { echo \"${99999999}\"; }\nf x\n", "f()")+1)
	if !ok || len(md) > 2000 {
		t.Fatalf("hover %d bytes ok %v", len(md), ok)
	}
}

func TestSignatureCallGuards(t *testing.T) {
	// the cursor in an argument that is itself a call in a nested
	// expansion of a non-function: no help from the outer call
	if sig, _, ok := sigAt(t, "f() { :; }\nf `echo ▮`"); ok {
		t.Errorf("backtick: %+v", sig)
	}
	// a function whose name is also used after an env prefix
	sig, active, ok := sigAt(t, "f() { echo \"$1\" \"$2\"; }\nX=1 f a ▮")
	if !ok || sig.Name != "f" || active != 1 {
		t.Errorf("env prefix: %+v active %d ok %v", sig, active, ok)
	}
}

func TestSignatureGetoptsCap(t *testing.T) {
	spec := strings.Repeat("a", 3) + strings.Repeat("b", maxGetoptsShown)
	got := optionList(spec)
	if n := len(strings.Fields(got)); n != maxGetoptsShown+1 || !strings.HasSuffix(got, " ...") {
		t.Fatalf("%d fields, %q", n, got)
	}
	if got := optionList(strings.Repeat("c", maxGetoptsShown)); strings.Contains(got, "...") {
		t.Fatalf("at the cap exactly: %q", got)
	}
}

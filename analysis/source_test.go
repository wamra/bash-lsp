package analysis

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/internal/front"
)

type world struct {
	root  string
	facts map[string]*FileFacts // by slash-separated path relative to root
	files map[string]bool       // absolute paths
}

func (w *world) exists(p string) bool { return w.files[p] }

func (w *world) glob(pat string) []string {
	var out []string
	for p := range w.files {
		if ok, _ := filepath.Match(pat, p); ok {
			out = append(out, p)
		}
	}
	return out
}

func loadDir(t *testing.T, dir string) *world {
	t.Helper()
	root, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	w := &world{root: root, facts: map[string]*FileFacts{}, files: map[string]bool{}}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		w.files[p] = true
		w.facts[filepath.ToSlash(rel)] = Bind(front.Parse(p, b, front.Detect(p, b)))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// memWorld binds one in-memory file at root/main.sh next to the given extra files (names relative to root).
func memWorld(t *testing.T, src string, extra ...string) (*world, *FileFacts) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "ws")
	w := &world{root: root, facts: map[string]*FileFacts{}, files: map[string]bool{}}
	for _, e := range extra {
		w.files[filepath.Join(root, filepath.FromSlash(e))] = true
	}
	p := filepath.Join(root, "main.sh")
	w.files[p] = true
	b := []byte(src)
	return w, Bind(front.Parse(p, b, front.Detect(p, b)))
}

func eqRel(root string, got []string, want []string) bool {
	rel := make([]string, len(got))
	for i, g := range got {
		r, err := filepath.Rel(root, g)
		if err != nil {
			return false
		}
		rel[i] = filepath.ToSlash(r)
	}
	return strings.Join(rel, "\x00") == strings.Join(want, "\x00")
}

func TestResolveCorpus(t *testing.T) {
	w := loadDir(t, "../testdata/corpus")
	check := func(file, how string, want ...string) {
		t.Helper()
		es := ResolveSources(w.facts[file], w.root, nil, w.exists, w.glob)
		if len(es) != 1 || es[0].How != how || es[0].Status != SourceResolved || !eqRel(w.root, es[0].Targets, want) {
			t.Fatalf("%s: %+v", file, es)
		}
	}
	check("main.sh", "literal", "lib/util.sh")
	check("dyn.sh", "script-dir", "lib/util.sh")
	es := ResolveSources(w.facts["app.sh"], w.root, nil, w.exists, w.glob)
	if len(es) != 2 || es[0].How != "literal" || es[1].How != "loop-glob" || !eqRel(w.root, es[1].Targets, []string{"plugins/a.sh"}) {
		t.Fatalf("%+v", es)
	}
}

func TestScriptDirForms(t *testing.T) {
	for _, src := range []string{
		`. "$(dirname "$0")/lib.sh"`,
		`. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"`,
		`. "$(dirname "$BASH_SOURCE")/lib.sh"`,
		`. "${BASH_SOURCE[0]%/*}/lib.sh"`,
		`. "${BASH_SOURCE%/*}/lib.sh"`,
		`. "${0%/*}/lib.sh"`,
		`. $(dirname $0)/lib.sh`,
		`. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"`,
		"D=\"$(cd \"$(dirname \"$0\")\" && pwd)\"\n. \"$D/lib.sh\"",
		"D=\"$(dirname \"$0\")\"\n. \"${D}/lib.sh\"",
		"D=${BASH_SOURCE%/*}\nsource $D/lib.sh",
	} {
		w, ff := memWorld(t, src, "lib.sh")
		es := ResolveSources(ff, w.root, nil, w.exists, w.glob)
		if len(es) != 1 || es[0].How != "script-dir" || es[0].Status != SourceResolved || !eqRel(w.root, es[0].Targets, []string{"lib.sh"}) {
			t.Errorf("%q: %+v", src, es)
		}
	}
}

func TestScriptDirUsesFileDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ws")
	p := filepath.Join(root, "sub", "run.sh")
	src := []byte(`. "$(dirname "$0")/lib.sh"`)
	ff := Bind(front.Parse(p, src, front.Detect(p, src)))
	want := filepath.Join(root, "sub", "lib.sh")
	es := ResolveSources(ff, root, nil, func(s string) bool { return s == want }, func(string) []string { return nil })
	if len(es) != 1 || es[0].How != "script-dir" || !eq(es[0].Targets, want) {
		t.Fatalf("%+v", es)
	}
}

func TestScriptDirNotMatched(t *testing.T) {
	for _, src := range []string{
		`. "$(dirname "$1")/lib.sh"`,
		`. "$(pwd)/lib.sh"`,
		"D=\"$(dirname \"$1\")\"\n. \"$D/lib.sh\"",
		"D=\"$(dirname \"$0\")\"\nD=x\n. \"$D/lib.sh\"",
	} {
		w, ff := memWorld(t, src, "lib.sh")
		es := ResolveSources(ff, w.root, nil, w.exists, w.glob)
		if len(es) != 1 || es[0].Status != SourceUnresolved || len(es[0].Targets) != 0 {
			t.Errorf("%q: %+v", src, es)
		}
	}
}

func TestDirectiveAndUnresolved(t *testing.T) {
	cases := []struct {
		src    string
		status SourceStatus
		how    string
		want   []string
	}{
		{"# shellcheck source=lib.sh\n. \"$X\"", SourceResolved, "directive", []string{"lib.sh"}},
		{"# shellcheck source=/dev/null\n. \"$X\"", SourceIgnored, "directive", nil},
		{". \"$X\"", SourceUnresolved, "", nil},
		{"X=a\nX=b\n. \"$X/lib.sh\"", SourceUnresolved, "", nil},
		{". missing.sh", SourceUnresolved, "", nil},
	}
	for _, c := range cases {
		w, ff := memWorld(t, c.src, "lib.sh")
		es := ResolveSources(ff, w.root, nil, w.exists, w.glob)
		if len(es) != 1 || es[0].Status != c.status || es[0].How != c.how || !eqRel(w.root, es[0].Targets, c.want) {
			t.Errorf("%q: %+v", c.src, es)
		}
	}
}

func TestDirectiveSearchOrder(t *testing.T) {
	// file dir first, then searchPaths, then root
	w, ff := memWorld(t, "# shellcheck source=x.sh\n. \"$X\"", "x.sh", "inc/x.sh")
	es := ResolveSources(ff, w.root, []string{filepath.Join(w.root, "inc")}, w.exists, w.glob)
	if len(es) != 1 || !eqRel(w.root, es[0].Targets, []string{"x.sh"}) {
		t.Fatalf("%+v", es)
	}
	w, ff = memWorld(t, "# shellcheck source=x.sh\n. \"$X\"", "inc/x.sh")
	es = ResolveSources(ff, w.root, []string{filepath.Join(w.root, "inc")}, w.exists, w.glob)
	if len(es) != 1 || es[0].How != "directive" || !eqRel(w.root, es[0].Targets, []string{"inc/x.sh"}) {
		t.Fatalf("%+v", es)
	}
}

func TestSearchPaths(t *testing.T) {
	w, ff := memWorld(t, ". util.sh", "lib/util.sh")
	es := ResolveSources(ff, w.root, []string{filepath.Join(w.root, "lib")}, w.exists, w.glob)
	if len(es) != 1 || es[0].How != "literal" || es[0].Status != SourceResolved || !eqRel(w.root, es[0].Targets, []string{"lib/util.sh"}) {
		t.Fatalf("%+v", es)
	}
	es = ResolveSources(ff, w.root, nil, w.exists, w.glob)
	if len(es) != 1 || es[0].Status != SourceUnresolved {
		t.Fatalf("without search path: %+v", es)
	}
}

func TestLiteralAbsoluteAndQuoted(t *testing.T) {
	w, ff := memWorld(t, "")
	abs := filepath.Join(w.root, "abs.sh")
	w.files[abs] = true
	ff = bindNamed(t, filepath.Join(w.root, "main.sh"), ". '"+filepath.ToSlash(abs)+"'\n. \"./q.sh\"")
	w.files[filepath.Join(w.root, "q.sh")] = true
	es := ResolveSources(ff, w.root, nil, w.exists, w.glob)
	if len(es) != 2 || es[0].Status != SourceResolved || !eq(es[0].Targets, abs) || !eqRel(w.root, es[1].Targets, []string{"q.sh"}) {
		t.Fatalf("%+v", es)
	}
}

func TestLoopGlob(t *testing.T) {
	src := "for f in plugins/*.sh extra.sh; do\n  source \"$f\"\ndone\nfor g in a.sh; do . $g; done\nfor h in $LIST; do . \"$h\"; done\nfor f in *.zz; do . $f; done"
	w, ff := memWorld(t, src, "plugins/a.sh", "plugins/b.sh", "plugins/c.txt", "extra.sh", "a.sh")
	es := ResolveSources(ff, w.root, nil, w.exists, w.glob)
	if len(es) != 4 {
		t.Fatalf("%+v", es)
	}
	if es[0].How != "loop-glob" || len(es[0].Targets) != 3 {
		t.Fatalf("0: %+v", es[0])
	}
	got := map[string]bool{}
	for _, p := range es[0].Targets {
		r, _ := filepath.Rel(w.root, p)
		got[filepath.ToSlash(r)] = true
	}
	if !got["plugins/a.sh"] || !got["plugins/b.sh"] || !got["extra.sh"] {
		t.Fatalf("0 targets: %+v", es[0].Targets)
	}
	if es[1].How != "loop-glob" || !eqRel(w.root, es[1].Targets, []string{"a.sh"}) {
		t.Fatalf("1: %+v", es[1]) // loop variable named g, word `$g`
	}
	if es[2].Status != SourceUnresolved { // non-literal iteration word
		t.Fatalf("2: %+v", es[2])
	}
	if es[3].Status != SourceUnresolved { // glob matches nothing
		t.Fatalf("3: %+v", es[3])
	}
}

func TestLoopGlobWrongVariable(t *testing.T) {
	w, ff := memWorld(t, "for f in a.sh; do . \"$g\"; done\n. \"$f\"", "a.sh")
	es := ResolveSources(ff, w.root, nil, w.exists, w.glob)
	if len(es) != 2 || es[0].Status != SourceUnresolved || es[1].Status != SourceUnresolved {
		t.Fatalf("%+v", es)
	}
}

func TestLoopGlobFallsBackToRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ws")
	p := filepath.Join(root, "sub", "run.sh")
	src := []byte("for f in plugins/*.sh; do . \"$f\"; done")
	ff := Bind(front.Parse(p, src, front.Detect(p, src)))
	want := filepath.Join(root, "plugins", "a.sh")
	glob := func(pat string) []string {
		if ok, _ := filepath.Match(pat, want); ok {
			return []string{want}
		}
		return nil
	}
	es := ResolveSources(ff, root, nil, func(s string) bool { return s == want }, glob)
	if len(es) != 1 || es[0].How != "loop-glob" || !eq(es[0].Targets, want) {
		t.Fatalf("%+v", es)
	}
}

// Options.Oversize turns the not-found of a source naming an existing file
// the caller skipped for its size into skipped-for-size, with that file as
// ToFile; the source still counts as unresolved for the FDS-008 fallback.
func TestSourceOfOversizeFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "w")
	big, main := filepath.Join(root, "big.sh"), filepath.Join(root, "m.sh")
	s := Analyze(map[string][]byte{main: []byte("source ./big.sh\nsource ./none.sh\n")},
		Options{Root: root, Oversize: func(p string) bool { return p == big }})
	var got []string
	for _, e := range s.Edges() {
		got = append(got, e.Res.String()+"/"+e.Reason+" "+e.ToFile+" "+e.Text)
	}
	if strings.Join(got, "|") != "unresolved/skipped-for-size "+big+" ./big.sh|unresolved/not-found  ./none.sh" {
		t.Fatalf("edges %q", got)
	}
	if !s.GraphIncomplete(main) {
		t.Fatal("a skipped source does not leave the graph incomplete")
	}
}

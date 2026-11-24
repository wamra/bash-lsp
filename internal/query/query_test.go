package query

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/internal/config"
	"github.com/wamra/bash-lsp/internal/workspace"
)

func world(t testing.TB, files map[string]string) (*analysis.Snapshot, string) {
	t.Helper()
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	for name, text := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := workspace.Open(root, workspace.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return ws.Snapshot(), ws.Root()
}

// chain is n functions, f0 calling f1 and so on.
func chain(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i+1 < n {
			fmt.Fprintf(&b, "f%d() { f%d; }\n", i, i+1)
		} else {
			fmt.Fprintf(&b, "f%d() { :; }\n", i)
		}
	}
	return b.String()
}

// docs returns a function that turns an answer into its *Doc and the decoded
// JSON of it.
func docs(t *testing.T) func(v any, err error) (*Doc, map[string]any) {
	return func(v any, err error) (*Doc, map[string]any) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		d, ok := v.(*Doc)
		if !ok {
			t.Fatalf("answer is a %T, not a *Doc", v)
		}
		b, err := Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%v\n%s", err, b)
		}
		return d, m
	}
}

// TestGraphDepthCap: a 100-function call chain; impact with depth 0 runs at
// 64 and says it was cut.
func TestGraphDepthCap(t *testing.T) {
	doc := docs(t)
	s, root := world(t, map[string]string{"c.sh": chain(100)})
	d, m := doc((Impact(s, root, "f99", 0, 0)))
	if got := len(m["results"].([]any)); got != MaxDepth {
		t.Fatalf("impact depth 0: %d levels (nodes), want %d", got, MaxDepth)
	}
	if m["truncated"] != true || !d.Truncated {
		t.Errorf("no truncated flag: %v", m)
	}
	// depth above the cap is cut the same way
	if _, m := doc((Impact(s, root, "f99", 1000, 0))); m["truncated"] != true || len(m["results"].([]any)) != MaxDepth {
		t.Errorf("depth 1000: %v", m["truncated"])
	}
	// a depth the caller chose, within the cap, is not a cut
	if _, m := doc((Impact(s, root, "f99", 10, 0))); m["truncated"] != nil || len(m["results"].([]any)) != 10 {
		t.Errorf("depth 10: truncated %v, %d results", m["truncated"], len(m["results"].([]any)))
	}
	// depth 0 over a chain that fits in 64 levels: all of it, nothing cut
	s2, root2 := world(t, map[string]string{"c.sh": chain(30)})
	if _, m := doc((Impact(s2, root2, "f29", 0, 0))); m["truncated"] != nil || len(m["results"].([]any)) != 29 {
		t.Errorf("30-chain depth 0: truncated %v, %d results", m["truncated"], len(m["results"].([]any)))
	}
	// callers and callees with depth 0
	for _, c := range []struct {
		name string
		f    func(*analysis.Snapshot, string, string, int, int) (any, error)
		spec string
	}{{"callers", Callers, "f99"}, {"callees", Callees, "f0"}} {
		_, m := doc(c.f(s, root, c.spec, 0, 0))
		if m["truncated"] != true || len(m["results"].([]any)) != MaxDepth {
			t.Errorf("%s depth 0: truncated %v, %d edges", c.name, m["truncated"], len(m["results"].([]any)))
		}
	}
}

func TestMaxResultsCut(t *testing.T) {
	doc := docs(t)
	var b strings.Builder
	for i := 0; i < 25; i++ {
		fmt.Fprintf(&b, "g%d() { :; }\n", i)
	}
	s, root := world(t, map[string]string{"g.sh": b.String()})
	d, m := doc(FindSymbol(s, root, "", 10), nil)
	if len(m["results"].([]any)) != 10 || m["truncated"] != true || !d.Truncated {
		t.Errorf("max 10: %d results, truncated %v", len(m["results"].([]any)), m["truncated"])
	}
	if _, m := doc(FindSymbol(s, root, "", 25), nil); m["truncated"] != nil || len(m["results"].([]any)) != 25 {
		t.Errorf("max 25 of 25: truncated %v", m["truncated"])
	}
	if _, m := doc(FindSymbol(s, root, "", 0), nil); len(m["results"].([]any)) != 25 {
		t.Errorf("max 0 means the default cap: %d", len(m["results"].([]any)))
	}
}

func TestFindSymbolRecord(t *testing.T) {
	doc := docs(t)
	s, root := world(t, map[string]string{"a.sh": "  deploy() {\n    :\n  }\nNAME=value\n"})
	_, m := doc(FindSymbol(s, root, "deploy", 0), nil)
	r := m["results"].([]any)[0].(map[string]any)
	want := map[string]any{"path": "a.sh", "line": 1.0, "col": 3.0, "kind": "function", "name": "deploy", "signature": "deploy() {"}
	for k, v := range want {
		if r[k] != v {
			t.Errorf("%s = %v, want %v", k, r[k], v)
		}
	}
	_, m = doc(FindSymbol(s, root, "NAME", 0), nil)
	r = m["results"].([]any)[0].(map[string]any)
	if r["kind"] != "variable" || r["signature"] != "NAME=value" {
		t.Errorf("%v", r)
	}
	long := "f() { echo " + strings.Repeat("é", 300) + "; }\n"
	s2, root2 := world(t, map[string]string{"l.sh": long})
	_, m = doc(FindSymbol(s2, root2, "f", 0), nil)
	sig := m["results"].([]any)[0].(map[string]any)["signature"].(string)
	if len(sig) > maxSignature || strings.ContainsRune(sig, '�') {
		t.Errorf("signature %d bytes: %q", len(sig), sig)
	}
}

func TestNotTruncatedMeansNoKey(t *testing.T) {
	s, root := world(t, map[string]string{"a.sh": "f() { g; }\ng() { :; }\n"})
	b, err := Marshal(mustDoc(StepsDoc(s, root, "callers", "g", 1, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "truncated") || strings.Contains(string(b), "found") {
		t.Errorf("%s", b)
	}
	tr, _ := TraceDoc(s, root, "f", "g", 0)
	b, _ = Marshal(tr)
	if !strings.Contains(string(b), `"found": true`) {
		t.Errorf("trace lacks found: %s", b)
	}
	tr, _ = TraceDoc(s, root, "g", "f", 0)
	b, _ = Marshal(tr)
	if !strings.Contains(string(b), `"found": false`) || strings.Join(tr.Lines, "") != "no path" {
		t.Errorf("trace without a path: %s %v", b, tr.Lines)
	}
}

func mustDoc(d *Doc, err error) *Doc {
	if err != nil {
		panic(err)
	}
	return d
}

func TestAmbiguousAndNotFound(t *testing.T) {
	s, root := world(t, map[string]string{"a.sh": "f() { :; }\n", "b.sh": "f() { :; }\n"})
	_, err := Callers(s, root, "f", 1, 0)
	var amb *AmbiguousError
	if !errors.As(err, &amb) || len(amb.Lines) != 2 || amb.Lines[0] != "a.sh:1:1  f" || amb.Lines[1] != "b.sh:1:1  f" {
		t.Fatalf("%v", err)
	}
	if want := "\"f\" is ambiguous; use one of:\na.sh:1:1  f\nb.sh:1:1  f"; err.Error() != want {
		t.Errorf("message %q", err.Error())
	}
	if _, err := Callers(s, root, "nope", 1, 0); err == nil || errors.As(err, &amb) {
		t.Errorf("not found: %v", err)
	}
}

func TestCheckInsideSymlinks(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "x.sh"), []byte(":\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, root := world(t, map[string]string{"in.sh": "f() { :; }\n"})
	if err := os.Symlink(outside, filepath.Join(root, "linkdir")); err != nil {
		t.Skip("no symlinks:", err)
	}
	if err := os.Symlink(filepath.Join(outside, "x.sh"), filepath.Join(root, "link.sh")); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []string{"../x.sh", "../../etc/passwd", filepath.Join(outside, "x.sh"), "linkdir/x.sh", "linkdir/x.sh:1:1", "link.sh", "linkdir/new.sh"} {
		if err := CheckInside(root, spec); err == nil || !strings.Contains(err.Error(), "outside the root") {
			t.Errorf("%q: %v", spec, err)
		}
		if _, err := Callers(s, root, spec, 1, 0); err == nil || !strings.Contains(err.Error(), "outside the root") {
			t.Errorf("Callers %q: %v", spec, err)
		}
	}
	for _, spec := range []string{"in.sh", "in.sh:1:1", "f", "sub/missing.sh", filepath.Join(root, "in.sh")} {
		if err := CheckInside(root, spec); err != nil {
			t.Errorf("%q refused: %v", spec, err)
		}
	}
	if _, err := UnresolvedDoc(s, root, []string{"in.sh", "linkdir/x.sh"}, 0); err == nil {
		t.Error("unresolved accepted a path through a symlink out")
	}
}

func TestDiagnosticsRecords(t *testing.T) {
	doc := docs(t)
	s, root := world(t, map[string]string{"a.sh": "source ./missing.sh\n", "b.sh": ":\n"})
	_, m := doc(Diagnostics(s, root, []string{"a.sh"}, analysis.DiagOptions{}, 0), nil)
	rs := m["results"].([]any)
	if len(rs) == 0 {
		t.Fatal("no finding for an unresolved source")
	}
	r := rs[0].(map[string]any)
	if r["path"] != "a.sh" || r["line"] != 1.0 || r["origin"] != "bash-lsp" || r["severity"] == "" || r["rule"] == "" {
		t.Errorf("%v", r)
	}
	// a path the snapshot does not hold, and one outside the root: no findings, no read
	_, m = doc(Diagnostics(s, root, []string{"nope.sh", "../../etc/passwd"}, analysis.DiagOptions{}, 0), nil)
	if len(m["results"].([]any)) != 0 {
		t.Errorf("%v", m["results"])
	}
	_, m = doc(Diagnostics(s, root, nil, analysis.DiagOptions{}, 1), nil)
	if len(m["results"].([]any)) != 1 {
		t.Errorf("max 1: %v", m["results"])
	}
}

func TestStatusIncludesSnapshotLimits(t *testing.T) {
	s, root := world(t, map[string]string{"x.sh": "alias a=b\nalias b=a\na\na\n"})
	rep := workspace.Report{Files: 1, LimitsHit: []string{"big.sh: 203 bytes exceeds the 100 byte file limit"}}
	st := Status(s, rep, config.Settings{}, WithRoot(root)).(*StatusDoc)
	if len(st.Limits) != 2 || st.Limits[0] != rep.LimitsHit[0] || !strings.HasPrefix(st.Limits[1], "x.sh:1: alias depth:") {
		t.Fatalf("limits %q: want the report's line, then the snapshot's", st.Limits)
	}
	if lines := strings.Join(st.Lines(), "\n"); !strings.Contains(lines, "limit hit: x.sh:1: alias depth") || !strings.HasSuffix(lines, "cache: none") {
		t.Errorf("text:\n%s", lines)
	}
	b, _ := Marshal(Status(s, workspace.Report{}, config.Settings{}))
	if !strings.Contains(string(b), `"dialects": {}`) || !strings.Contains(string(b), `"skipped_files": []`) {
		t.Errorf("empty report: %s", b)
	}
}

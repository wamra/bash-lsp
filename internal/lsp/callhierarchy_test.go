package lsp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The call-hierarchy structures as a client decodes them (LSP 3.17,
// CallHierarchyItem, CallHierarchyIncomingCall, CallHierarchyOutgoingCall).
type chItem struct {
	Name           string          `json:"name"`
	Kind           int             `json:"kind"`
	Tags           []int           `json:"tags"`
	Detail         string          `json:"detail"`
	URI            string          `json:"uri"`
	Range          Range           `json:"range"`
	SelectionRange Range           `json:"selectionRange"`
	Data           json.RawMessage `json:"data,omitempty"`
}

type chIncoming struct {
	From       chItem  `json:"from"`
	FromRanges []Range `json:"fromRanges"`
}

type chOutgoing struct {
	To         chItem  `json:"to"`
	FromRanges []Range `json:"fromRanges"`
}

func (c *testClient) prepareRaw(rel string, p Position) json.RawMessage {
	c.t.Helper()
	return c.call("textDocument/prepareCallHierarchy", map[string]any{
		"textDocument": map[string]any{"uri": c.uri(rel)}, "position": p})
}

func (c *testClient) prepare(rel, text string, col int) []chItem {
	c.t.Helper()
	raw := c.prepareRaw(rel, c.pos(rel, text, col))
	var out []chItem
	if err := json.Unmarshal(raw, &out); err != nil {
		c.t.Fatalf("%v: %s", err, raw)
	}
	return out
}

func (c *testClient) incoming(it chItem) []chIncoming {
	c.t.Helper()
	raw := c.call("callHierarchy/incomingCalls", map[string]any{"item": it})
	var out []chIncoming
	if err := json.Unmarshal(raw, &out); err != nil {
		c.t.Fatalf("%v: %s", err, raw)
	}
	if out == nil {
		c.t.Fatalf("incomingCalls answered %s, want an array", raw)
	}
	return out
}

func (c *testClient) outgoing(it chItem) []chOutgoing {
	c.t.Helper()
	raw := c.call("callHierarchy/outgoingCalls", map[string]any{"item": it})
	var out []chOutgoing
	if err := json.Unmarshal(raw, &out); err != nil {
		c.t.Fatalf("%v: %s", err, raw)
	}
	if out == nil {
		c.t.Fatalf("outgoingCalls answered %s, want an array", raw)
	}
	return out
}

// rel is the item's file relative to the client root, with forward slashes.
func (c *testClient) relOf(uri string) string {
	c.t.Helper()
	s := c.short([]Location{{URI: uri}})[0]
	return s[:strings.LastIndexByte(s, ':')]
}

// lines renders ranges as "line:char" (both 1-based) of their starts.
func lines(rs []Range) string {
	var out []string
	for _, r := range rs {
		out = append(out, fmt.Sprintf("%d:%d", r.Start.Line+1, r.Start.Character+1))
	}
	return strings.Join(out, " ")
}

// itemText is "name kind rel detail" for comparison.
func (c *testClient) itemText(it chItem) string {
	return fmt.Sprintf("%s %d %s [%s]", it.Name, it.Kind, c.relOf(it.URI), it.Detail)
}

func TestCallHierarchyPrepare(t *testing.T) { // C09
	c := client(t, copyCorpus(t))
	c.open("main.sh")
	c.open("lib/util.sh")

	items := c.prepare("lib/util.sh", "build_release() {", 2) // on the declaration
	if len(items) != 1 {
		t.Fatalf("%d items", len(items))
	}
	it := items[0]
	if got := c.itemText(it); got != "build_release 12 lib/util.sh [lib/util.sh]" {
		t.Fatalf("item %s", got)
	}
	if it.Range.Start.Line != 3 || it.Range.End.Line != 6 || it.SelectionRange != (Range{Position{3, 0}, Position{3, 13}}) {
		t.Fatalf("ranges %+v %+v", it.Range, it.SelectionRange)
	}
	var data map[string]any
	if err := json.Unmarshal(it.Data, &data); err != nil {
		t.Fatalf("data %s: %v", it.Data, err)
	}
	if data["name"] != "build_release" || !strings.HasSuffix(fmt.Sprint(data["path"]), filepath.Join("lib", "util.sh")) {
		t.Fatalf("data %s", it.Data)
	}

	for _, tc := range []struct {
		file, text string
		col        int
		want       string
	}{
		{"main.sh", `  br "$name"`, 2, "build_release 12 lib/util.sh [lib/util.sh]"},           // through an alias
		{"main.sh", `"$cmd" x`, 2, "build_release 12 lib/util.sh [lib/util.sh]"},               // through a variable
		{"main.sh", `eval "build_release y"`, 7, "build_release 12 lib/util.sh [lib/util.sh]"}, // inside an eval string
		{"main.sh", `trap 'log bye'`, 6, "log 12 lib/util.sh [lib/util.sh]"},                   // inside a trap string
		{"main.sh", `deploy app`, 0, "deploy 12 main.sh [main.sh]"},                            // top-level call
	} {
		got := c.prepare(tc.file, tc.text, tc.col)
		if len(got) != 1 || c.itemText(got[0]) != tc.want {
			var s []string
			for _, g := range got {
				s = append(s, c.itemText(g))
			}
			t.Errorf("prepare at %q: %v, want %s", tc.text, s, tc.want)
		}
	}

	// Elsewhere: a variable, a builtin, the path of a `source` command.
	for _, at := range []struct {
		text string
		col  int
	}{{`local name="$1"`, 6}, {`echo "$name"`, 1}, {`source ./lib/util.sh`, 9}} {
		if raw := c.prepareRaw("main.sh", c.pos("main.sh", at.text, at.col)); string(raw) != "null" {
			t.Errorf("prepare at %q: %s, want null", at.text, raw)
		}
	}
}

func TestCallHierarchyPrepareAmbiguous(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "v.sh", "a() { :; }\nb() { :; }\nif true; then cmd=a; else cmd=b; fi\n\"$cmd\"\n")
	writeFile(t, root, "m.sh", "source ./x.sh\nsource ./y.sh\nf\n")
	writeFile(t, root, "x.sh", "f() { :; }\n")
	writeFile(t, root, "y.sh", "f() { :; }\n")
	c := client(t, root)
	var got []string
	for _, it := range c.prepare("v.sh", `"$cmd"`, 2) {
		got = append(got, c.itemText(it))
	}
	if !eq(got, "a 12 v.sh [v.sh]", "b 12 v.sh [v.sh]") {
		t.Errorf("variable with two constant values: %v", got)
	}
	got = nil
	for _, it := range c.prepare("m.sh", "f", 0) {
		got = append(got, c.itemText(it))
	}
	if !eq(got, "f 12 x.sh [x.sh]", "f 12 y.sh [y.sh]") {
		t.Errorf("two definitions in view: %v", got)
	}
}

func TestCallHierarchyIncomingOutgoing(t *testing.T) { // C10, C11, AC-05
	c := client(t, copyCorpus(t))
	c.open("lib/util.sh")
	br := c.prepare("lib/util.sh", "build_release() {", 2)[0]

	in := c.incoming(br)
	var got []string
	for _, x := range in {
		got = append(got, c.itemText(x.From)+" "+lines(x.FromRanges))
	}
	want := []string{
		"dyn.sh 1 dyn.sh [dyn.sh] 4:1",
		"deploy 12 main.sh [main.sh · candidate/variable-constant, candidate/string-eval, resolved/alias] 6:3 7:3 9:3 10:9",
	}
	if !eq(got, want...) {
		t.Fatalf("incoming\n got %q\nwant %q", got, want)
	}

	dep := in[1].From
	out := c.outgoing(dep)
	got = nil
	for _, x := range out {
		got = append(got, c.itemText(x.To)+" "+lines(x.FromRanges))
	}
	want = []string{
		"build_release 12 lib/util.sh [lib/util.sh · candidate/variable-constant, candidate/string-eval, resolved/alias] 6:3 7:3 9:3 10:9",
		"log 12 lib/util.sh [lib/util.sh · candidate/string-trap] 11:9",
	}
	if !eq(got, want...) {
		t.Fatalf("outgoing of deploy\n got %q\nwant %q", got, want)
	}

	// A file item (top-level code) has the calls of its top level; a
	// `source` line is not a call.
	dyn := in[0].From
	if dyn.SelectionRange.Start != (Position{}) || dyn.Range.Start != (Position{}) || dyn.Range.End.Line < 3 {
		t.Fatalf("file item ranges %+v %+v", dyn.Range, dyn.SelectionRange)
	}
	got = nil
	for _, x := range c.outgoing(dyn) {
		got = append(got, c.itemText(x.To)+" "+lines(x.FromRanges))
	}
	if !eq(got, "build_release 12 lib/util.sh [lib/util.sh] 4:1") {
		t.Fatalf("outgoing of dyn.sh: %q", got)
	}

	// Incoming of a function called from a trap string (the review focus:
	// a function called only from a trap must not show zero callers).
	logIt := c.prepare("lib/util.sh", "log() {", 1)[0]
	got = nil
	for _, x := range c.incoming(logIt) {
		got = append(got, c.itemText(x.From)+" "+lines(x.FromRanges))
	}
	if !eq(got, "build_release 12 lib/util.sh [lib/util.sh] 6:3", "deploy 12 main.sh [main.sh · candidate/string-trap] 11:9") {
		t.Fatalf("incoming of log: %q", got)
	}
}

func TestCallHierarchyRecursionDynamicInvokes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.sh", "f() { f; }\ng() { $(pick) x; h; }\nh() { :; }\nf\n./b.sh\n")
	writeFile(t, root, "b.sh", "echo b\n")
	c := client(t, root)
	f := c.prepare("a.sh", "f() {", 0)[0]
	var got []string
	in := c.incoming(f)
	for _, x := range in {
		got = append(got, c.itemText(x.From)+" "+lines(x.FromRanges))
	}
	if !eq(got, "f 12 a.sh [a.sh] 1:7", "a.sh 1 a.sh [a.sh] 4:1") { // in the order of their first site
		t.Fatalf("incoming of a recursive function: %q", got)
	}
	got = nil
	for _, x := range c.outgoing(c.prepare("a.sh", "g() {", 0)[0]) {
		got = append(got, c.itemText(x.To)+" "+lines(x.FromRanges))
	}
	if !eq(got, "h 12 a.sh [a.sh] 2:18") {
		t.Fatalf("outgoing of g (a dynamic site is no item): %q", got)
	}
	got = nil
	for _, x := range c.outgoing(in[1].From) {
		got = append(got, c.itemText(x.To)+" "+lines(x.FromRanges))
	}
	if !eq(got, "f 12 a.sh [a.sh] 4:1", "b.sh 1 b.sh [b.sh] 5:1") {
		t.Fatalf("outgoing of a.sh (a call and an invoked file): %q", got)
	}
}

// A follow-up request after an edit resolves the item by path and name
// against the current generation; a function that no longer exists gives an
// empty result, not an error.
func TestCallHierarchyAfterEdit(t *testing.T) {
	root := copyCorpus(t)
	c := client(t, root)
	c.open("main.sh")
	c.open("lib/util.sh")
	br := c.prepare("lib/util.sh", "build_release() {", 2)[0]

	main := readFile(t, root, "main.sh")
	c.change("main.sh", fullText("\n"+main))
	in := c.incoming(br)
	if len(in) != 2 || in[1].From.Name != "deploy" || lines(in[1].FromRanges) != "7:3 8:3 10:3 11:9" {
		t.Fatalf("after inserting a line in main.sh: %+v", in)
	}

	util := readFile(t, root, "lib/util.sh")
	c.change("lib/util.sh", fullText("\n\n"+util)) // the offset hint in data is stale now
	if in := c.incoming(br); len(in) != 2 {
		t.Fatalf("after moving the definition: %+v", in)
	}

	noData := br
	noData.Data = nil // the item's uri and name are enough
	if in := c.incoming(noData); len(in) != 2 {
		t.Fatalf("item without data: %+v", in)
	}

	c.change("lib/util.sh", fullText(strings.Replace(util, "build_release() {", "build_it() {", 1)))
	raw := c.call("callHierarchy/incomingCalls", map[string]any{"item": br})
	if string(raw) != "[]" {
		t.Fatalf("incoming of a removed function: %s, want []", raw)
	}
	raw = c.call("callHierarchy/outgoingCalls", map[string]any{"item": br})
	if string(raw) != "[]" {
		t.Fatalf("outgoing of a removed function: %s, want []", raw)
	}
	bogus := br
	bogus.Data = json.RawMessage(`"not an object"`)
	bogus.URI = c.uri("nope.sh")
	if raw := c.call("callHierarchy/incomingCalls", map[string]any{"item": bogus}); string(raw) != "[]" {
		t.Fatalf("incoming of an unknown item: %s", raw)
	}
}

func TestReferencesResolvedOnly(t *testing.T) { // AC-03 both ways
	root := copyCorpus(t)
	all := client(t, root)
	all.open("main.sh")
	if got := all.references("main.sh", `  build_release "$name"`, 3); !eq(got,
		"dyn.sh:4", "lib/util.sh:4", "main.sh:3", "main.sh:6", "main.sh:7", "main.sh:8", "main.sh:9", "main.sh:10") {
		t.Fatalf("default references: %v", got)
	}

	c := clientInit(t, root, false, map[string]any{"references": map[string]any{"resolvedOnly": true}})
	c.open("main.sh")
	if got := c.references("main.sh", `  build_release "$name"`, 3); !eq(got,
		"dyn.sh:4", "lib/util.sh:4", "main.sh:3", "main.sh:6", "main.sh:7") {
		t.Fatalf("resolvedOnly references: %v", got)
	}
	// Without the declaration.
	got := c.short(c.locs("textDocument/references", "main.sh", c.pos("main.sh", `  build_release "$name"`, 3),
		map[string]any{"context": map[string]any{"includeDeclaration": false}}))
	if !eq(got, "dyn.sh:4", "main.sh:3", "main.sh:6", "main.sh:7") {
		t.Fatalf("resolvedOnly references without declaration: %v", got)
	}
	// Variables are unaffected.
	if got := c.references("main.sh", `local name="$1"`, 6); !eq(got, "main.sh:5", "main.sh:6", "main.sh:7") {
		t.Fatalf("resolvedOnly variable references: %v", got)
	}

	// unset-in-view lives on edges only: the binding stays resolved, the
	// edge is candidate, and resolvedOnly drops the site.
	ws := t.TempDir()
	writeFile(t, ws, "u.sh", "f() { :; }\nf\nunset -f f\n")
	u := clientInit(t, ws, false, map[string]any{"references": map[string]any{"resolvedOnly": true}})
	if got := u.references("u.sh", "f() {", 0); !eq(got, "u.sh:1") {
		t.Fatalf("resolvedOnly with unset -f in view: %v", got)
	}
	if got := client(t, ws).references("u.sh", "f() {", 0); !eq(got, "u.sh:1", "u.sh:2") {
		t.Fatalf("default with unset -f in view: %v", got)
	}

	// A malformed option is ignored, not an initialize error.
	for _, opt := range []any{"yes", map[string]any{"references": 5}, map[string]any{"references": map[string]any{"resolvedOnly": "true"}}} {
		b := clientInit(t, root, false, opt)
		b.open("main.sh")
		if got := b.references("main.sh", `  build_release "$name"`, 3); len(got) != 8 {
			t.Fatalf("option %v: %v", opt, got)
		}
	}
}

func (c *testClient) hoverText(rel, text string, col int) string {
	c.t.Helper()
	return c.hoverAt(rel, c.pos(rel, text, col))
}

func (c *testClient) hoverAt(rel string, p Position) string {
	c.t.Helper()
	raw := c.call("textDocument/hover", map[string]any{
		"textDocument": map[string]any{"uri": c.uri(rel)}, "position": p})
	var h struct {
		Contents markupContent `json:"contents"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		c.t.Fatalf("%v: %s", err, raw)
	}
	return h.Contents.Value
}

func TestHoverLabelLines(t *testing.T) { // AC-01, AC-02, FDS-011
	c := client(t, copyCorpus(t))
	c.open("main.sh")
	for _, tc := range []struct {
		text string
		col  int
		want string
	}{
		{`  br "$name"`, 2, "calls `build_release` through alias `br` (resolved/alias)"},
		{`trap 'log bye'`, 6, "calls `log` through a `trap` string (candidate/string-trap)"},
		{`"$cmd" x`, 2, "calls `build_release` through variable `cmd` (candidate/variable-constant)"},
		{`eval "build_release y"`, 7, "calls `build_release` through an `eval` string (candidate/string-eval)"},
		{`local cmd=build_release`, 11, "value of `cmd` names `build_release` (candidate/variable-constant)"}, // no edge: from the binding
		{`alias br=build_release`, 10, "alias `br` names `build_release` (resolved/direct)"},
	} {
		md := c.hoverText("main.sh", tc.text, tc.col)
		if strings.Count(md, "\n"+tc.want+"\n") != 1 {
			t.Errorf("hover at %q: no line %q in:\n%s", tc.text, tc.want, md)
		}
	}
	if md := c.hoverText("main.sh", `  build_release "$name"`, 3); strings.Contains(md, "calls `") {
		t.Errorf("a resolved/direct call has no label line:\n%s", md)
	}

	ws := t.TempDir()
	writeFile(t, ws, "u.sh", "f() { :; }\nf\nunset -f f\nbash -c 'f'\n")
	u := client(t, ws)
	if md := u.hoverAt("u.sh", Position{1, 0}); !strings.Contains(md, "\ncalls `f` (candidate/unset-in-view)\n") {
		t.Errorf("unset-in-view site:\n%s", md)
	}
	if md := u.hoverText("u.sh", "bash -c 'f'", 9); !strings.Contains(md, "\ncalls `f` through a shell `-c` string (candidate/unset-in-view)\n") {
		t.Errorf("shell -c site:\n%s", md)
	}
}

// Thousands of call sites on one long line with a non-ASCII character
// before them: every published position goes through one converter per file
// and request, so the answer is linear (a per-location rescan of the line
// would take minutes).
func TestCallHierarchyManySitesAreFast(t *testing.T) {
	const bound = 5 * time.Second
	n := 100_000
	if raceEnabled {
		n /= 4
	}
	root := t.TempDir()
	writeFile(t, root, "w.sh", "f() { :; }\n: é;"+strings.Repeat(" f;", n)+"\n")
	timed := func(what string, fn func()) {
		t.Helper()
		start := time.Now()
		fn()
		d := time.Since(start)
		t.Logf("%s: %v", what, d)
		if d > bound && !raceEnabled {
			t.Errorf("%s took %v, want under %v", what, d, bound)
		}
	}
	c := clientInit(t, root, false, map[string]any{"references": map[string]any{"resolvedOnly": true}})
	f := c.prepare("w.sh", "f() {", 0)[0]
	var in []chIncoming
	timed("incomingCalls", func() { in = c.incoming(f) })
	if len(in) != 1 || len(in[0].FromRanges) != n {
		t.Fatalf("incoming: %d entries", len(in))
	}
	if r := in[0].FromRanges[n-1]; r.Start.Character != 4+3*n-2 { // ": é;" is 4 UTF-16 units, each " f;" 3
		t.Fatalf("last site %+v", r)
	}
	var out []chOutgoing
	timed("outgoingCalls", func() { out = c.outgoing(in[0].From) })
	if len(out) != 1 || len(out[0].FromRanges) != n {
		t.Fatalf("outgoing: %d entries", len(out))
	}
	timed("references resolvedOnly", func() {
		if got := c.references("w.sh", "f() {", 0); len(got) != n+1 {
			t.Fatalf("%d references", len(got))
		}
	})
}

func readFile(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Item data comes back from the client and is untrusted: whatever it names,
// the answer is from the current snapshot only. A file the snapshot does not
// hold (outside the root, not a shell file, a name the snapshot has no
// function for) answers [], never an error, and is never read from disk: the
// files outside the root and the text file below define and call functions,
// so reading and parsing one would answer calls.
func TestCallHierarchyHostileItemData(t *testing.T) {
	root := copyCorpus(t)
	outside := t.TempDir()
	evil := "evil() { victim; }\nvictim() { :; }\nevil\n"
	if err := os.WriteFile(filepath.Join(outside, "evil.sh"), []byte(evil), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte(evil), 0o644); err != nil {
		t.Fatal(err)
	}
	c := client(t, root)
	c.open("main.sh")
	main := filepath.Join(root, "main.sh")
	long := strings.Repeat("a", 2<<20)
	data := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	item := func(uri, name string, kind int, d json.RawMessage) map[string]any {
		it := map[string]any{"name": name, "kind": kind, "uri": uri,
			"range": rng(0, 0, 0, 0), "selectionRange": rng(0, 0, 0, 0)}
		if d != nil {
			it["data"] = d
		}
		return it
	}
	mainURI := c.uri("main.sh")
	for name, params := range map[string]any{
		"outside the root":         map[string]any{"item": item(mainURI, "evil", 12, data(map[string]any{"path": filepath.Join(outside, "evil.sh"), "name": "evil"}))},
		"outside, file item":       map[string]any{"item": item(mainURI, "x", 1, data(map[string]any{"path": filepath.Join(outside, "evil.sh"), "name": ""}))},
		"dot-dot to /etc/hosts":    map[string]any{"item": item(mainURI, "x", 1, data(map[string]any{"path": root + "/../../../etc/hosts", "name": ""}))},
		"not a shell file":         map[string]any{"item": item(mainURI, "evil", 12, data(map[string]any{"path": filepath.Join(root, "notes.txt"), "name": "evil"}))},
		"2 MiB path":               map[string]any{"item": item(mainURI, "x", 12, data(map[string]any{"path": "/" + long, "name": "deploy"}))},
		"2 MiB name":               map[string]any{"item": item(mainURI, "x", 12, data(map[string]any{"path": main, "name": long}))},
		"a variable's name":        map[string]any{"item": item(mainURI, "name", 12, data(map[string]any{"path": main, "name": "name"}))},
		"data 12345":               map[string]any{"item": item("file:///etc/passwd", "x", 12, json.RawMessage(`12345`))},
		"data array":               map[string]any{"item": item("file:///etc/passwd", "x", 12, json.RawMessage(`[1,2,3]`))},
		"data null":                map[string]any{"item": item("file:///etc/passwd", "x", 1, json.RawMessage(`null`))},
		"data path a number":       map[string]any{"item": item("file:///etc/passwd", "x", 1, json.RawMessage(`{"path":5}`))},
		"uri /etc/passwd":          map[string]any{"item": item("file:///etc/passwd", "root", 12, nil)},
		"uri outside, no data":     map[string]any{"item": item("file://"+filepath.ToSlash(filepath.Join(outside, "evil.sh")), "evil", 12, nil)},
		"uri not a file scheme":    map[string]any{"item": item("https://example.invalid/x.sh", "deploy", 12, nil)},
		"missing item":             map[string]any{},
		"item with no uri or data": map[string]any{"item": map[string]any{"name": "deploy", "kind": 12}},
	} {
		for _, m := range []string{"callHierarchy/incomingCalls", "callHierarchy/outgoingCalls"} {
			r := c.rawCall(m, params)
			if r.Error != nil || string(r.Result) != "[]" {
				t.Errorf("%s, %s: error %+v, result %.200s; want []", name, m, r.Error, r.Result)
			}
		}
	}
	// The session is intact.
	if got := c.prepare("main.sh", "deploy app", 0); len(got) != 1 {
		t.Fatalf("prepare after hostile items: %d items", len(got))
	}
}

package lsp

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wamra/bash-lsp/internal/workspace"
)

func TestIsGitignoreFolds(t *testing.T) {
	for _, c := range []struct {
		path string
		fold bool
		want bool
	}{
		{"/r/.gitignore", false, true},
		{"/r/.gitignore", true, true},
		{"/r/.GitIgnore", false, false},
		{"/r/.GITIGNORE", true, true},
		{"/r/sub/.GitIgnore", true, true},
		{"/r/gitignore", true, false},
	} {
		if got := isGitignore(c.path, c.fold); got != c.want {
			t.Errorf("isGitignore(%q, %v) = %v, want %v", c.path, c.fold, got, c.want)
		}
	}
}

// A change event for a case variant of .gitignore re-walks the folder exactly
// when the folder's volume folds case.
func TestGitignoreVariantEventFollowsVolume(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "echo a\n")
	var jobs atomic.Int32
	hookMu.Lock()
	folderJobHook = func() { jobs.Add(1) }
	hookMu.Unlock()
	t.Cleanup(func() { hookMu.Lock(); folderJobHook = nil; hookMu.Unlock() })

	var srv *server
	c := clientWith(t, clientCfg{root: root, onServer: func(s *server) { srv = s }})
	c.open("a.sh") // the first build is done after a request
	c.symbols("")
	var fold bool
	done := make(chan struct{})
	srv.post(func() { fold = srv.folders[0].ws.CaseInsensitive(); close(done) })
	<-done
	c.notify("workspace/didChangeWatchedFiles", map[string]any{"changes": []any{
		map[string]any{"uri": workspace.URIFromPath(filepath.Join(root, ".GITIGNORE")), "type": 2}}})
	c.symbols("") // a request waits for the job, so the count is final
	want := int32(0)
	if fold {
		want = 1
	}
	if got := jobs.Load(); got != want {
		t.Fatalf("folding volume %v: %d folder jobs after a .GITIGNORE event, want %d", fold, got, want)
	}
}

func TestResolvedOnlyCanonicalKey(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "u.sh"), "f() { :; }\nf\nunset -f f\n")
	for name, opts := range map[string]any{
		"canonical": map[string]any{"references": map[string]any{"resolved_only": true}},
		"alias":     map[string]any{"references": map[string]any{"resolvedOnly": true}},
	} {
		c := clientInit(t, root, false, opts)
		if got := c.references("u.sh", "f() {", 0); !eq(got, "u.sh:1") {
			t.Errorf("%s: references %v, want the declaration only", name, got)
		}
	}
	if got := client(t, root).references("u.sh", "f() {", 0); !eq(got, "u.sh:1", "u.sh:2") {
		t.Errorf("default: references %v", got)
	}
	// The key also comes from the repo file, per folder.
	put(t, filepath.Join(root, ".bash-lsp.toml"), "[references]\nresolved_only = true\n")
	if got := client(t, root).references("u.sh", "f() {", 0); !eq(got, "u.sh:1") {
		t.Errorf(".bash-lsp.toml: references %v, want the declaration only", got)
	}
}

// ---- helpers ----

// waitPush returns the version and findings of the next publishDiagnostics for
// rel, in arrival order (version -1: the push had none).
func (c *testClient) waitPush(rel string) (int, []Diagnostic) {
	c.t.Helper()
	uri := c.uri(rel)
	deadline := time.AfterFunc(10*time.Second, func() { c.mu.Lock(); c.closed = true; c.cond.Broadcast(); c.mu.Unlock() })
	defer deadline.Stop()
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.diags[uri]) == 0 {
		if c.closed {
			c.t.Fatalf("no diagnostics pushed for %s", rel)
		}
		c.cond.Wait()
	}
	d, v := c.diags[uri][0], c.vers[uri][0]
	c.diags[uri], c.vers[uri] = c.diags[uri][1:], c.vers[uri][1:]
	return v, d
}

// waitPushWhere returns the first push for rel that satisfies ok, dropping the
// ones before it.
func (c *testClient) waitPushWhere(rel string, ok func(version int, ds []Diagnostic) bool) (int, []Diagnostic) {
	c.t.Helper()
	for {
		v, ds := c.waitPush(rel)
		if ok(v, ds) {
			return v, ds
		}
	}
}

// pending is how many pushes for rel are waiting to be read.
func (c *testClient) pending(rel string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.diags[c.uri(rel)])
}

func codes(ds []Diagnostic) []string {
	out := []string{}
	for _, d := range ds {
		out = append(out, d.Code)
	}
	return out
}

func hasCode(ds []Diagnostic, code string) bool { return contains(codes(ds), code) }

func hasSource(ds []Diagnostic, src string) bool {
	for _, d := range ds {
		if d.Source == src {
			return true
		}
	}
	return false
}

// ---- push ----

func TestNativeOnOpenAndChange(t *testing.T) { // AC-12 shape: native findings, versioned, no ShellCheck
	t.Setenv("PATH", t.TempDir())
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	c := client(t, root)
	c.openText("a.sh", "#!/bin/bash\nrm $f\n")
	v, ds := c.waitPush("a.sh")
	if v != 1 || !hasCode(ds, "WB1401") || !hasSource(ds, "bash-lsp") || hasSource(ds, "shellcheck") {
		t.Fatalf("open: version %d, %v", v, ds)
	}
	c.change("a.sh", fullText("#!/bin/bash\nrm \"$f\"\n"))
	v, ds = c.waitPush("a.sh")
	if v != 2 || hasCode(ds, "WB1401") {
		t.Fatalf("change: version %d, %v", v, ds)
	}
	c.notify("textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": c.uri("a.sh")}})
	v, ds = c.waitPush("a.sh")
	if v != -1 || len(ds) != 0 {
		t.Fatalf("close: version %d, %v", v, ds)
	}
}

func TestDependentsRepublished(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "lib.sh"), "deploy_it() { :; }\nhelper() { :; }\n")
	put(t, filepath.Join(root, "main.sh"), "#!/bin/bash\nsource ./lib.sh\ndeploy_it\n")
	c := client(t, root)
	c.open("main.sh")
	if _, ds := c.waitPush("main.sh"); hasCode(ds, "WB1102") {
		t.Fatalf("main.sh before the edit: %v", ds)
	}
	c.open("lib.sh")
	c.waitPush("lib.sh")
	c.symbols("") // a barrier: every push of the opens has been sent
	if n := c.pending("main.sh"); n != 0 {
		t.Fatalf("opening lib.sh as it is on disk pushed main.sh again (%d)", n)
	}
	c.change("lib.sh", fullText("helper() { :; }\n"))
	if v, _ := c.waitPush("lib.sh"); v != 2 {
		t.Fatalf("lib.sh push version %d", v)
	}
	v, ds := c.waitPush("main.sh")
	if v != 1 || !hasCode(ds, "WB1102") {
		t.Fatalf("main.sh after deleting deploy_it: version %d, %v", v, ds)
	}
	// Restoring it clears the finding again.
	c.change("lib.sh", fullText("deploy_it() { :; }\n"))
	c.waitPush("lib.sh")
	if _, ds := c.waitPush("main.sh"); hasCode(ds, "WB1102") {
		t.Fatalf("main.sh after restoring deploy_it: %v", ds)
	}
}

// ---- pull ----

type pullReport struct {
	Kind     string       `json:"kind"`
	ResultID string       `json:"resultId"`
	Items    []Diagnostic `json:"items"`
}

func (c *testClient) pull(rel, previous string) pullReport {
	c.t.Helper()
	raw := c.call("textDocument/diagnostic", map[string]any{
		"textDocument": map[string]any{"uri": c.uri(rel)}, "previousResultId": previous})
	var r pullReport
	if err := json.Unmarshal(raw, &r); err != nil {
		c.t.Fatalf("%v: %s", err, raw)
	}
	return r
}

func TestPullDiagnostics(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "#!/bin/bash\nrm $f\n")
	put(t, filepath.Join(root, "b.sh"), "#!/bin/bash\nrm $g\n")
	c := client(t, root)
	c.open("a.sh")
	_, pushed := c.waitPush("a.sh")
	full := c.pull("a.sh", "")
	if full.Kind != "full" || full.ResultID == "" || !hasCode(full.Items, "WB1401") || len(full.Items) != len(pushed) {
		t.Fatalf("first pull: %+v (pushed %v)", full, pushed)
	}
	if !regexp.MustCompile(`^[0-9]+\.1$`).MatchString(full.ResultID) {
		t.Fatalf("resultId %q is not <generation>.<version>", full.ResultID)
	}
	same := c.pull("a.sh", full.ResultID)
	if same.Kind != "unchanged" || same.ResultID != full.ResultID || same.Items != nil {
		t.Fatalf("second pull: %+v", same)
	}
	c.change("a.sh", fullText("#!/bin/bash\nrm \"$f\"\n"))
	changed := c.pull("a.sh", full.ResultID)
	if changed.Kind != "full" || changed.ResultID == full.ResultID || hasCode(changed.Items, "WB1401") {
		t.Fatalf("pull after an edit: %+v", changed)
	}
	// A file that is not open is answered from the workspace.
	b := c.pull("b.sh", "")
	if b.Kind != "full" || !hasCode(b.Items, "WB1401") || !strings.HasSuffix(b.ResultID, ".0") {
		t.Fatalf("pull of a file that is not open: %+v", b)
	}
	// A change anywhere in the workspace moves the generation: the resultId
	// of a dependent differs even though its own version did not.
	c.openText("b.sh", "#!/bin/bash\nrm $g\n")
	if after := c.pull("a.sh", changed.ResultID); after.ResultID == changed.ResultID {
		t.Fatalf("resultId did not follow the generation: %+v", after)
	}
}

// ---- code actions ----

type testAction struct {
	Title       string       `json:"title"`
	Kind        string       `json:"kind"`
	Diagnostics []Diagnostic `json:"diagnostics"`
	IsPreferred *bool        `json:"isPreferred"`
	Edit        struct {
		Changes map[string][]TextEdit `json:"changes"`
	} `json:"edit"`
}

func (c *testClient) actions(rel string, r map[string]any, ctx []Diagnostic, only ...string) []testAction {
	c.t.Helper()
	context := map[string]any{"diagnostics": ctx}
	if ctx == nil {
		context["diagnostics"] = []Diagnostic{}
	}
	if only != nil {
		context["only"] = only
	}
	raw := c.call("textDocument/codeAction", map[string]any{
		"textDocument": map[string]any{"uri": c.uri(rel)}, "range": r, "context": context})
	var out []testAction
	if err := json.Unmarshal(raw, &out); err != nil {
		c.t.Fatalf("%v: %s", err, raw)
	}
	if string(raw) == "null" {
		c.t.Fatalf("codeAction answered null, want a list")
	}
	return out
}

func TestQuickFix(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	c := client(t, root)
	c.openText("a.sh", "#!/bin/bash\nrm $f\n")
	_, ds := c.waitPush("a.sh")
	var d Diagnostic
	for _, x := range ds {
		if x.Code == "WB1401" {
			d = x
		}
	}
	if d.Code == "" {
		t.Fatalf("no WB1401 in %v", ds)
	}
	want := TextEdit{Range: d.Range, NewText: `"$f"`}
	check := func(what string, acts []testAction) {
		t.Helper()
		if len(acts) != 1 {
			t.Fatalf("%s: %d actions: %+v", what, len(acts), acts)
		}
		a := acts[0]
		edits := a.Edit.Changes[c.uri("a.sh")]
		if a.Title != "Quote $f" || a.Kind != "quickfix" || len(edits) != 1 || edits[0] != want ||
			len(a.Diagnostics) != 1 || a.Diagnostics[0].Code != "WB1401" {
			t.Fatalf("%s: %+v", what, a)
		}
		// isPreferred is Fix.Safe, and the quote fix is never safe (a script may split on purpose).
		if a.IsPreferred == nil || *a.IsPreferred {
			t.Fatalf("%s: isPreferred = %v, want false (the fix is not Safe)", what, a.IsPreferred)
		}
	}
	check("with the diagnostic", c.actions("a.sh", d.Range.toMap(), []Diagnostic{d}))
	check("range only", c.actions("a.sh", d.Range.toMap(), nil))
	check("cursor in the range", c.actions("a.sh", Range{d.Range.Start, d.Range.Start}.toMap(), nil, "quickfix"))
	if got := c.actions("a.sh", d.Range.toMap(), []Diagnostic{d}, "refactor"); len(got) != 0 {
		t.Fatalf("only refactor: %+v", got)
	}
	if got := c.actions("a.sh", rng(0, 0, 0, 5), nil); len(got) != 0 {
		t.Fatalf("elsewhere: %+v", got)
	}
	other := d
	other.Code = "WB1102"
	if got := c.actions("a.sh", d.Range.toMap(), []Diagnostic{other}); len(got) != 0 {
		t.Fatalf("another code at that range: %+v", got)
	}
	// After an edit the old findings no longer match.
	c.change("a.sh", fullText("#!/bin/bash\n\nrm $f\n"))
	c.waitPush("a.sh")
	if got := c.actions("a.sh", d.Range.toMap(), []Diagnostic{d}); len(got) != 0 {
		t.Fatalf("stale diagnostic matched: %+v", got)
	}
	// A document that is not open has none.
	if got := c.actions("zzz.sh", d.Range.toMap(), nil); len(got) != 0 {
		t.Fatalf("unknown document: %+v", got)
	}
}

func (r Range) toMap() map[string]any {
	return rng(r.Start.Line, r.Start.Character, r.End.Line, r.End.Character)
}

func TestFixActionIsPreferredFollowsSafe(t *testing.T) {
	pub := []pubDiag{{
		diag:  Diagnostic{Code: "X1", Range: Range{Position{1, 0}, Position{1, 3}}},
		fixes: []lspFix{{title: "safe", safe: true, edits: []TextEdit{{NewText: "a"}}}, {title: "unsafe", edits: []TextEdit{{NewText: "b"}}}},
	}}
	got := fixActions("file:///x.sh", pub, nil, Range{Position{1, 1}, Position{1, 1}}, nil)
	if len(got) != 2 || !got[0].IsPreferred || got[1].IsPreferred || got[0].Title != "safe" {
		t.Fatalf("%+v", got)
	}
}

func TestDiskChangeRepublishesOpenDependent(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "lib.sh"), "deploy_it() { :; }\n")
	put(t, filepath.Join(root, "main.sh"), "#!/bin/bash\nsource ./lib.sh\ndeploy_it\n")
	c := client(t, root)
	c.open("main.sh")
	c.waitPush("main.sh")
	put(t, filepath.Join(root, "lib.sh"), "other() { :; }\n")
	c.notify("workspace/didChangeWatchedFiles", map[string]any{"changes": []any{map[string]any{"uri": c.uri("lib.sh"), "type": 2}}})
	if _, ds := c.waitPush("main.sh"); !hasCode(ds, "WB1102") {
		t.Fatalf("after the watched change: %v", ds)
	}
	put(t, filepath.Join(root, "lib.sh"), "deploy_it() { :; }\n")
	c.notify("textDocument/didSave", map[string]any{"textDocument": map[string]any{"uri": c.uri("lib.sh")}})
	if _, ds := c.waitPush("main.sh"); hasCode(ds, "WB1102") {
		t.Fatalf("after the save: %v", ds)
	}
}

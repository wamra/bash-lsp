package lsp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/wamra/bash-lsp/analysis"
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
	deadline := time.AfterFunc(slack(10*time.Second), func() { c.mu.Lock(); c.closed = true; c.cond.Broadcast(); c.mu.Unlock() })
	defer deadline.Stop()
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.diags[uri]) == 0 {
		if c.closed {
			c.t.Fatalf("no diagnostics pushed for %s\n%s", rel, c.stalled())
		}
		c.cond.Wait()
	}
	d, v := c.diags[uri][0], c.vers[uri][0]
	c.diags[uri], c.vers[uri] = c.diags[uri][1:], c.vers[uri][1:]
	return v, d
}

// stalled describes where a session stands when a wait ran out: every push
// received (with its version), the log messages, the server's own log and the
// stacks of all goroutines, which show where a ShellCheck run is stuck. The
// caller holds c.mu.
func (c *testClient) stalled() string {
	stacks := make([]byte, 256<<10)
	stacks = stacks[:runtime.Stack(stacks, true)]
	return fmt.Sprintf("pushes received (%d):\n  %s\nwindow/logMessage: %q\nserver log:\n%s\ngoroutines:\n%s",
		len(c.history), strings.Join(c.history, "\n  "), c.logMsgs, c.logs.String(), stacks)
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

func TestNativeOnOpenAndChange(t *testing.T) { // native findings, versioned, no ShellCheck
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
	check("with the diagnostic", c.actions("a.sh", d.Range.toMap(), []Diagnostic{d}, "quickfix"))
	check("range only", c.actions("a.sh", d.Range.toMap(), nil, "quickfix"))
	check("cursor in the range", c.actions("a.sh", Range{d.Range.Start, d.Range.Start}.toMap(), nil, "quickfix"))
	if got := c.actions("a.sh", d.Range.toMap(), []Diagnostic{d}, "source.organizeImports"); len(got) != 0 {
		t.Fatalf("a kind that is not offered: %+v", got)
	}
	if got := c.actions("a.sh", rng(0, 0, 0, 5), nil); len(got) != 0 {
		t.Fatalf("elsewhere: %+v", got)
	}
	other := d
	other.Code = "WB1102"
	if got := c.actions("a.sh", d.Range.toMap(), []Diagnostic{other}, "quickfix"); len(got) != 0 {
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

// ---- ShellCheck ----

// setDebounce lowers the quiet time before ShellCheck runs; servers started
// after the call use it.
func setDebounce(t *testing.T, d time.Duration) {
	t.Helper()
	old := checkDebounce
	checkDebounce = d
	t.Cleanup(func() { checkDebounce = old })
}

func scInit(path string, timeoutSeconds int) map[string]any {
	o := map[string]any{"path": path}
	if timeoutSeconds > 0 {
		o["timeout_seconds"] = timeoutSeconds
	}
	return map[string]any{"shellcheck": o}
}

func messages(ds []Diagnostic) []string {
	out := []string{}
	for _, d := range ds {
		out = append(out, d.Message)
	}
	return out
}

func anyMessage(ds []Diagnostic, sub string) bool {
	return slices.ContainsFunc(messages(ds), func(m string) bool { return strings.Contains(m, sub) })
}

func (c *testClient) changeTo(rel string, version int, text string) {
	c.notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": c.uri(rel), "version": version},
		"contentChanges": []any{fullText(text)}})
}

// logsContaining returns the window/logMessage texts that contain sub.
func (c *testClient) logsContaining(sub string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, m := range c.logMsgs {
		if strings.Contains(m, sub) {
			out = append(out, m)
		}
	}
	return out
}

func (c *testClient) waitLog(sub string) string {
	c.t.Helper()
	deadline := time.Now().Add(slack(10 * time.Second))
	for time.Now().Before(deadline) {
		if l := c.logsContaining(sub); len(l) > 0 {
			return l[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.t.Fatalf("no window/logMessage containing %q; got %q", sub, c.logMsgs)
	return ""
}

func TestStaleShellCheckDropped(t *testing.T) {
	setDebounce(t, 20*time.Millisecond)
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	c := clientInit(t, root, false, scInit(fakeShellCheck(t, "mark"), 0))
	c.openText("a.sh", "#!/bin/bash\n# MARK v1 SLOW\nrm $f\n")
	if v, ds := c.waitPush("a.sh"); v != 1 || !hasCode(ds, "WB1401") || hasSource(ds, "shellcheck") {
		t.Fatalf("v1 native push: %d %v", v, ds)
	}
	time.Sleep(150 * time.Millisecond) // the v1 run has started and sleeps
	c.changeTo("a.sh", 2, "#!/bin/bash\n# MARK v2\nrm $f\n")
	v, ds := c.waitPush("a.sh")
	if v != 2 || !hasCode(ds, "WB1401") || hasSource(ds, "shellcheck") {
		t.Fatalf("v2 must be published natively first: %d %v", v, ds)
	}
	v, ds = c.waitPush("a.sh")
	if v != 2 || !anyMessage(ds, "mark:# MARK v2") || anyMessage(ds, "v1") || !hasCode(ds, "WB1401") {
		t.Fatalf("v2 with ShellCheck: %d %v", v, messages(ds))
	}
	time.Sleep(700 * time.Millisecond) // long enough for the v1 run to have answered
	if n := c.pending("a.sh"); n != 0 {
		_, late := c.waitPush("a.sh")
		t.Fatalf("a push after the v2 result: %v", messages(late))
	}
}

func TestFinishCheckDropsStaleResults(t *testing.T) {
	var out bytes.Buffer
	s := newServer(strings.NewReader(""), &out, nil, "test")
	d := &document{uri: "file:///x/a.sh", path: "/x/a.sh", version: 3}
	s.docs[d.path] = d
	d.lint.scSeq = 2
	ds := []analysis.Diagnostic{{Rule: "SC1", Message: "late"}}
	for name, c := range map[string]struct{ ver, seq int }{"old version": {2, 2}, "old run": {3, 1}} {
		s.finishCheck(d, c.ver, c.seq, ds, []string{"a note"}, nil, 0)
		if out.Len() != 0 || d.lint.scDone {
			t.Errorf("%s: a stale result was accepted (output %q)", name, out.String())
		}
	}
	delete(s.docs, d.path)
	s.finishCheck(d, 3, 2, ds, nil, nil, 0)
	if out.Len() != 0 || d.lint.scDone {
		t.Errorf("closed document: a result was accepted (output %q)", out.String())
	}
}

func TestShellCheckTimeoutReported(t *testing.T) {
	setDebounce(t, 10*time.Millisecond)
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	c := clientInit(t, root, false, scInit(fakeShellCheck(t, "sleep"), 1))
	c.openText("a.sh", "#!/bin/bash\nrm $f\n")
	c.waitPush("a.sh") // native
	v, ds := c.waitPush("a.sh")
	if v != 1 || !hasCode(ds, "WB1004") || !hasCode(ds, "WB1401") {
		t.Fatalf("after the timeout: %d %v", v, codes(ds))
	}
	for _, d := range ds {
		if d.Code == "WB1004" && (d.Severity != 3 || d.Source != "bash-lsp" || !strings.Contains(d.Message, "did not finish")) {
			t.Fatalf("WB1004: %+v", d)
		}
	}
}

func TestShellCheckOutputCapReported(t *testing.T) {
	setDebounce(t, 10*time.Millisecond)
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	c := clientInit(t, root, false, scInit(fakeShellCheck(t, "flood"), 5))
	c.openText("a.sh", "#!/bin/bash\nrm $f\n")
	c.waitPush("a.sh")
	if _, ds := c.waitPush("a.sh"); !hasCode(ds, "WB1004") || !hasCode(ds, "WB1401") {
		t.Fatalf("after the flood: %v", codes(ds))
	}
}

func TestWB1004HonoursSettings(t *testing.T) {
	setDebounce(t, 10*time.Millisecond)
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	opts := scInit(fakeShellCheck(t, "sleep"), 1)
	opts["diagnostics"] = map[string]any{"disable": []string{"WB1004"}}
	c := clientInit(t, root, false, opts)
	c.openText("a.sh", "#!/bin/bash\nrm $f\n")
	c.waitPush("a.sh")
	time.Sleep(1600 * time.Millisecond)
	if n := c.pending("a.sh"); n != 0 {
		_, ds := c.waitPush("a.sh")
		if hasCode(ds, "WB1004") {
			t.Fatalf("a disabled WB1004 was published: %v", codes(ds))
		}
	}
	opts = scInit(fakeShellCheck(t, "sleep"), 1)
	opts["severity"] = map[string]any{"WB1004": "warning"}
	c2 := clientInit(t, root, false, opts)
	c2.openText("a.sh", "#!/bin/bash\nrm $f\n")
	c2.waitPush("a.sh")
	_, ds := c2.waitPush("a.sh")
	for _, d := range ds {
		if d.Code == "WB1004" && d.Severity != 2 {
			t.Fatalf("severity override not applied: %+v", d)
		}
	}
	if !hasCode(ds, "WB1004") {
		t.Fatalf("no WB1004: %v", codes(ds))
	}
}

func TestServerResponsiveWhileShellCheckHangs(t *testing.T) {
	setDebounce(t, 10*time.Millisecond)
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "f() { :; }\nf\n")
	c := clientInit(t, root, false, scInit(fakeShellCheck(t, "sleep"), 30))
	c.openText("a.sh", "f() { :; }\nf\nrm $x\n")
	c.waitPush("a.sh")
	time.Sleep(100 * time.Millisecond) // the hung run is going
	for i := 2; i < 30; i++ {
		start := time.Now()
		c.changeTo("a.sh", i, fmt.Sprintf("f() { :; }\nf\nrm $x%d\n", i))
		c.waitPush("a.sh") // the native push of this edit
		c.symbols("")
		if d := time.Since(start); d > slack(time.Second) {
			t.Fatalf("edit %d took %v while ShellCheck hung", i, d)
		}
	}
}

func TestShellCheckBurstRunsOnce(t *testing.T) {
	setDebounce(t, time.Second) // long against a loaded machine: the burst must fit well inside it
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	runlog := filepath.Join(t.TempDir(), "runs")
	c := clientInit(t, root, false, scInit(fakeShellCheck(t, "mark", "WB_RUNLOG="+runlog), 0))
	t.Cleanup(func() {
		if t.Failed() {
			b, _ := os.ReadFile(runlog)
			t.Logf("run log of the fake (%s):\n%s", runlog, b)
		}
	})
	c.openText("a.sh", "#!/bin/bash\n# MARK 0\n")
	for i := 2; i <= 12; i++ {
		c.changeTo("a.sh", i, fmt.Sprintf("#!/bin/bash\n# MARK %d\n", i))
	}
	_, ds := c.waitPushWhere("a.sh", func(v int, ds []Diagnostic) bool { return anyMessage(ds, "mark:") })
	if !anyMessage(ds, "mark:# MARK 12") {
		t.Fatalf("the result is not for the last edit: %v", messages(ds))
	}
	time.Sleep(600 * time.Millisecond)
	b, _ := os.ReadFile(runlog)
	if n := strings.Count(string(b), "run"); n != 1 {
		t.Fatalf("ShellCheck ran %d times for one burst of edits", n)
	}
}

func TestShellCheckCancelledOnClose(t *testing.T) {
	setDebounce(t, 10*time.Millisecond)
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	c := clientInit(t, root, false, scInit(fakeShellCheck(t, "mark"), 0))
	c.openText("a.sh", "#!/bin/bash\n# MARK SLOW\n")
	c.waitPush("a.sh")
	time.Sleep(100 * time.Millisecond) // the run is going
	c.notify("textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": c.uri("a.sh")}})
	if v, ds := c.waitPush("a.sh"); v != -1 || len(ds) != 0 {
		t.Fatalf("close: %d %v", v, ds)
	}
	time.Sleep(700 * time.Millisecond)
	if n := c.pending("a.sh"); n != 0 {
		_, ds := c.waitPush("a.sh")
		t.Fatalf("a push for a closed document: %v", messages(ds))
	}
}

func TestShellCheckRefusalAndFailureLogged(t *testing.T) {
	setDebounce(t, 10*time.Millisecond)
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	put(t, filepath.Join(root, "b.sh"), "")
	// A program inside the workspace is refused, once, with the reason.
	inside := filepath.Join(root, "bin", "shellcheck")
	put(t, inside, "#!/bin/sh\n")
	if err := os.Chmod(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	c := clientInit(t, root, false, scInit(inside, 0))
	c.openText("a.sh", "#!/bin/bash\nrm $f\n")
	c.openText("b.sh", "#!/bin/bash\nrm $f\n")
	msg := c.waitLog("inside the workspace folder")
	if !strings.Contains(msg, "shellcheck") {
		t.Fatalf("refusal text: %q", msg)
	}
	c.symbols("")
	if n := len(c.logsContaining("inside the workspace folder")); n != 1 {
		t.Fatalf("the refusal was logged %d times", n)
	}
	c.waitPush("a.sh")
	c.waitPush("b.sh")
	if c.pending("a.sh") != 0 || c.pending("b.sh") != 0 {
		t.Fatalf("a refused ShellCheck pushed again")
	}

	// A run that fails (exit status 2) is logged once and leaves the findings alone.
	root2 := t.TempDir()
	put(t, filepath.Join(root2, "a.sh"), "")
	put(t, filepath.Join(root2, "b.sh"), "")
	c2 := clientInit(t, root2, false, scInit(fakeShellCheck(t, "exit2"), 0))
	c2.openText("a.sh", "#!/bin/bash\nrm $f\n")
	c2.openText("b.sh", "#!/bin/bash\nrm $f\n")
	c2.waitLog("fake failure: first line")
	time.Sleep(300 * time.Millisecond)
	if n := len(c2.logsContaining("fake failure: first line")); n != 1 {
		t.Fatalf("the failure was logged %d times (want once per distinct text)", n)
	}
}

func TestShellCheckNoticesLoggedOnce(t *testing.T) {
	setDebounce(t, 10*time.Millisecond)
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	put(t, filepath.Join(root, ".shellcheckrc"), "this line is not a setting\n")
	c := clientInit(t, root, false, scInit(fakeShellCheck(t, "mark"), 0))
	c.openText("a.sh", "#!/bin/bash\n# MARK 1\n")
	note := c.waitLog(".shellcheckrc:1")
	if !strings.Contains(note, "ShellCheck ignores such a file") {
		t.Fatalf("notice: %q", note)
	}
	c.changeTo("a.sh", 2, "#!/bin/bash\n# MARK 2\n")
	c.waitPushWhere("a.sh", func(v int, ds []Diagnostic) bool { return anyMessage(ds, "mark:# MARK 2") })
	if n := len(c.logsContaining(".shellcheckrc:1")); n != 1 {
		t.Fatalf("the notice was logged %d times", n)
	}
}

// With the real program: its findings replace the native findings they
// duplicate, and its fixes are offered.
func TestRealShellCheckMergedAndFixed(t *testing.T) {
	sc, err := exec.LookPath("shellcheck")
	if err != nil {
		t.Skip("ShellCheck is not installed")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	t.Setenv("XDG_CONFIG_HOME", home)
	setDebounce(t, 10*time.Millisecond)
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	c := clientInit(t, root, false, scInit(sc, 0))
	c.openText("a.sh", "#!/bin/bash\nrm $f\necho `date`\n")
	c.waitPush("a.sh") // native: WB1401
	_, ds := c.waitPushWhere("a.sh", func(v int, ds []Diagnostic) bool { return hasSource(ds, "shellcheck") })
	if !hasCode(ds, "SC2086") || !hasCode(ds, "SC2006") || hasCode(ds, "WB1401") {
		t.Fatalf("merged findings: %v", codes(ds))
	}
	var d Diagnostic
	for _, x := range ds {
		if x.Code == "SC2086" {
			d = x
		}
	}
	acts := c.actions("a.sh", d.Range.toMap(), []Diagnostic{d}, "quickfix")
	if len(acts) != 1 || acts[0].Title != "ShellCheck: apply fix" || len(acts[0].Edit.Changes[c.uri("a.sh")]) == 0 {
		t.Fatalf("ShellCheck fix: %+v", acts)
	}
	// Pull answers with the same merged set and a result id that moved with it.
	r := c.pull("a.sh", "")
	if !hasCode(r.Items, "SC2086") {
		t.Fatalf("pull: %v", codes(r.Items))
	}
}

func TestPullFollowsShellCheckResult(t *testing.T) {
	setDebounce(t, 10*time.Millisecond)
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	c := clientInit(t, root, false, scInit(fakeShellCheck(t, "mark", "WB_X=1"), 0))
	c.openText("a.sh", "#!/bin/bash\n# MARK SLOW\n")
	c.waitPush("a.sh")
	before := c.pull("a.sh", "")
	if anyMessage(before.Items, "mark:") {
		t.Fatalf("pull before ShellCheck answered: %v", messages(before.Items))
	}
	c.waitPushWhere("a.sh", func(v int, ds []Diagnostic) bool { return hasSource(ds, "shellcheck") })
	after := c.pull("a.sh", before.ResultID)
	if after.Kind != "full" || after.ResultID == before.ResultID || !anyMessage(after.Items, "mark:") {
		t.Fatalf("pull after ShellCheck: %+v (before %+v)", after, before)
	}
	if again := c.pull("a.sh", after.ResultID); again.Kind != "unchanged" {
		t.Fatalf("pull again: %+v", again)
	}
}

// ---- settings reload ----

func (c *testClient) watchedPath(path string) {
	c.watched(fileEvent{workspace.URIFromPath(path), 2})
}

// quiet fails if a push for rel arrives within d.
func (c *testClient) quiet(rel string, d time.Duration) {
	c.t.Helper()
	time.Sleep(d)
	c.symbols("") // a request after the wait: everything the server sent has arrived
	if n := c.pending(rel); n != 0 {
		_, ds := c.waitPush(rel)
		c.t.Fatalf("an unexpected push for %s: %v", rel, codes(ds))
	}
}

func TestSettingsReloadRepoFile(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	repo := filepath.Join(root, ".bash-lsp.toml")
	c := client(t, root)
	c.openText("a.sh", "#!/bin/bash\nrm $f\n")
	if _, ds := c.waitPush("a.sh"); !hasCode(ds, "WB1401") {
		t.Fatalf("before: %v", codes(ds))
	}
	put(t, repo, "[diagnostics]\ndisable = [\"WB1401\"]\n")
	c.watchedPath(repo)
	if v, ds := c.waitPush("a.sh"); v != 1 || hasCode(ds, "WB1401") {
		t.Fatalf("after disabling WB1401: %d %v", v, codes(ds))
	}
	// Touching the file without changing it republishes nothing.
	put(t, repo, "[diagnostics]\ndisable = [\"WB1401\"]\n")
	c.watchedPath(repo)
	c.quiet("a.sh", 100*time.Millisecond)
	// A save of the open file counts as well.
	put(t, repo, "[severity]\nWB1401 = \"error\"\n")
	c.notify("textDocument/didSave", map[string]any{"textDocument": map[string]any{"uri": workspace.URIFromPath(repo)}})
	_, ds := c.waitPush("a.sh")
	if len(ds) == 0 || ds[0].Code != "WB1401" || ds[0].Severity != 1 {
		t.Fatalf("after the severity change: %+v", ds)
	}
	// Deleting it restores the defaults.
	if err := os.Remove(repo); err != nil {
		t.Fatal(err)
	}
	c.notify("workspace/didChangeWatchedFiles", map[string]any{"changes": []any{
		map[string]any{"uri": workspace.URIFromPath(repo), "type": 3}}})
	if _, ds := c.waitPush("a.sh"); len(ds) == 0 || ds[0].Severity != 2 {
		t.Fatalf("after deleting the file: %+v", ds)
	}
}

func TestSettingsReloadUserFileAndConfigurationChange(t *testing.T) {
	user := userConfig(t, "")
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	c := client(t, root)
	c.openText("a.sh", "#!/bin/bash\nrm $f\n")
	c.openText("loose.sh", "#!/bin/bash\nrm $f\n") // inside the root too: both are served by the folder
	c.waitPush("a.sh")
	c.waitPush("loose.sh")
	writeUserConfig(t, user, "[diagnostics]\ndisable = [\"WB1401\"]\n")
	c.watchedPath(user) // the user file is outside every folder
	for _, rel := range []string{"a.sh", "loose.sh"} {
		if _, ds := c.waitPush(rel); hasCode(ds, "WB1401") {
			t.Fatalf("%s after the user file change: %v", rel, codes(ds))
		}
	}
	writeUserConfig(t, user, "")
	c.notify("workspace/didChangeConfiguration", map[string]any{"settings": nil})
	for _, rel := range []string{"a.sh", "loose.sh"} {
		if _, ds := c.waitPush(rel); !hasCode(ds, "WB1401") {
			t.Fatalf("%s after didChangeConfiguration: %v", rel, codes(ds))
		}
	}
}

func TestSettingsReloadLooseDocuments(t *testing.T) {
	user := userConfig(t, "")
	c := client(t, "") // no root
	c.openText("l.sh", "#!/bin/bash\nrm $f\n")
	if _, ds := c.waitPush("l.sh"); !hasCode(ds, "WB1401") {
		t.Fatalf("before: %v", codes(ds))
	}
	writeUserConfig(t, user, "[diagnostics]\ndisable = [\"WB1401\"]\n")
	c.watchedPath(user)
	if _, ds := c.waitPush("l.sh"); hasCode(ds, "WB1401") {
		t.Fatalf("after: %v", codes(ds))
	}
}

func TestSettingsReloadFailureKeepsPrevious(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	repo := filepath.Join(root, ".bash-lsp.toml")
	put(t, repo, "[diagnostics]\ndisable = [\"WB1401\"]\n")
	c := client(t, root)
	c.openText("a.sh", "#!/bin/bash\nrm $f\n")
	if _, ds := c.waitPush("a.sh"); hasCode(ds, "WB1401") {
		t.Fatalf("start: %v", codes(ds))
	}
	put(t, repo, "[diagnostics\ndisable = \n")
	c.watchedPath(repo)
	msg := c.waitLog(".bash-lsp.toml")
	if !strings.Contains(msg, "not reloaded") || !strings.Contains(msg, "file ignored") {
		t.Fatalf("log text %q", msg)
	}
	c.quiet("a.sh", 100*time.Millisecond) // the previous settings stay: no push
	// The same broken text is not logged again, and a good file is picked up.
	c.watchedPath(repo)
	c.symbols("")
	if n := len(c.logsContaining("not reloaded")); n != 1 {
		t.Fatalf("logged %d times", n)
	}
	put(t, repo, "")
	c.watchedPath(repo)
	if _, ds := c.waitPush("a.sh"); !hasCode(ds, "WB1401") {
		t.Fatalf("after fixing the file: %v", codes(ds))
	}
}

func TestSettingsReloadShellCheck(t *testing.T) {
	setDebounce(t, 10*time.Millisecond)
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	repo := filepath.Join(root, ".bash-lsp.toml")
	runlog := filepath.Join(t.TempDir(), "runs")
	c := clientInit(t, root, false, scInit(fakeShellCheck(t, "mark", "WB_RUNLOG="+runlog), 0))
	c.openText("a.sh", "#!/bin/bash\n# MARK 1\nrm $f\n")
	c.waitPushWhere("a.sh", func(v int, ds []Diagnostic) bool { return anyMessage(ds, "mark:") })
	// An unrelated change keeps the ShellCheck findings: no flicker, no new run.
	put(t, repo, "[diagnostics]\ndisable = [\"WB1102\"]\n")
	c.watchedPath(repo)
	c.quiet("a.sh", 300*time.Millisecond)
	if b, _ := os.ReadFile(runlog); strings.Count(string(b), "run") != 1 {
		t.Fatalf("ShellCheck ran again after an unrelated change: %q", b)
	}
	// Turning ShellCheck off drops its findings and stops the runs.
	put(t, repo, "[shellcheck]\nenabled = false\n")
	c.watchedPath(repo)
	if _, ds := c.waitPush("a.sh"); hasSource(ds, "shellcheck") || !hasCode(ds, "WB1401") {
		t.Fatalf("after disabling: %v", codes(ds))
	}
	c.changeTo("a.sh", 2, "#!/bin/bash\n# MARK 2\nrm $f\n")
	c.waitPush("a.sh")
	c.quiet("a.sh", 300*time.Millisecond)
	if b, _ := os.ReadFile(runlog); strings.Count(string(b), "run") != 1 {
		t.Fatalf("ShellCheck ran while disabled: %q", b)
	}
	// Turning it on again runs it for the open document.
	put(t, repo, "")
	c.watchedPath(repo)
	_, ds := c.waitPushWhere("a.sh", func(v int, ds []Diagnostic) bool { return anyMessage(ds, "mark:") })
	if !anyMessage(ds, "mark:# MARK 2") {
		t.Fatalf("after enabling: %v", messages(ds))
	}
}

func TestSettingsReloadFormatSettings(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	repo := filepath.Join(root, ".bash-lsp.toml")
	src := "f() {\necho a\n}\n"
	c := client(t, root)
	c.openText("a.sh", src)
	c.waitPush("a.sh")
	if got := apply(t, src, c.formatting("a.sh", nil)); got != "f() {\n\techo a\n}\n" {
		t.Fatalf("before: %q", got)
	}
	put(t, repo, "[format]\nindent = 2\n")
	c.watchedPath(repo)
	c.symbols("") // the reload ran
	if got := apply(t, src, c.formatting("a.sh", nil)); got != "f() {\n  echo a\n}\n" {
		t.Fatalf("after: %q", got)
	}
}

func TestSettingsReloadNamesWhatNeedsARestart(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	repo := filepath.Join(root, ".bash-lsp.toml")
	c := client(t, root)
	c.openText("a.sh", "echo a\n")
	c.waitPush("a.sh")
	put(t, repo, "[files]\nexclude = [\"vendor/\"]\n")
	c.watchedPath(repo)
	msg := c.waitLog("restart")
	if !strings.Contains(msg, "files.exclude") {
		t.Fatalf("log text %q", msg)
	}
}

func TestPullAfterSettingsReloadIsNotUnchanged(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "#!/bin/bash\nrm $f\n")
	put(t, filepath.Join(root, "b.sh"), "#!/bin/bash\nrm $g\n")
	repo := filepath.Join(root, ".bash-lsp.toml")
	c := client(t, root)
	c.openText("a.sh", "#!/bin/bash\nrm $f\n")
	c.waitPush("a.sh")
	open, closed := c.pull("a.sh", ""), c.pull("b.sh", "")
	if !hasCode(open.Items, "WB1401") || !hasCode(closed.Items, "WB1401") {
		t.Fatalf("before: %v %v", codes(open.Items), codes(closed.Items))
	}
	put(t, repo, "[diagnostics]\ndisable = [\"WB1401\"]\n")
	c.watchedPath(repo)
	c.waitPush("a.sh")
	for name, prev := range map[string]struct {
		rel string
		id  string
	}{"open": {"a.sh", open.ResultID}, "not open": {"b.sh", closed.ResultID}} {
		r := c.pull(prev.rel, prev.id)
		if r.Kind != "full" || hasCode(r.Items, "WB1401") || r.ResultID == prev.id {
			t.Fatalf("%s document, pull with the id from before the reload: %+v", name, r)
		}
	}
}

func TestPullDoesNotHideAPush(t *testing.T) {
	// A pull recomputes the findings of a document whose backend moved to a new
	// generation without sending them; the next refresh must still push them.
	var out bytes.Buffer
	s := newServer(strings.NewReader(""), &out, nil, "test")
	s.loose = newDocsOnly()
	main, lib := absPath("/x/main.sh"), absPath("/x/lib.sh")
	s.loose.SetOverlay(lib, 1, []byte("deploy_it() { :; }\n"))
	text := []byte("#!/bin/bash\nsource ./lib.sh\ndeploy_it\n")
	s.loose.SetOverlay(main, 1, text)
	d := &document{uri: workspace.URIFromPath(main), path: main, be: s.loose, version: 1, text: text}
	s.docs[main] = d
	s.publishDoc(d, true)
	if strings.Contains(out.String(), "WB1101") {
		t.Fatalf("main.sh before: %s", out.String())
	}
	out.Reset()
	s.loose.CloseOverlay(lib)                                                                                           // a new generation: the source no longer resolves
	if pub := s.findings(d, true); !slices.ContainsFunc(pub, func(p pubDiag) bool { return p.diag.Code == "WB1101" }) { // what a pull does
		t.Fatalf("the pull did not see the change: %v", pub)
	}
	s.refreshOthers(s.loose, nil)
	if !strings.Contains(out.String(), "WB1101") {
		t.Fatalf("a document the client was never sent the new findings of was skipped: %q", out.String())
	}
}

func TestGitignoreSaveRepublishesDependents(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "lib.sh"), "deploy_it() { :; }\n")
	put(t, filepath.Join(root, "main.sh"), "#!/bin/bash\nsource ./lib.sh\ndeploy_it\n")
	c := client(t, root)
	c.open("main.sh")
	c.waitPush("main.sh")
	put(t, filepath.Join(root, ".gitignore"), "lib.sh\n")
	c.notify("textDocument/didSave", map[string]any{"textDocument": map[string]any{"uri": c.uri(".gitignore")}})
	if _, ds := c.waitPushWhere("main.sh", func(v int, ds []Diagnostic) bool { return hasCode(ds, "WB1102") || hasCode(ds, "WB1101") }); len(ds) == 0 {
		t.Fatal("no push after the .gitignore save")
	}
}

func TestReloadAppliesWhileAnotherFileStaysBroken(t *testing.T) {
	// The user file is broken from the start (ShellCheck is switched off through the
	// initialization options so that no real program runs). A valid edit of the repo
	// file is still applied, and nothing says "not reloaded".
	user := filepath.Join(t.TempDir(), "config.toml")
	put(t, user, "[diagnostics\n")
	t.Setenv("BASH_LSP_CONFIG", user)
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	repo := filepath.Join(root, ".bash-lsp.toml")
	c := clientInit(t, root, false, map[string]any{"shellcheck": map[string]any{"enabled": false}})
	c.openText("a.sh", "#!/bin/bash\nrm $f\n")
	if _, ds := c.waitPush("a.sh"); !hasCode(ds, "WB1401") {
		t.Fatalf("start: %v", codes(ds))
	}
	put(t, repo, "[diagnostics]\ndisable = [\"WB1401\"]\n")
	c.watchedPath(repo)
	if _, ds := c.waitPush("a.sh"); hasCode(ds, "WB1401") {
		t.Fatalf("a valid repo edit was not applied: %v", codes(ds))
	}
	if n := len(c.logsContaining("not reloaded")); n != 0 {
		t.Fatalf("an old failure was reported as a failed reload (%d)", n)
	}
	// A new failure still keeps the settings.
	put(t, user, "[diagnostics]\ndisable = \n")
	c.watchedPath(user)
	c.waitLog("not reloaded")
}

func TestRepoFileRefusalIsReportedToTheUser(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	put(t, filepath.Join(root, ".bash-lsp.toml"), "[shellcheck]\npath = \"/tmp/evil\"\n")
	c := client(t, root)
	c.openText("a.sh", "echo a\n")
	msg := c.waitLog("shellcheck.path")
	if !strings.Contains(msg, "accepted only from user settings") {
		t.Fatalf("log text %q", msg)
	}
	c.symbols("")
	if n := len(c.logsContaining("shellcheck.path")); n != 1 {
		t.Fatalf("reported %d times", n)
	}
}

// ---- pulls across backend changes ----

// A pull answered under one backend must not be `unchanged` under another: a
// loose document that moves into a folder (with its own settings) and a
// document whose folder set changed.
func TestPullAfterBackendChangeIsNotUnchanged(t *testing.T) {
	t.Run("loose document moves into a folder", func(t *testing.T) {
		base, _ := filepath.EvalSymlinks(t.TempDir())
		a := filepath.Join(base, "A")
		put(t, filepath.Join(a, "a.sh"), "#!/bin/bash\nrm $f\n")
		put(t, filepath.Join(a, ".bash-lsp.toml"), "[diagnostics]\ndisable = [\"WB1401\"]\n")
		put(t, filepath.Join(base, "other", "x.sh"), "")
		c := clientWith(t, clientCfg{folders: []string{filepath.Join(base, "other")}, workspaceCap: map[string]any{"workspaceFolders": true}})
		c.root = a
		c.open("a.sh")
		c.waitPush("a.sh")
		p1 := c.pull("a.sh", "")
		if !hasCode(p1.Items, "WB1401") {
			t.Fatalf("before: %v", codes(p1.Items))
		}
		c.folders([]string{a}, nil)
		c.symbols("") // answered once the folder job has been applied
		p2 := c.pull("a.sh", p1.ResultID)
		if p2.Kind != "full" || hasCode(p2.Items, "WB1401") || p2.ResultID == p1.ResultID {
			t.Fatalf("pull with the id issued under the loose backend: %+v", p2)
		}
	})
	t.Run("a folder is added", func(t *testing.T) {
		base, _ := filepath.EvalSymlinks(t.TempDir())
		a, b := filepath.Join(base, "A"), filepath.Join(base, "B")
		put(t, filepath.Join(b, "lib.sh"), "#!/bin/bash\nlibf() { :; }\n")
		put(t, filepath.Join(a, "a.sh"), "#!/bin/bash\nsource "+filepath.Join(b, "lib.sh")+"\nlibf\n")
		c := clientWith(t, clientCfg{folders: []string{a}, workspaceCap: map[string]any{"workspaceFolders": true}})
		c.root = a
		c.open("a.sh")
		c.waitPush("a.sh")
		p1 := c.pull("a.sh", "")
		c.folders([]string{b}, nil)
		c.symbols("")
		if p2 := c.pull("a.sh", p1.ResultID); p2.Kind != "full" || p2.ResultID == p1.ResultID {
			t.Fatalf("pull with the id issued before the folder set changed: %+v", p2)
		}
	})
}

// WB1004 is built when the findings are merged, so a reload that disables it or
// changes its severity takes effect on a result that is already in.
func TestWB1004FollowsSettingsReload(t *testing.T) {
	setDebounce(t, 10*time.Millisecond)
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	repo := filepath.Join(root, ".bash-lsp.toml")
	c := clientInit(t, root, false, scInit(fakeShellCheck(t, "sleep"), 1))
	c.openText("a.sh", "#!/bin/bash\nrm $f\n")
	c.waitPush("a.sh")
	c.waitPushWhere("a.sh", func(v int, ds []Diagnostic) bool { return hasCode(ds, "WB1004") })
	put(t, repo, "[severity]\nWB1004 = \"warning\"\n")
	c.watchedPath(repo)
	_, ds := c.waitPushWhere("a.sh", func(v int, ds []Diagnostic) bool {
		return slices.ContainsFunc(ds, func(d Diagnostic) bool { return d.Code == "WB1004" && d.Severity == 2 })
	})
	if !hasCode(ds, "WB1401") {
		t.Fatalf("the native findings are gone: %v", codes(ds))
	}
	put(t, repo, "[diagnostics]\ndisable = [\"WB1004\"]\n")
	c.watchedPath(repo)
	c.waitPushWhere("a.sh", func(v int, ds []Diagnostic) bool { return !hasCode(ds, "WB1004") && hasCode(ds, "WB1401") })
	if r := c.pull("a.sh", ""); hasCode(r.Items, "WB1004") {
		t.Fatalf("pull still has WB1004: %v", codes(r.Items))
	}
	put(t, repo, "")
	c.watchedPath(repo)
	c.waitPushWhere("a.sh", func(v int, ds []Diagnostic) bool { return hasCode(ds, "WB1004") })
}

func startedPIDs(t *testing.T, runlog string) []int {
	t.Helper()
	b, _ := os.ReadFile(runlog)
	var pids []int
	for _, l := range strings.Split(string(b), "\n") {
		var pid int
		if _, err := fmt.Sscanf(l, "start %d", &pid); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

func waitPIDs(t *testing.T, runlog string, n int) []int {
	t.Helper()
	deadline := time.Now().Add(slack(10 * time.Second))
	for time.Now().Before(deadline) {
		if p := startedPIDs(t, runlog); len(p) >= n {
			return p
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the fake ShellCheck started %d times, want %d", len(startedPIDs(t, runlog)), n)
	return nil
}

func waitGone(t *testing.T, pid int, why string) {
	t.Helper()
	deadline := time.Now().Add(slack(10 * time.Second))
	for time.Now().Before(deadline) {
		p, err := os.FindProcess(pid)
		if err != nil || p.Signal(syscall.Signal(0)) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the ShellCheck child %d is still running %s", pid, why)
}

// A run in flight is killed (the child process, not only the result dropped)
// when a newer version is scheduled and when the document closes.
func TestShellCheckChildKilledOnNewerVersionAndClose(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake ShellCheck is a shell script")
	}
	setDebounce(t, 10*time.Millisecond)
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	runlog := filepath.Join(t.TempDir(), "runs")
	c := clientInit(t, root, false, scInit(fakeShellCheck(t, "hang", "WB_RUNLOG="+runlog), 60))
	c.openText("a.sh", "#!/bin/bash\necho 1\n")
	pid1 := waitPIDs(t, runlog, 1)[0]
	c.changeTo("a.sh", 2, "#!/bin/bash\necho 2\n")
	pid2 := waitPIDs(t, runlog, 2)[1]
	waitGone(t, pid1, "after a newer version was scheduled")
	c.closeDoc("a.sh")
	waitGone(t, pid2, "after the document was closed")
}

// A burst of edits inside the quiet time starts ShellCheck once.
func TestShellCheckBurstStartsOneProcess(t *testing.T) {
	setDebounce(t, 2*time.Second)
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	runlog := filepath.Join(t.TempDir(), "runs")
	c := clientInit(t, root, false, scInit(fakeShellCheck(t, "mark", "WB_RUNLOG="+runlog), 0))
	c.openText("a.sh", "#!/bin/bash\n# MARK 1\n")
	for i := 2; i <= 12; i++ {
		time.Sleep(40 * time.Millisecond) // long enough for a process started by an edit to show itself
		c.changeTo("a.sh", i, fmt.Sprintf("#!/bin/bash\n# MARK %d\n", i))
	}
	if b, _ := os.ReadFile(runlog); len(b) != 0 {
		t.Fatalf("ShellCheck started inside the quiet time: %q", b)
	}
	c.waitPushWhere("a.sh", func(v int, ds []Diagnostic) bool { return anyMessage(ds, "mark:# MARK 12") })
	if b, _ := os.ReadFile(runlog); strings.Count(string(b), "start") != 1 {
		t.Fatalf("ShellCheck started more than once for a burst: %q", b)
	}
}

// merged keeps the ShellCheck findings and drops the native findings they
// duplicate (analysis.Merge), without a real ShellCheck.
func TestMergedDropsDuplicatedNativeFindings(t *testing.T) {
	var out bytes.Buffer
	s := newServer(strings.NewReader(""), &out, nil, "test")
	s.loose = newDocsOnly()
	path := "/x/a.sh"
	text := []byte("#!/bin/bash\nrm $f\n")
	s.loose.SetOverlay(path, 1, text)
	d := &document{uri: "file:///x/a.sh", path: path, be: s.loose, version: 1, text: text}
	s.docs[path] = d
	s.publishDoc(d, true)
	l := &d.lint
	i := slices.IndexFunc(l.native, func(x analysis.Diagnostic) bool { return x.Rule == "WB1401" })
	if i < 0 {
		t.Fatalf("no native WB1401: %v", l.native)
	}
	sc := analysis.Diagnostic{Rule: "SC2086", Severity: analysis.SevInfo, Origin: "shellcheck", Range: l.native[i].Range, Message: "quote it"}
	l.sc, l.scDone, l.scVer = []analysis.Diagnostic{sc}, true, 1
	got := codes(diagnosticsOf(s.findings(d, true)))
	if !slices.Contains(got, "SC2086") || slices.Contains(got, "WB1401") {
		t.Fatalf("merged findings: %v", got)
	}
	l.scVer = 0 // findings kept for another version are not merged
	if got := codes(diagnosticsOf(s.findings(d, true))); !slices.Contains(got, "WB1401") || slices.Contains(got, "SC2086") {
		t.Fatalf("an older version's ShellCheck findings were merged: %v", got)
	}
}

// A repo settings file is recognised by its name where the volume folds case,
// and a name that cannot match never reaches the disk.
func TestIsSettingsFileFoldsLikeTheVolume(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	var srv *server
	c := clientWith(t, clientCfg{root: root, onServer: func(s *server) { srv = s }})
	c.open("a.sh")
	c.symbols("")
	var fold, plain, variant, other bool
	done := make(chan struct{})
	srv.post(func() {
		fold = srv.folders[0].ws.CaseInsensitive()
		plain = srv.isSettingsFile(filepath.Join(root, ".bash-lsp.toml"))
		variant = srv.isSettingsFile(filepath.Join(root, ".bash-lsp.TOML"))
		other = srv.isSettingsFile(filepath.Join(root, "notes.toml"))
		close(done)
	})
	<-done
	if !plain || other || variant != fold {
		t.Fatalf("fold %v: .bash-lsp.toml %v, case variant %v, other name %v", fold, plain, variant, other)
	}
}

// The message for a settings file that cannot be read is sent again when the
// file was valid in between and breaks again with the same text.
func TestReloadFailureReportedAgainAfterRecovery(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	repo := filepath.Join(root, ".bash-lsp.toml")
	c := client(t, root)
	c.openText("a.sh", "#!/bin/bash\nrm $f\n")
	c.waitPush("a.sh")
	put(t, repo, "[diagnostics\n")
	c.watchedPath(repo)
	c.waitLogCount("not reloaded", 1)
	put(t, repo, "[diagnostics]\ndisable = [\"WB1401\"]\n")
	c.watchedPath(repo)
	c.waitPushWhere("a.sh", func(v int, ds []Diagnostic) bool { return !hasCode(ds, "WB1401") })
	put(t, repo, "[diagnostics\n")
	c.watchedPath(repo)
	c.waitLogCount("not reloaded", 2)
}

// Text that goes out to the editor is redacted: a credential-looking value in
// a .shellcheckrc or a settings file is not echoed in window/logMessage or
// window/showMessage.
func TestOutwardMessagesAreRedacted(t *testing.T) {
	const secret = "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB"
	setDebounce(t, 10*time.Millisecond)
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	put(t, filepath.Join(root, ".shellcheckrc"), "enable="+secret+"\n")
	put(t, filepath.Join(root, ".bash-lsp.toml"), "[diagnostics]\ndisable = [\"token=abcdef0123456789\"]\n")
	c := clientInit(t, root, false, scInit(fakeShellCheck(t, "mark"), 0))
	c.openText("a.sh", "#!/bin/bash\n# MARK 1\n")
	note := c.waitLog("enable entry")
	if strings.Contains(note, secret) || !strings.Contains(note, "[redacted]") {
		t.Fatalf("logMessage: %q", note)
	}
	c.waitLog("enable entry")
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		n := len(c.shown)
		c.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.shown) == 0 {
		t.Fatal("no window/showMessage")
	}
	for _, m := range c.shown {
		if strings.Contains(string(m), "abcdef0123456789") {
			t.Fatalf("showMessage: %s", m)
		}
	}
	for _, m := range c.logMsgs {
		if strings.Contains(m, secret) || strings.Contains(m, "abcdef0123456789") {
			t.Fatalf("logMessage: %q", m)
		}
	}
}

// A text with old-Mac line breaks puts the "did not finish" finding on its
// first line, not across the whole file.
func TestShellCheckTimeoutRangeStopsAtLoneCR(t *testing.T) {
	setDebounce(t, 10*time.Millisecond)
	root := t.TempDir()
	put(t, filepath.Join(root, "a.sh"), "")
	c := clientInit(t, root, false, scInit(fakeShellCheck(t, "sleep"), 1))
	c.openText("a.sh", "#!/bin/bash\rrm $f\r")
	c.waitPush("a.sh")
	_, ds := c.waitPush("a.sh")
	for _, d := range ds {
		if d.Code == "WB1004" {
			if d.Range.End.Line != 0 || d.Range.End.Character != len("#!/bin/bash") {
				t.Fatalf("WB1004 range %+v, want to end at line 0 column %d", d.Range, len("#!/bin/bash"))
			}
			return
		}
	}
	t.Fatalf("no WB1004 in %v", codes(ds))
}

// When Serve returns, the ShellCheck run in flight has been killed and
// reaped: the server does not leave it behind for the process to exit around.
func TestShellCheckChildGoneWhenServeReturns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("probing a process with signal 0 is Unix only")
	}
	setDebounce(t, 10*time.Millisecond)
	for i := 0; i < 20; i++ {
		root := t.TempDir()
		put(t, filepath.Join(root, "a.sh"), "")
		runlog := filepath.Join(t.TempDir(), "runs")
		c := clientInit(t, root, false, scInit(fakeShellCheck(t, "hang", "WB_RUNLOG="+runlog), 60))
		c.openText("a.sh", "#!/bin/bash\necho 1\n")
		pid := waitPIDs(t, runlog, 1)[0]
		c.in.Close()
		select {
		case <-c.done:
		case <-time.After(10 * time.Second):
			t.Fatal("Serve did not return")
		}
		if p, err := os.FindProcess(pid); err == nil && p.Signal(syscall.Signal(0)) == nil {
			t.Fatalf("round %d: the ShellCheck child %d is still running after Serve returned", i, pid)
		}
	}
}

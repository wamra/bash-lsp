package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/wamra/bash-lsp/internal/extool"
)

type compItem struct {
	Label         string          `json:"label"`
	Kind          int             `json:"kind"`
	Detail        string          `json:"detail"`
	SortText      string          `json:"sortText"`
	Documentation *markupContent  `json:"documentation"`
	TextEdit      *TextEdit       `json:"textEdit"`
	Data          json.RawMessage `json:"data"`
}

type compList struct {
	IsIncomplete bool       `json:"isIncomplete"`
	Items        []compItem `json:"items"`
}

func (l compList) find(label string) (compItem, bool) {
	for _, it := range l.Items {
		if it.Label == label {
			return it, true
		}
	}
	return compItem{}, false
}

func markPos(src string) (string, Position) {
	before, after, _ := strings.Cut(src, "▮")
	col := before[strings.LastIndex(before, "\n")+1:]
	return before + after, Position{Line: strings.Count(before, "\n"), Character: len(utf16.Encode([]rune(col)))}
}

// completeAt opens src (cursor = the ▮ mark, removed) as rel and completes there.
func (c *testClient) completeAt(rel, src string) (compList, json.RawMessage) {
	c.t.Helper()
	text, p := markPos(src)
	c.openText(rel, text)
	return c.complete(rel, p)
}

func (c *testClient) complete(rel string, p Position) (compList, json.RawMessage) {
	c.t.Helper()
	raw := c.call("textDocument/completion", map[string]any{"textDocument": map[string]any{"uri": c.uri(rel)}, "position": p})
	var l compList
	if string(raw) != "null" {
		if err := json.Unmarshal(raw, &l); err != nil {
			c.t.Fatalf("%v: %s", err, raw)
		}
	}
	return l, raw
}

func TestCompletionCapability(t *testing.T) {
	got, _ := json.Marshal(capabilities()["completionProvider"])
	if string(got) != `{"resolveProvider":true,"triggerCharacters":["$","{","-","/"]}` {
		t.Fatalf("completionProvider = %s", got)
	}
}

func TestCompletionItemsOnTheWire(t *testing.T) {
	c := client(t, t.TempDir())
	l, raw := c.completeAt("a.sh", "g=1; f() { local a; echo $▮; }; h() { local b; }")
	if l.IsIncomplete || !strings.Contains(string(raw), `"items":[`) {
		t.Fatalf("got %s", raw)
	}
	a, ok := l.find("a")
	if !ok || a.Kind != 6 {
		t.Fatalf("a: %s", raw)
	}
	if _, ok := l.find("g"); !ok {
		t.Fatalf("g: %s", raw)
	}
	if _, ok := l.find("b"); ok {
		t.Fatalf("b offered: %s", raw)
	}
	var data struct{ URI, Label, Kind string }
	if err := json.Unmarshal(a.Data, &data); err != nil || data.Label != "a" || data.URI != c.uri("a.sh") || data.Kind != "variable" {
		t.Fatalf("data %s", a.Data)
	}
	if a.TextEdit == nil || a.TextEdit.NewText != "a" {
		t.Fatalf("textEdit %+v", a.TextEdit)
	}
}

func TestCompletionKinds(t *testing.T) {
	c := client(t, t.TempDir())
	l, raw := c.completeAt("a.sh", "alias ll='ls -l'\nmine() { :; }\n▮")
	want := map[string]int{"ll": 9, "mine": 3, "printf": 21, "while": 14}
	for label, kind := range want {
		if it, ok := l.find(label); !ok || it.Kind != kind {
			t.Errorf("%s: %+v %v", label, it, ok)
		}
	}
	if len(raw) == 0 {
		t.Fatal("empty")
	}
}

func TestCompletionUTF16(t *testing.T) {
	c := client(t, t.TempDir())
	// The emoji is two UTF-16 units; the name starts after it.
	src := "name=1; echo \"\U0001F600\" $na▮"
	l, raw := c.completeAt("a.sh", src)
	it, ok := l.find("name")
	if !ok || it.TextEdit == nil {
		t.Fatalf("got %s", raw)
	}
	before, _, _ := strings.Cut(src, "▮")
	end := len(utf16.Encode([]rune(before)))
	r := it.TextEdit.Range
	if r.Start.Line != 0 || r.Start.Character != end-2 || r.End.Character != end {
		t.Fatalf("range %+v, want %d..%d", r, end-2, end)
	}
}

func TestCompletionParamOpsFollowTheTarget(t *testing.T) {
	c := client(t, t.TempDir())
	l, raw := c.completeAt("a.sh", "echo ${x▮")
	up, ok := l.find("^^")
	if !ok || up.Detail != "needs bash 4.0; target starts at 3.2" || up.Kind != 24 {
		t.Fatalf("got %s", raw)
	}
	plain, _ := l.find(":-")
	if strings.Contains(plain.Detail, "needs bash") || plain.SortText >= up.SortText {
		t.Fatalf("plain %+v gated %+v", plain, up)
	}
	if q, _ := l.find("@Q"); q.Detail != "needs bash 4.4; target starts at 3.2" || q.SortText <= plain.SortText {
		t.Fatalf("@Q %+v", q)
	}
	c2 := clientWith(t, clientCfg{root: t.TempDir(), initOpts: map[string]any{"bash": map[string]any{"target": "4.1-5.3"}}})
	l, raw = c2.completeAt("a.sh", "echo ${x▮")
	up, _ = l.find("^^")
	q, _ := l.find("@Q")
	if strings.Contains(up.Detail, "needs bash") || q.Detail != "needs bash 4.4; target starts at 4.1" {
		t.Fatalf("target 4.1: %s", raw)
	}
	if up.SortText >= q.SortText {
		t.Fatalf("a gated item must sort after an available one: %+v %+v", up, q)
	}
}

func TestCompletionSourcePath(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "lib.sh"), "libfn() { :; }\n")
	put(t, filepath.Join(root, "notes.txt"), "plain\n")
	put(t, filepath.Join(root, "lib", "inner.sh"), "x=1\n")
	c := client(t, root)
	l, raw := c.completeAt("main.sh", "source ./l▮")
	if it, ok := l.find("lib.sh"); !ok || it.Kind != 17 {
		t.Fatalf("lib.sh: %s", raw)
	}
	if it, ok := l.find("lib/"); !ok || it.Kind != 19 {
		t.Fatalf("lib/: %s", raw)
	}
	if _, ok := l.find("notes.txt"); ok {
		t.Fatalf("notes.txt offered: %s", raw)
	}
}

func TestCompletionIncomplete(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 1200; i++ {
		fmt.Fprintf(&sb, "fn%04d() { :; }\n", i)
	}
	sb.WriteString("▮")
	c := client(t, t.TempDir())
	l, raw := c.completeAt("a.sh", sb.String())
	if !l.IsIncomplete || len(l.Items) != 1000 || !strings.Contains(string(raw)[:30], `"isIncomplete":true`) {
		t.Fatalf("incomplete %v, %d items", l.IsIncomplete, len(l.Items))
	}
}

func progDir(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestCompletionPathCommands(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("execute bits")
	}
	dir := progDir(t, "zzprog", "zzother")
	opts := map[string]any{"path": map[string]any{"profile": []string{dir}}}
	c := clientWith(t, clientCfg{root: t.TempDir(), initOpts: opts})
	l, raw := c.completeAt("a.sh", "zzp▮")
	it, ok := l.find("zzprog")
	if !ok || it.Kind != 3 || it.TextEdit == nil || it.TextEdit.Range.Start.Character != 0 || it.TextEdit.Range.End.Character != 3 {
		t.Fatalf("got %s", raw)
	}
	if _, ok := l.find("zzother"); ok {
		t.Fatalf("prefix ignored: %s", raw)
	}
	var data struct{ Kind string }
	json.Unmarshal(it.Data, &data)
	if data.Kind != "command" {
		t.Fatalf("data %s", it.Data)
	}
	// A function of the same name shadows the program: one item.
	l, _ = c.completeAt("b.sh", "zzprog() { :; }\nzzp▮")
	n := 0
	for _, x := range l.Items {
		if x.Label == "zzprog" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d items named zzprog", n)
	}
	off := clientWith(t, clientCfg{root: t.TempDir(), initOpts: map[string]any{
		"path": opts["path"], "completion": map[string]any{"path_commands": false}}})
	l, raw = off.completeAt("a.sh", "zzp▮")
	if _, ok := l.find("zzprog"); ok {
		t.Fatalf("path_commands=false still lists: %s", raw)
	}
}

func TestCompletionPathCommandsCapAndIncomplete(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("execute bits")
	}
	var names []string
	for i := 0; i < 1500; i++ {
		names = append(names, fmt.Sprintf("qq%04d", i))
	}
	dir := progDir(t, names...)
	c := clientWith(t, clientCfg{root: t.TempDir(), initOpts: map[string]any{"path": map[string]any{"profile": []string{dir}}}})
	l, raw := c.completeAt("a.sh", "f() { :; }\nqq▮")
	if !l.IsIncomplete || len(l.Items) != 1000 {
		t.Fatalf("incomplete %v, %d items: %.80s", l.IsIncomplete, len(l.Items), raw)
	}
}

// helpRunner counts runs and answers `help printf` and `help grep`-like text.
func countingRunner(calls *atomic.Int32) func(context.Context, extool.Tool, []string, extool.RunOptions) (extool.Result, error) {
	return func(ctx context.Context, tl extool.Tool, args []string, o extool.RunOptions) (extool.Result, error) {
		calls.Add(1)
		return extool.Result{Stdout: []byte(helpSample)}, nil
	}
}

func (c *testClient) resolve(item map[string]any) (*rpcReply, map[string]any) {
	c.t.Helper()
	r := c.rawCall("completionItem/resolve", item)
	var out map[string]any
	if r.Error == nil {
		if err := json.Unmarshal(r.Result, &out); err != nil {
			c.t.Fatalf("%v: %s", err, r.Result)
		}
	}
	return r, out
}

func rawItem(t *testing.T, l compList, raw json.RawMessage, label string) map[string]any {
	t.Helper()
	var generic struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	for _, it := range generic.Items {
		if it["label"] == label {
			return it
		}
	}
	t.Fatalf("no item %q in %s", label, raw)
	return nil
}

func TestCompletionNeverSpawns(t *testing.T) {
	root := t.TempDir()
	var calls atomic.Int32
	c := clientWith(t, clientCfg{root: root, initOpts: helpInitOpts(t, nil), onServer: func(s *server) { s.helpRun = countingRunner(&calls) }})
	for _, src := range []string{"▮", "printf -▮", "echo $▮", "echo ${x▮", "source ./▮", "echo hi; pri▮"} {
		l, raw := c.completeAt("a.sh", src)
		_ = l
		_ = raw
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("%d runs on completion", n)
	}
	l, raw := c.completeAt("a.sh", "pri▮")
	item := rawItem(t, l, raw, "printf")
	r, out := c.resolve(item)
	if r.Error != nil {
		t.Fatalf("resolve: %+v", r.Error)
	}
	doc, _ := out["documentation"].(map[string]any)
	if doc == nil || !strings.Contains(fmt.Sprint(doc["value"]), "printf [-v var] format [arguments]") || doc["kind"] != "markdown" {
		t.Fatalf("resolve: %s", r.Result)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("%d runs on resolve, want 1", n)
	}
	// The label and the edit survive.
	if out["label"] != "printf" || out["textEdit"] == nil {
		t.Fatalf("resolve dropped fields: %s", r.Result)
	}
	// Cached now.
	c.resolve(item)
	if n := calls.Load(); n != 1 {
		t.Fatalf("%d runs after a second resolve", n)
	}
}

func TestCompletionResolveDeferredWhileRunning(t *testing.T) {
	root := t.TempDir()
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	run := func(ctx context.Context, tl extool.Tool, args []string, o extool.RunOptions) (extool.Result, error) {
		started <- struct{}{}
		<-release
		return extool.Result{Stdout: []byte(helpSample)}, nil
	}
	c := clientWith(t, clientCfg{root: root, initOpts: helpInitOpts(t, nil), onServer: func(s *server) { s.helpRun = run }})
	l, raw := c.completeAt("a.sh", "pri▮")
	item := rawItem(t, l, raw, "printf")
	c.sendReq("800", "completionItem/resolve", item)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the lookup never started")
	}
	if syms := c.docSymbols("a.sh"); syms == nil {
		t.Log("document symbols empty")
	}
	if c.hasReply("800") {
		t.Fatal("answered before the lookup finished")
	}
	close(release)
	r := c.waitReply("800")
	if r.Error != nil || !strings.Contains(string(r.Result), "printf [-v var] format") {
		t.Fatalf("got %+v %s", r.Error, r.Result)
	}
}

func TestCompletionResolveScriptSymbol(t *testing.T) {
	var calls atomic.Int32
	c := clientWith(t, clientCfg{root: t.TempDir(), initOpts: helpInitOpts(t, nil), onServer: func(s *server) { s.helpRun = countingRunner(&calls) }})
	l, raw := c.completeAt("a.sh", "# Build it.\nbuild_it() { local x=\"$1\"; }\nbuild▮")
	r, out := c.resolve(rawItem(t, l, raw, "build_it"))
	doc, _ := out["documentation"].(map[string]any)
	if r.Error != nil || doc == nil || !strings.Contains(fmt.Sprint(doc["value"]), "build_it") || doc["kind"] != "markdown" {
		t.Fatalf("got %s", r.Result)
	}
	if calls.Load() != 0 {
		t.Fatal("a script symbol started a program")
	}
	l, raw = c.completeAt("b.sh", "level=3\necho $lev▮")
	_, out = c.resolve(rawItem(t, l, raw, "level"))
	if doc, _ := out["documentation"].(map[string]any); doc == nil || !strings.Contains(fmt.Sprint(doc["value"]), "level") {
		t.Fatalf("variable doc: %v", out)
	}
}

func TestCompletionResolveOtherKindsUnchanged(t *testing.T) {
	c := client(t, t.TempDir())
	l, raw := c.completeAt("a.sh", "echo ${x▮")
	item := rawItem(t, l, raw, "^^")
	r, out := c.resolve(item)
	if r.Error != nil || out["label"] != "^^" || out["documentation"] != nil {
		t.Fatalf("got %s", r.Result)
	}
	// A malformed data field is not an error.
	r, _ = c.resolve(map[string]any{"label": "x", "data": "garbage"})
	if r.Error != nil {
		t.Fatalf("garbage data: %+v", r.Error)
	}
	r, _ = c.resolve(map[string]any{"label": "x"})
	if r.Error != nil {
		t.Fatalf("no data: %+v", r.Error)
	}
}

func TestCompletionOptionsFromCachedDoc(t *testing.T) {
	var calls atomic.Int32
	c := clientWith(t, clientCfg{root: t.TempDir(), initOpts: helpInitOpts(t, nil), onServer: func(s *server) { s.helpRun = countingRunner(&calls) }})
	l, raw := c.completeAt("a.sh", "printf -▮")
	if len(l.Items) != 0 {
		t.Fatalf("options before the doc is cached: %s", raw)
	}
	l, raw = c.completeAt("b.sh", "pri▮")
	c.resolve(rawItem(t, l, raw, "printf")) // caches the doc
	before := calls.Load()
	l, raw = c.completeAt("a.sh", "printf -▮")
	it, ok := l.find("-v")
	if !ok || it.Kind != 10 || it.Detail != "var" {
		t.Fatalf("got %s", raw)
	}
	if calls.Load() != before {
		t.Fatal("completion ran a program")
	}
}

// bigScript mirrors bench/gen_big.py: n functions, about 280 bytes each.
func bigScript(n int) string {
	var b strings.Builder
	b.WriteString("#!/usr/bin/env bash\necho start\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "# Function number %d.\nfn_%d() {\n  local a=\"$1\" b=\"$2\"\n", i, i)
		fmt.Fprintf(&b, "  if [[ -n \"$a\" && \"$b\" != \"x%d\" ]]; then\n    printf '%%s %%s\\n' \"$a\" \"$b\" | tr a-z A-Z\n  else\n", i)
		b.WriteString("    for f in \"$@\"; do echo \"${f:-none}\" >&2; done\n  fi\n")
		fmt.Fprintf(&b, "  case \"$a\" in start|stop) echo \"$a\";; *) return %d;; esac\n}\n\n", i%250)
		if i > 0 {
			fmt.Fprintf(&b, "fn_%d a b\n", i-1)
		}
	}
	return b.String()
}

// TestCompletionLatencyBig times completion over the wire on the 561 KB
// script. The target is 20 ms; the test only guards against a
// regression of an order of magnitude, since the machine may be loaded.
func TestCompletionLatencyBig(t *testing.T) {
	if raceEnabled {
		t.Skip("timings under the race detector mean nothing")
	}
	c := client(t, t.TempDir())
	big := bigScript(2000)
	cases := map[string]string{
		"variable":       big + "\nlast() {\n  local mine=1\n  echo $m▮\n}\n",
		"command-prefix": big + "\nfn_19▮",
		"command-empty":  big + "\n▮",
		"paramop":        big + "\necho ${x▮",
	}
	for name, src := range cases {
		text, p := markPos(src)
		c.openText(name+".sh", text)
		c.waitDiag(name + ".sh")
		var ds []time.Duration
		var lastItems []compItem
		for i := 0; i < 15; i++ {
			start := time.Now()
			l, _ := c.complete(name+".sh", p)
			ds = append(ds, time.Since(start))
			if len(l.Items) == 0 {
				t.Fatalf("%s: no items", name)
			}
			lastItems = l.Items
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		t.Logf("%s: %d items, median %v, max %v", name, len(lastItems), ds[len(ds)/2], ds[len(ds)-1])
		if name == "command-empty" && len(lastItems) != 1000 {
			t.Errorf("%s: %d items, want the full 1000 this timing is about", name, len(lastItems))
		}
		if ds[len(ds)/2] > 250*time.Millisecond {
			t.Errorf("%s: median %v", name, ds[len(ds)/2])
		}
	}
}

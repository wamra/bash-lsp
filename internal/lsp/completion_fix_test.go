package lsp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wamra/bash-lsp/internal/extool"
)

func TestCompletionResolveCancelKillsLookup(t *testing.T) {
	started := make(chan struct{}, 1)
	sawDone := make(chan struct{}, 1)
	run := func(ctx context.Context, tl extool.Tool, args []string, o extool.RunOptions) (extool.Result, error) {
		started <- struct{}{}
		select {
		case <-ctx.Done():
			sawDone <- struct{}{}
			return extool.Result{Exit: -1}, ctx.Err()
		case <-time.After(10 * time.Second):
			return extool.Result{Stdout: []byte(helpSample)}, nil
		}
	}
	c := clientWith(t, clientCfg{root: t.TempDir(), initOpts: helpInitOpts(t, nil), onServer: func(s *server) { s.helpRun = run }})
	l, raw := c.completeAt("a.sh", "pri▮")
	c.sendReq("820", "completionItem/resolve", rawItem(t, l, raw, "printf"))
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the lookup never started")
	}
	c.cancel("820")
	r := c.waitReply("820")
	if r.Error == nil || r.Error.Code != codeRequestCancelled {
		t.Fatalf("reply %+v %s, want -32800", r.Error, r.Result)
	}
	select {
	case <-sawDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the runner's context was not cancelled")
	}
}

const manSecret = `ZZPROG(1)

SYNOPSIS
     zzprog [-t arg] token=abc123secret

OPTIONS
     -t arg  Use the key password=hunter2 when asked.
`

func TestCompletionOutwardTextIsRedacted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("execute bits")
	}
	dir := progDir(t, "zzprog")
	run := func(ctx context.Context, tl extool.Tool, args []string, o extool.RunOptions) (extool.Result, error) {
		return extool.Result{Stdout: []byte(manSecret)}, nil
	}
	opts := helpInitOpts(t, nil)
	opts["path"] = map[string]any{"profile": []string{dir}}
	c := clientWith(t, clientCfg{root: t.TempDir(), initOpts: opts, onServer: func(s *server) { s.helpRun = run }})
	l, raw := c.completeAt("a.sh", "zzp▮")
	_, out := c.resolve(rawItem(t, l, raw, "zzprog"))
	if doc, _ := out["documentation"].(map[string]any); doc == nil || strings.Contains(fmt.Sprint(doc["value"]), "abc123secret") {
		t.Fatalf("synopsis not redacted: %v", out["documentation"])
	}
	// The option text of the cached page.
	l, raw = c.completeAt("b.sh", "zzprog -▮")
	it, ok := l.find("-t")
	if !ok || it.Documentation == nil {
		t.Fatalf("no -t with text: %s", raw)
	}
	if strings.Contains(it.Documentation.Value, "hunter2") || !strings.Contains(it.Documentation.Value, "[redacted]") {
		t.Fatalf("option text %q", it.Documentation.Value)
	}
	// A script symbol's doc comment.
	l, raw = c.completeAt("c.sh", "# Login with password=hunter2 now.\nlogin() { :; }\nlog▮")
	_, out = c.resolve(rawItem(t, l, raw, "login"))
	doc, _ := out["documentation"].(map[string]any)
	if doc == nil || strings.Contains(fmt.Sprint(doc["value"]), "hunter2") || !strings.Contains(fmt.Sprint(doc["value"]), "[redacted]") {
		t.Fatalf("script doc: %v", out["documentation"])
	}
	// A variable's declaration line.
	l, raw = c.completeAt("d.sh", "export token=abc123secretvalue\necho $tok▮")
	_, out = c.resolve(rawItem(t, l, raw, "token"))
	if doc, _ := out["documentation"].(map[string]any); doc != nil && strings.Contains(fmt.Sprint(doc["value"]), "token=abc123secretvalue") {
		t.Fatalf("variable doc: %v", out["documentation"])
	}
}

func TestCompletionResolveRefusedWordStartsNothing(t *testing.T) {
	var calls atomic.Int32
	c := clientWith(t, clientCfg{root: t.TempDir(), initOpts: helpInitOpts(t, nil), onServer: func(s *server) { s.helpRun = countingRunner(&calls) }})
	c.completeAt("a.sh", "▮")
	before := resolveLookups.Load()
	for _, d := range []map[string]any{
		{"uri": c.uri("a.sh"), "label": "nosuchprogram", "kind": "command"},
		{"uri": c.uri("a.sh"), "label": "notabuiltin", "kind": "builtin"},
		{"uri": c.uri("a.sh"), "label": "ls; rm -rf /", "kind": "command"},
	} {
		r, out := c.resolve(map[string]any{"label": d["label"], "data": d})
		if r.Error != nil || out["documentation"] != nil {
			t.Fatalf("%v: %s", d, r.Result)
		}
	}
	if n := resolveLookups.Load() - before; n != 0 {
		t.Fatalf("%d lookup goroutines for refused words", n)
	}
	if calls.Load() != 0 {
		t.Fatalf("%d runs", calls.Load())
	}
	c.resolve(map[string]any{"label": "printf", "data": map[string]any{"uri": c.uri("a.sh"), "label": "printf", "kind": "builtin"}})
	if n := resolveLookups.Load() - before; n != 1 || calls.Load() != 1 {
		t.Fatalf("control: %d goroutines, %d runs", n, calls.Load())
	}
}

func TestCompletionResolveHostileData(t *testing.T) {
	var mu sync.Mutex
	var ran [][]string
	run := func(ctx context.Context, tl extool.Tool, args []string, o extool.RunOptions) (extool.Result, error) {
		mu.Lock()
		ran = append(ran, args)
		mu.Unlock()
		return extool.Result{Stdout: []byte(helpSample)}, nil
	}
	dir := progDir(t, "okprog")
	opts := helpInitOpts(t, nil)
	opts["path"] = map[string]any{"profile": []string{dir}}
	c := clientWith(t, clientCfg{root: t.TempDir(), initOpts: opts, onServer: func(s *server) { s.helpRun = run }})
	c.openText("a.sh", "x=1\n")
	labels := []string{
		"ls; rm -rf /", "$(touch x)", "--html=cmd", strings.Repeat("a", 300), "../bin/x", "-x", "a\nb",
		"`id`", "printf; id", " printf", "printf ", "/bin/sh", "okprog --help",
	}
	kinds := []string{"builtin", "keyword", "command", "function", "variable", "alias", "../x", ""}
	for _, l := range labels {
		for _, k := range kinds {
			r, _ := c.resolve(map[string]any{"label": "x", "data": map[string]any{"uri": c.uri("a.sh"), "label": l, "kind": k}})
			if r.Error != nil {
				t.Fatalf("label %q kind %q: %+v", l, k, r.Error)
			}
		}
	}
	// Wrong shapes of data.
	for _, d := range []any{nil, 5, []any{1}, map[string]any{"uri": 3, "label": []any{}, "kind": map[string]any{}}, "x"} {
		if r, _ := c.resolve(map[string]any{"label": "x", "data": d}); r.Error != nil {
			t.Fatalf("data %v: %+v", d, r.Error)
		}
	}
	mu.Lock()
	n := len(ran)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("hostile data started programs: %q", ran)
	}
	// Controls: a builtin and a program of the profile do run.
	c.resolve(map[string]any{"label": "printf", "data": map[string]any{"uri": c.uri("a.sh"), "label": "printf", "kind": "builtin"}})
	mu.Lock()
	defer mu.Unlock()
	if len(ran) != 1 || ran[0][len(ran[0])-1] != "printf" {
		t.Fatalf("runs %q, want one for printf", ran)
	}
}

func TestCompletionHostilePathNamesAreNotOffered(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("execute bits")
	}
	dir := t.TempDir()
	for _, n := range []string{"x\nrm -rf y", "-rf", "zgood"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	c := clientWith(t, clientCfg{root: t.TempDir(), initOpts: map[string]any{"path": map[string]any{"profile": []string{dir}}}})
	for _, src := range []string{"▮", "x▮", "-▮"} {
		l, raw := c.completeAt("a.sh", src)
		for _, it := range l.Items {
			if strings.ContainsAny(it.Label, "\n") || strings.HasPrefix(it.Label, "-") && it.Kind != 10 {
				t.Fatalf("%q: hostile item %q in %.200s", src, it.Label, raw)
			}
		}
	}
	l, _ := c.completeAt("b.sh", "zg▮")
	if _, ok := l.find("zgood"); !ok {
		t.Fatal("the plain name is missing")
	}
}

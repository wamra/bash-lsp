package lsp

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/internal/extool"
)

const tokenName = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"

func TestCompletionDropsItemsThatRedactionWouldChange(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("execute bits")
	}
	dir := progDir(t, tokenName, "ghp_plain")
	c := clientWith(t, clientCfg{root: t.TempDir(), initOpts: map[string]any{"path": map[string]any{"profile": []string{dir}}}})
	l, raw := c.completeAt("a.sh", "ghp▮")
	if strings.Contains(string(raw), tokenName) || strings.Contains(string(raw), "[redacted]") {
		t.Fatalf("secret-shaped PATH name in the answer: %.300s", raw)
	}
	if _, ok := l.find("ghp_plain"); !ok {
		t.Fatalf("the plain neighbour is missing: %.300s", raw)
	}
}

const manToken = `ZZPROG(1)

SYNOPSIS
     zzprog [-t arg]

OPTIONS
     -t arg  A plain option.
     --xoxb-1234567890-abc  An option named like a token.
`

func TestCompletionDropsOptionsThatRedactionWouldChange(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("execute bits")
	}
	dir := progDir(t, "zzprog")
	run := func(ctx context.Context, tl extool.Tool, args []string, o extool.RunOptions) (extool.Result, error) {
		return extool.Result{Stdout: []byte(manToken)}, nil
	}
	opts := helpInitOpts(t, nil)
	opts["path"] = map[string]any{"profile": []string{dir}}
	c := clientWith(t, clientCfg{root: t.TempDir(), initOpts: opts, onServer: func(s *server) { s.helpRun = run }})
	l, raw := c.completeAt("a.sh", "zzp▮")
	c.resolve(rawItem(t, l, raw, "zzprog"))
	l, raw = c.completeAt("b.sh", "zzprog -▮")
	if _, ok := l.find("-t"); !ok {
		t.Fatalf("the plain option is missing: %s", raw)
	}
	if strings.Contains(string(raw), "xoxb-1234567890-abc") || strings.Contains(string(raw), "[redacted]") {
		t.Fatalf("token-shaped option in the answer: %s", raw)
	}
}

func TestCompletionDropsItemsWithASecretInTheDetail(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, tokenName+".sh"), "secretfn() { :; }\n")
	put(t, filepath.Join(root, "plain.sh"), "plainfn() { :; }\n")
	c := client(t, root)
	l, raw := c.completeAt("main.sh", "source ./"+tokenName+".sh\nsource ./plain.sh\n▮")
	if _, ok := l.find("plainfn"); !ok {
		t.Fatalf("plainfn missing: %.300s", raw)
	}
	if strings.Contains(string(raw), tokenName) || strings.Contains(string(raw), "secretfn") {
		t.Fatalf("an item whose detail names the secret is offered: %.300s", raw)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatal(err)
	}
}

func TestCompletionDropsItemsWithASecretInTheData(t *testing.T) {
	// The document URI is part of every item's data: a root whose name looks
	// like a token leaves nothing to offer, and nothing of it is sent.
	root := filepath.Join(t.TempDir(), tokenName)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	c := client(t, root)
	l, raw := c.completeAt("a.sh", "pri▮")
	if len(l.Items) != 0 || strings.Contains(string(raw), tokenName) {
		t.Fatalf("items carry the secret in data: %.300s", raw)
	}
}

// Each text an item sends has its own guard: a secret in any one of them, with
// the others plain and different, drops the item. The URI is not one of them;
// completion checks it once per request.
func TestOutwardSafeGuardsEachField(t *testing.T) {
	plain := func() completionItem {
		return completionItem{Label: "lbl", Detail: "det", SortText: "00001",
			TextEdit: TextEdit{NewText: "txt"}, Data: completionData{URI: "file:///w/a.sh", Label: "dlbl", Kind: "variable"}}
	}
	if !outwardSafe(plain()) {
		t.Fatal("the plain item is refused")
	}
	for name, mutate := range map[string]func(*completionItem){
		"label":    func(it *completionItem) { it.Label = tokenName },
		"detail":   func(it *completionItem) { it.Detail = "xoxb-1234567890-abc" },
		"newText":  func(it *completionItem) { it.TextEdit.NewText = "AKIAABCDEFGHIJKLMNOP" },
		"sortText": func(it *completionItem) { it.SortText = tokenName },
		"data.label": func(it *completionItem) {
			it.Data = completionData{URI: "file:///w/a.sh", Label: tokenName, Kind: "variable"}
		},
		"data.kind": func(it *completionItem) {
			it.Data = completionData{URI: "file:///w/a.sh", Label: "dlbl", Kind: tokenName}
		},
	} {
		it := plain()
		mutate(&it)
		if outwardSafe(it) {
			t.Errorf("a secret in %s is not caught", name)
		}
	}
}

// A dropped item and a dropped root each leave one notice in the log.
func TestCompletionLogsWhatItDrops(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, tokenName+".sh"), "secretfn() { :; }\n")
	put(t, filepath.Join(root, "plain.sh"), "plainfn() { :; }\n")
	c := client(t, root)
	c.completeAt("main.sh", "source ./"+tokenName+".sh\nsource ./plain.sh\n▮")
	if n := strings.Count(c.logs.String(), "completion: left out 1 item(s)"); n != 1 {
		t.Fatalf("%d notices for a dropped item: %q", n, c.logs.String())
	}
	bad := filepath.Join(t.TempDir(), tokenName)
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	c2 := client(t, bad)
	c2.completeAt("a.sh", "pri▮")
	if n := strings.Count(c2.logs.String(), "completion: document URI looks like a secret"); n != 1 {
		t.Fatalf("%d notices for a dropped root: %q", n, c2.logs.String())
	}
	if strings.Contains(c2.logs.String(), tokenName) || strings.Contains(c.logs.String(), tokenName) {
		t.Fatal("the secret is in the log")
	}
}

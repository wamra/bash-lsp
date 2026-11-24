package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/internal/config"
	"github.com/wamra/bash-lsp/internal/mcp"
)

// mcpText runs one tools/call over MCP on root and returns the text of its
// content and whether the result is an error.
func mcpText(t *testing.T, root, tool string, args map[string]any) (text string, isError bool) {
	t.Helper()
	req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
	var out, logw bytes.Buffer
	if err := mcp.Serve(strings.NewReader(string(req)+"\n"), &out, &logw, root, config.Settings{}, "test"); err != nil {
		t.Fatal(err)
	}
	var r struct {
		Result struct {
			Content []struct{ Text string }
			IsError bool
		}
		Error any
	}
	mustJSON(t, out.String(), &r)
	if r.Error != nil || len(r.Result.Content) != 1 {
		t.Fatalf("reply %s", out.String())
	}
	return r.Result.Content[0].Text, r.Result.IsError
}

// TestAC20: a question asked on the command line and over MCP gets the same
// bytes, because one encoder writes both.
func TestAC20(t *testing.T) {
	for _, c := range []struct {
		tool string
		args map[string]any
		cli  []string
	}{
		{"get_callers", map[string]any{"symbol": "lib/util.sh:4:1"}, []string{"callers", "lib/util.sh:4:1"}},
		{"get_callers", map[string]any{"symbol": "lib/util.sh:4:1", "depth": 3}, []string{"callers", "lib/util.sh:4:1", "--depth", "3"}},
		{"get_callees", map[string]any{"symbol": "main.sh"}, []string{"callees", "main.sh"}},
		{"find_references", map[string]any{"symbol": "lib/util.sh:4:1"}, []string{"refs", "lib/util.sh:4:1"}},
		{"analyze_impact", map[string]any{"target": "lib/util.sh:4:1", "depth": 1}, []string{"impact", "lib/util.sh:4:1", "--depth", "1"}},
		{"trace_dependency", map[string]any{"from": "main.sh", "to": "lib/util.sh:3:1"}, []string{"trace", "main.sh", "lib/util.sh:3:1"}},
		{"trace_dependency", map[string]any{"from": "orphan.sh", "to": "main.sh"}, []string{"trace", "orphan.sh", "main.sh"}},
		{"list_unresolved", map[string]any{}, []string{"unresolved"}},
		{"get_index_status", map[string]any{}, []string{"status"}},
	} {
		got, isErr := mcpText(t, corpus, c.tool, c.args)
		want := runOK(t, append(append([]string(nil), c.cli...), "--root", corpus, "--json")...)
		if isErr || got != want {
			t.Errorf("%s %v: MCP and CLI differ (isError %v)\n--- MCP\n%s--- CLI\n%s", c.tool, c.args, isErr, got, want)
		}
	}
	// the brief's own example, build_release, is defined in two files of the
	// corpus: both surfaces refuse it with the same list of candidates
	got, isErr := mcpText(t, corpus, "get_callers", map[string]any{"symbol": "build_release"})
	code, _, errs := runCode(t, "callers", "build_release", "--root", corpus, "--json")
	if !isErr || code != 2 || errs != "bash-lsp: "+got+"\n" {
		t.Errorf("ambiguous symbol: MCP %q (isError %v), CLI exit %d %q", got, isErr, code, errs)
	}
	// and generation 1 is in the document
	text, _ := mcpText(t, corpus, "get_callers", map[string]any{"symbol": "lib/util.sh:4:1"})
	var doc struct {
		Generation int `json:"generation"`
		Results    []struct{ Relation string }
	}
	mustJSON(t, text, &doc)
	if doc.Generation != 1 || len(doc.Results) == 0 {
		t.Errorf("%s", text)
	}
}

func TestMCPCommandUsage(t *testing.T) {
	if code, _, errs := runCode(t, "mcp", "extra"); code != 2 || !strings.Contains(errs, "usage: bash-lsp mcp") {
		t.Errorf("exit %d %q", code, errs)
	}
	if code, _, _ := runCode(t, "mcp", "--root"); code != 2 {
		t.Errorf("exit %d", code)
	}
}

// `bash-lsp mcp` over its real entry point: replies on stdout, the log on stderr.
func TestMCPCommandServes(t *testing.T) {
	var out, errb bytes.Buffer
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")
	if code := run([]string{"mcp", "--root", corpus}, in, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if strings.TrimSpace(out.String()) != `{"jsonrpc":"2.0","id":1,"result":{}}` {
		t.Errorf("stdout %q", out.String())
	}
	if !strings.Contains(errb.String(), "serving") {
		t.Errorf("stderr %q", errb.String())
	}
	if code := run([]string{"mcp", "--root", corpus + "/nope"}, strings.NewReader(""), &out, &errb); code != 1 {
		t.Errorf("missing root: exit %d", code)
	}
}

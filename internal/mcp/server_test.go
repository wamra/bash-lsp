package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wamra/bash-lsp/internal/config"
)

// TestMain points the user settings file at a path that does not exist.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "bash-lsp-mcp-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("BASH_LSP_CONFIG", filepath.Join(dir, "none.toml"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// workspaceRoot writes files (name -> text) under a new directory whose path
// has no symlink in it.
func workspaceRoot(t testing.TB, files map[string]string) string {
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
	return root
}

type session struct {
	out  []byte
	logs string
}

// serve runs one server over the given input lines (each gets a newline) and
// returns what it wrote to stdout and to its log.
func serve(t testing.TB, root string, cfg config.Settings, input ...string) session {
	t.Helper()
	var out, logw bytes.Buffer
	in := strings.NewReader(strings.Join(input, "\n") + "\n")
	if err := Serve(in, &out, &logw, root, cfg, "test"); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	return session{out.Bytes(), logw.String()}
}

// replies decodes every stdout line.
func (s session) replies(t testing.TB) []map[string]any {
	t.Helper()
	var rs []map[string]any
	for _, line := range bytes.Split(bytes.TrimRight(s.out, "\n"), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("stdout line is not JSON: %v\n%.300s", err, line)
		}
		rs = append(rs, m)
	}
	return rs
}

func call(id int, name string, args any) string {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	return string(b)
}

// toolResult is the decoded result of a tools/call reply.
type toolResult struct {
	IsError bool
	Text    string
	Doc     map[string]any
}

func result(t testing.TB, reply map[string]any) toolResult {
	t.Helper()
	if reply["error"] != nil {
		t.Fatalf("protocol error: %v", reply["error"])
	}
	res, _ := reply["result"].(map[string]any)
	content, _ := res["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content: %v", res)
	}
	c := content[0].(map[string]any)
	if c["type"] != "text" {
		t.Fatalf("content type %v", c["type"])
	}
	tr := toolResult{IsError: res["isError"] == true, Text: c["text"].(string)}
	if !tr.IsError {
		if err := json.Unmarshal([]byte(tr.Text), &tr.Doc); err != nil {
			t.Fatalf("text is not a JSON document: %v\n%.300s", err, tr.Text)
		}
	}
	return tr
}

const initialize = `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`

func TestToolsList(t *testing.T) {
	root := workspaceRoot(t, map[string]string{"a.sh": ":\n"})
	rs := serve(t, root, config.Settings{}, initialize, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`).replies(t)
	if len(rs) != 2 {
		t.Fatalf("%d replies, want 2 (the notification gets none): %v", len(rs), rs)
	}
	tools := rs[1]["result"].(map[string]any)["tools"].([]any)
	var names []string
	for _, x := range tools {
		tl := x.(map[string]any)
		names = append(names, tl["name"].(string))
		if tl["description"] == "" || tl["description"] == nil {
			t.Errorf("%v: no description", tl["name"])
		}
		sch, ok := tl["inputSchema"].(map[string]any)
		if !ok || sch["type"] != "object" || sch["additionalProperties"] != false {
			t.Errorf("%v: schema %v", tl["name"], tl["inputSchema"])
		}
		if a, _ := tl["annotations"].(map[string]any); a["readOnlyHint"] != true {
			t.Errorf("%v: not marked read-only: %v", tl["name"], tl["annotations"])
		}
	}
	want := "find_symbol find_references get_callers get_callees trace_dependency analyze_impact list_unresolved get_index_status search_code get_diagnostics preview_rename"
	if got := strings.Join(names, " "); got != want {
		t.Fatalf("tools\n got %s\nwant %s", got, want)
	}
	for _, n := range names {
		for _, bad := range []string{"write", "exec", "run", "rename", "delete", "edit", "shell"} {
			if n == "preview_rename" {
				continue // a preview: it plans and never writes (TestMCPPreviewRenameWritesNothing)
			}
			if strings.Contains(n, bad) {
				t.Errorf("tool %s looks like a write or exec tool", n)
			}
		}
	}
}

func TestMCPVersionNegotiation(t *testing.T) {
	root := workspaceRoot(t, nil)
	for _, c := range []struct{ client, want string }{
		{"2025-06-18", "2025-06-18"},
		{"2025-11-25", "2025-11-25"},
		{"2025-03-26", "2025-03-26"},
		{"2024-11-05", "2024-11-05"},
		{"2026-07-28", "2025-11-25"},
		{"1999-01-01", "2025-11-25"},
		{"", "2025-11-25"},
	} {
		req := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":%q,"capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`, c.client)
		rs := serve(t, root, config.Settings{}, req).replies(t)
		res := rs[0]["result"].(map[string]any)
		if res["protocolVersion"] != c.want {
			t.Errorf("client %q: got %v, want %s", c.client, res["protocolVersion"], c.want)
		}
		info := res["serverInfo"].(map[string]any)
		if info["name"] != "bash-lsp" || info["version"] != "test" {
			t.Errorf("serverInfo %v", info)
		}
		if _, ok := res["capabilities"].(map[string]any)["tools"]; !ok {
			t.Errorf("capabilities %v", res["capabilities"])
		}
	}
	// a client version that is not a string
	rs := serve(t, root, config.Settings{}, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":7}}`).replies(t)
	if rs[0]["error"] == nil {
		t.Errorf("protocolVersion 7: %v", rs[0])
	}
}

func TestMCPStdoutClean(t *testing.T) {
	root := workspaceRoot(t, map[string]string{"a.sh": "f() { g; }\ng() { :; }\n"})
	s := serve(t, root, config.Settings{},
		initialize,
		`not json at all`,
		`{"jsonrpc":"2.0","id":1,"method":"nope"}`,
		call(2, "get_callers", map[string]any{"symbol": "g"}),
		call(3, "get_callers", map[string]any{"symbol": "missing"}),
		`[1,2]`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":2}}`,
		`{"jsonrpc":"2.0","id":4,"method":"ping"}`,
	)
	rs := s.replies(t)
	// initialize, garbage, nope, call 2, call 3, batch, ping; the notification gets none
	if len(rs) != 7 {
		t.Fatalf("%d replies: %s", len(rs), s.out)
	}
	for _, r := range rs {
		if r["jsonrpc"] != "2.0" {
			t.Errorf("reply without jsonrpc 2.0: %v", r)
		}
		if _, hasID := r["id"]; !hasID {
			t.Errorf("reply without id member: %v", r)
		}
		if (r["result"] == nil) == (r["error"] == nil) {
			t.Errorf("reply must have exactly one of result and error: %v", r)
		}
	}
	if !strings.Contains(s.logs, "missing") {
		t.Errorf("the failed call is not in the log (stderr):\n%s", s.logs)
	}
	if bytes.Contains(s.out, []byte("mcp:")) {
		t.Error("log text on stdout")
	}
}

func TestMCPRootEscape(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "x.sh"), []byte("evil() { :; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := workspaceRoot(t, map[string]string{"in.sh": "f() { :; }\n"})
	if err := os.Symlink(outside, filepath.Join(root, "out")); err != nil {
		t.Skip("no symlinks:", err)
	}
	if err := os.Symlink(filepath.Join(outside, "x.sh"), filepath.Join(root, "x.sh")); err != nil {
		t.Fatal(err)
	}
	escapes := []string{"../../etc/passwd", "../x.sh", filepath.Join(outside, "x.sh"), "/etc/passwd", "out/x.sh", "out/x.sh:1:1", "x.sh", "out/../../x.sh"}
	if runtime.GOOS == "windows" {
		escapes = escapes[:3]
	}
	var in []string
	id := 10
	type probe struct{ tool, what string }
	var probes []probe
	for _, e := range escapes {
		for _, c := range []struct {
			tool string
			args map[string]any
		}{
			{"get_callers", map[string]any{"symbol": e}},
			{"get_callees", map[string]any{"symbol": e}},
			{"find_references", map[string]any{"symbol": e}},
			{"analyze_impact", map[string]any{"target": e}},
			{"trace_dependency", map[string]any{"from": e, "to": "in.sh"}},
			{"trace_dependency", map[string]any{"from": "in.sh", "to": e}},
			{"list_unresolved", map[string]any{"paths": []string{"in.sh", e}}},
			{"get_diagnostics", map[string]any{"paths": []string{e}}},
			{"preview_rename", map[string]any{"symbol": e, "new_name": "zz"}},
		} {
			id++
			in = append(in, call(id, c.tool, c.args))
			probes = append(probes, probe{c.tool, e})
		}
	}
	rs := serve(t, root, config.Settings{}, in...).replies(t)
	if len(rs) != len(probes) {
		t.Fatalf("%d replies for %d calls", len(rs), len(probes))
	}
	for i, r := range rs {
		tr := result(t, r)
		if !tr.IsError || !strings.Contains(tr.Text, "outside the root") {
			t.Errorf("%s %q: not refused as outside the root: isError=%v %.200s", probes[i].tool, probes[i].what, tr.IsError, tr.Text)
		}
		if strings.Contains(tr.Text, "evil") {
			t.Errorf("%s %q: content from outside the root", probes[i].tool, probes[i].what)
		}
	}
	// the same tools answer a path inside the root
	rs = serve(t, root, config.Settings{}, call(1, "get_diagnostics", map[string]any{"paths": []string{"in.sh"}}), call(2, "get_callees", map[string]any{"symbol": "in.sh"})).replies(t)
	for _, r := range rs {
		if tr := result(t, r); tr.IsError {
			t.Errorf("a path inside the root refused: %s", tr.Text)
		}
	}
}

func TestMCPOutputCap(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&b, "function_with_a_long_name_%05d() { :; }\n", i)
	}
	root := workspaceRoot(t, map[string]string{"big.sh": b.String()})
	s := serve(t, root, config.Settings{}, call(1, "find_symbol", map[string]any{"query": ""}))
	line := bytes.TrimRight(s.out, "\n")
	if len(line) > 1<<20 {
		t.Fatalf("reply is %d bytes, over 1 MiB", len(line))
	}
	var r map[string]any
	if err := json.Unmarshal(line, &r); err != nil {
		t.Fatal(err)
	}
	tr := result(t, r)
	if tr.Doc["truncated"] != true {
		t.Fatalf("not marked truncated: %v", tr.Doc["truncated"])
	}
	n := len(tr.Doc["results"].([]any))
	if n == 0 || n >= 10000 {
		t.Errorf("%d results: want some, and fewer than the 10,000 list cap (the byte cap cut first)", n)
	}
	if len(tr.Text) >= 1<<20 {
		t.Errorf("document text is %d bytes", len(tr.Text))
	}
}

func TestMCPRedactsTokens(t *testing.T) {
	token := "ghp_" + strings.Repeat("aB3", 12)
	// "token=sekrit1234.sh" is a field the query layer does not redact
	// itself (a path): only the encoding of the whole answer does.
	root := workspaceRoot(t, map[string]string{"a.sh": "TOKEN=" + token + "\nf() { :; }\n", "token=sekrit1234.sh": "gfunc() { :; }\n"})
	s := serve(t, root, config.Settings{},
		call(1, "find_symbol", map[string]any{"query": "TOKEN"}),
		call(2, "get_callers", map[string]any{"symbol": "TOKEN=" + token}),
		call(3, "find_symbol", map[string]any{"query": "gfunc"}),
	)
	if strings.Contains(string(s.out), token) {
		t.Fatalf("token in the reply:\n%s", s.out)
	}
	rs := s.replies(t)
	tr := result(t, rs[0])
	rec := tr.Doc["results"].([]any)[0].(map[string]any)
	if rec["name"] != "TOKEN" || rec["signature"] != "TOKEN=[redacted]" {
		t.Errorf("record %v", rec)
	}
	if e := result(t, rs[1]); !e.IsError || !strings.Contains(e.Text, "[redacted]") {
		t.Errorf("error text %q", e.Text)
	}
	if strings.Contains(string(s.out), "sekrit1234") {
		t.Fatalf("a path was not redacted:\n%s", s.out)
	}
	if rec := result(t, rs[2]).Doc["results"].([]any)[0].(map[string]any); rec["path"] != "token=[redacted]" {
		t.Errorf("path %v", rec["path"])
	}
	// the log is written through redact.Writer
	if strings.Contains(s.logs, token) {
		t.Fatalf("token in the log:\n%s", s.logs)
	}
	if !strings.Contains(s.logs, "[redacted]") {
		t.Errorf("the failed call (whose message names the symbol) is not in the log:\n%s", s.logs)
	}
}

// ids of every JSON type, notifications that carry one, batches and other
// shapes get a reply or none, never a panic.
func TestMCPShapesOfMessages(t *testing.T) {
	root := workspaceRoot(t, map[string]string{"a.sh": ":\n"})
	long := strings.Repeat("x", 5000)
	type tc struct {
		name, line string
		replies    int    // -1: any number
		id         string // the id the reply echoes (raw), "" when it must be null
	}
	cases := []tc{
		{"int id", `{"jsonrpc":"2.0","id":7,"method":"ping"}`, 1, `7`},
		{"negative id", `{"jsonrpc":"2.0","id":-7,"method":"ping"}`, 1, `-7`},
		{"float id", `{"jsonrpc":"2.0","id":1.5,"method":"ping"}`, 1, `1.5`},
		{"big number id", `{"jsonrpc":"2.0","id":123456789012345678901234567890,"method":"ping"}`, 1, `123456789012345678901234567890`},
		{"string id", `{"jsonrpc":"2.0","id":"abc","method":"ping"}`, 1, `"abc"`},
		{"string id with escapes", `{"jsonrpc":"2.0","id":"a\"b\\cé","method":"ping"}`, 1, `"a\"b\\cé"`},
		{"empty string id", `{"jsonrpc":"2.0","id":"","method":"ping"}`, 1, `""`},
		{"null id", `{"jsonrpc":"2.0","id":null,"method":"ping"}`, 1, ""},
		{"bool id", `{"jsonrpc":"2.0","id":true,"method":"ping"}`, 1, ""},
		{"false id", `{"jsonrpc":"2.0","id":false,"method":"ping"}`, 1, ""},
		{"object id", `{"jsonrpc":"2.0","id":{"a":1},"method":"ping"}`, 1, ""},
		{"array id", `{"jsonrpc":"2.0","id":[1],"method":"ping"}`, 1, ""},
		{"huge string id", `{"jsonrpc":"2.0","id":"` + long + `","method":"ping"}`, 1, ""},
		{"notification", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, 0, ""},
		{"unknown notification", `{"jsonrpc":"2.0","method":"notifications/whatever","params":{"a":[1]}}`, 0, ""},
		{"unknown method as notification", `{"jsonrpc":"2.0","method":"nope"}`, 0, ""},
		{"notification method with id", `{"jsonrpc":"2.0","id":3,"method":"notifications/initialized"}`, 1, `3`},
		{"unknown method", `{"jsonrpc":"2.0","id":3,"method":"nope"}`, 1, `3`},
		{"empty method", `{"jsonrpc":"2.0","id":3,"method":""}`, 1, `3`},
		{"method not a string", `{"jsonrpc":"2.0","id":3,"method":5}`, 1, ""},
		{"no method", `{"jsonrpc":"2.0","id":3}`, 0, ""},
		{"a client response", `{"jsonrpc":"2.0","id":3,"result":{}}`, 0, ""},
		{"a client error response", `{"jsonrpc":"2.0","id":3,"error":{"code":1,"message":"x"}}`, 0, ""},
		{"wrong jsonrpc", `{"jsonrpc":"1.0","id":3,"method":"ping"}`, 1, `3`},
		{"batch", `[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","id":2,"method":"ping"}]`, 1, ""},
		{"empty batch", `[]`, 1, ""},
		{"batch of junk", `[1,"a",null,[],{}]`, 1, ""},
		{"number", `42`, 1, ""},
		{"string", `"ping"`, 1, ""},
		{"null", `null`, 1, ""},
		{"true", `true`, 1, ""},
		{"empty object", `{}`, 0, ""},
		{"truncated json", `{"jsonrpc":"2.0","id":1,"method":"pi`, 1, ""},
		{"trailing junk", `{"jsonrpc":"2.0","id":1,"method":"ping"} x`, 1, ""},
		{"two objects on a line", `{"jsonrpc":"2.0","id":1,"method":"ping"}{"jsonrpc":"2.0","id":2,"method":"ping"}`, 1, ""},
		{"deep params", `{"jsonrpc":"2.0","id":1,"method":"ping","params":` + strings.Repeat("[", 5000) + strings.Repeat("]", 5000) + `}`, 1, ""},
		{"deep params object", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"find_symbol","arguments":` + strings.Repeat(`{"a":`, 3000) + `1` + strings.Repeat("}", 3000) + `}}`, 1, ""},
		{"params an array", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":[1]}`, 1, `1`},
		{"params null", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":null}`, 1, `1`},
		{"call without name", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`, 1, `1`},
		{"call name not a string", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":[1]}}`, 1, `1`},
		{"unknown tool", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"no_such_tool","arguments":{}}}`, 1, `1`},
		{"arguments a string", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"find_symbol","arguments":"x"}}`, 1, `1`},
		{"arguments an array", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"find_symbol","arguments":[]}}`, 1, `1`},
		{"missing required argument", call(1, "get_callers", map[string]any{}), 1, `1`},
		{"wrong argument type", call(1, "get_callers", map[string]any{"symbol": 5}), 1, `1`},
		{"unknown argument", call(1, "find_symbol", map[string]any{"query": "a", "extra": 1}), 1, `1`},
		{"negative depth", call(1, "get_callers", map[string]any{"symbol": "a.sh", "depth": -5}), 1, `1`},
		{"huge depth", call(1, "get_callers", map[string]any{"symbol": "a.sh", "depth": 1e18}), 1, `1`},
		{"fractional depth", call(1, "get_callers", map[string]any{"symbol": "a.sh", "depth": 1.5}), 1, `1`},
		{"oversized symbol", call(1, "find_references", map[string]any{"symbol": long + long}), 1, `1`},
		{"too many paths", call(1, "list_unresolved", map[string]any{"paths": make([]string, 5000)}), 1, `1`},
		{"NUL in a path", call(1, "get_diagnostics", map[string]any{"paths": []string{"a\x00.sh"}}), 1, `1`},
		{"invalid UTF-8", "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"find_symbol\",\"arguments\":{\"query\":\"\xff\xfe\"}}}", 1, `1`},
		{"lone surrogate", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"find_symbol","arguments":{"query":"\ud800"}}}`, 1, `1`},
		{"blank line", ``, 0, ""},
		{"spaces", `   `, 0, ""},
		{"CR only", "\r", 0, ""},
		{"NUL bytes", "\x00\x00\x00", 1, ""},
	}
	for _, c := range cases {
		s := serve(t, root, config.Settings{}, c.line, `{"jsonrpc":"2.0","id":"after","method":"ping"}`)
		rs := s.replies(t)
		if len(rs) != c.replies+1 {
			t.Errorf("%s: %d replies, want %d (and the ping after it): %.300s", c.name, len(rs), c.replies+1, s.out)
			continue
		}
		if last := rs[len(rs)-1]; last["id"] != "after" || last["result"] == nil {
			t.Errorf("%s: the ping after was not answered: %v", c.name, last)
		}
		if c.replies == 0 {
			continue
		}
		raw := firstLine(s.out)
		var env struct {
			ID    json.RawMessage `json:"id"`
			Error *struct{ Code int }
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		wantID := c.id
		if wantID == "" {
			wantID = "null"
		}
		if string(env.ID) != wantID {
			t.Errorf("%s: reply id %s, want %s", c.name, env.ID, wantID)
		}
	}
}

func firstLine(b []byte) []byte {
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		return b[:i]
	}
	return b
}

func TestMCPErrorCodes(t *testing.T) {
	root := workspaceRoot(t, nil)
	for _, c := range []struct {
		line string
		code float64
	}{
		{`not json`, -32700},
		{`[]`, -32600},
		{`{"jsonrpc":"2.0","id":1,"method":"nope"}`, -32601},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope"}}`, -32602},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"find_symbol","arguments":[]}}`, -32602},
		{`{"jsonrpc":"2.0","id":true,"method":"ping"}`, -32600},
	} {
		rs := serve(t, root, config.Settings{}, c.line).replies(t)
		e, _ := rs[0]["error"].(map[string]any)
		if e == nil || e["code"] != c.code || e["message"] == "" {
			t.Errorf("%s: %v, want code %v", c.line, rs[0], c.code)
		}
	}
}

func TestMCPOversizedFrame(t *testing.T) {
	root := workspaceRoot(t, nil)
	huge := `{"jsonrpc":"2.0","id":1,"method":"ping","params":"` + strings.Repeat("a", 3<<20) + `"}`
	s := serve(t, root, config.Settings{}, huge, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	rs := s.replies(t)
	if len(rs) != 2 {
		t.Fatalf("%d replies: %.200s", len(rs), s.out)
	}
	if e, _ := rs[0]["error"].(map[string]any); e == nil || rs[0]["id"] != nil {
		t.Errorf("oversized frame: %v", rs[0])
	}
	if rs[1]["id"] != float64(2) || rs[1]["result"] == nil {
		t.Errorf("the next frame was lost: %v", rs[1])
	}
	// the last frame has no newline
	var out bytes.Buffer
	if err := Serve(strings.NewReader(`{"jsonrpc":"2.0","id":9,"method":"ping"}`), &out, nil, root, config.Settings{}, "t"); err != nil || !strings.Contains(out.String(), `"id":9`) {
		t.Errorf("unterminated last frame: %v %q", err, out.String())
	}
}

func TestMCPToolsAnswer(t *testing.T) {
	root := workspaceRoot(t, map[string]string{
		"lib/util.sh": "build() { :; }\nclean() { build; }\n",
		"main.sh":     "source ./lib/util.sh\nrelease() { build; clean; }\nrelease\n\"$x\"\n",
	})
	rs := serve(t, root, config.Settings{},
		call(1, "find_symbol", map[string]any{"query": "rel"}),
		call(2, "find_references", map[string]any{"symbol": "build"}),
		call(3, "get_callers", map[string]any{"symbol": "build", "depth": 2}),
		call(4, "get_callees", map[string]any{"symbol": "release"}),
		call(5, "trace_dependency", map[string]any{"from": "main.sh", "to": "build"}),
		call(6, "analyze_impact", map[string]any{"target": "build"}),
		call(7, "list_unresolved", map[string]any{}),
		call(8, "get_index_status", map[string]any{}),
		call(9, "get_diagnostics", map[string]any{"paths": []string{"main.sh"}}),
		call(10, "get_index_status", nil),
	).replies(t)
	for i, r := range rs {
		tr := result(t, r)
		if tr.IsError {
			t.Errorf("call %d: %s", i+1, tr.Text)
			continue
		}
		if tr.Doc["generation"] == nil {
			t.Errorf("call %d: no generation: %s", i+1, tr.Text)
		}
	}
	names := func(tr toolResult) []string {
		var out []string
		for _, x := range tr.Doc["results"].([]any) {
			out = append(out, x.(map[string]any)["name"].(string))
		}
		return out
	}
	if got := names(result(t, rs[0])); len(got) != 1 || got[0] != "release" {
		t.Errorf("find_symbol: %v", got)
	}
	if got := result(t, rs[2]).Doc["results"].([]any); len(got) < 3 {
		t.Errorf("get_callers depth 2: %d edges", len(got))
	}
	if tr := result(t, rs[4]); tr.Doc["found"] != true {
		t.Errorf("trace: %v", tr.Doc)
	}
	if tr := result(t, rs[7]); tr.Doc["files"] != float64(2) || tr.Doc["root"] != root {
		t.Errorf("status: %v", tr.Doc)
	}
}

// Every call reads the files again: an edit between two calls shows.
func TestMCPRefreshesOnEveryCall(t *testing.T) {
	root := workspaceRoot(t, map[string]string{"a.sh": "one() { :; }\n"})
	in, out := newPipe()
	done := make(chan error, 1)
	go func() { done <- Serve(in.r, out.w, nil, root, config.Settings{}, "t") }()
	ask := func(line string) map[string]any {
		in.send(line)
		return out.next(t)
	}
	r1 := result(t, ask(call(1, "find_symbol", map[string]any{"query": "two"})))
	if n := len(r1.Doc["results"].([]any)); n != 0 {
		t.Fatalf("before the edit: %d", n)
	}
	if err := os.WriteFile(filepath.Join(root, "a.sh"), []byte("one() { :; }\ntwo() { :; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.sh"), []byte("three() { :; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r2 := result(t, ask(call(2, "find_symbol", map[string]any{"query": "two"})))
	if n := len(r2.Doc["results"].([]any)); n != 1 {
		t.Fatalf("after the edit: %d results", n)
	}
	if g1, g2 := r1.Doc["generation"].(float64), r2.Doc["generation"].(float64); g2 <= g1 {
		t.Errorf("generation %v then %v", g1, g2)
	}
	if n := len(result(t, ask(call(3, "find_symbol", map[string]any{"query": "three"}))).Doc["results"].([]any)); n != 1 {
		t.Errorf("a new file: %d", n)
	}
	in.close()
	if err := <-done; err != nil {
		t.Errorf("Serve: %v", err)
	}
}

// Nothing in the workspace is run: a program named code-index inside the
// root, configured by path or reachable through PATH, is refused.
func TestMCPRunsNothingFromTheWorkspace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stand-in")
	}
	marker := filepath.Join(t.TempDir(), "ran")
	script := "#!/bin/sh\necho ran > " + marker + "\necho '{\"hits\":[],\"route\":\"fts\"}'\n"
	root := workspaceRoot(t, map[string]string{"a.sh": "f() { :; }\n"})
	prog := filepath.Join(root, "code-index")
	if err := os.WriteFile(prog, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, cfg := range []config.Settings{{CodeIndexPath: prog}, {}} {
		rs := serve(t, root, cfg, call(1, "search_code", map[string]any{"text": "f"})).replies(t)
		tr := result(t, rs[0])
		ci, _ := tr.Doc["codeindex"].(map[string]any)
		if tr.IsError || ci == nil || ci["used"] != false {
			t.Fatalf("cfg %+v: %s", cfg, tr.Text)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("cfg %+v: the workspace's code-index ran", cfg)
		}
	}
}

func TestServeMissingRoot(t *testing.T) {
	// a root that is not a directory is an error from Serve, not a panic
	if err := Serve(strings.NewReader(""), &bytes.Buffer{}, nil, filepath.Join(t.TempDir(), "missing"), config.Settings{}, "t"); err == nil {
		t.Error("a missing root must be an error")
	}
}

// pipe is a connection a test drives one line at a time.
type inPipe struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func (p inPipe) send(line string) { io.WriteString(p.w, line+"\n") }
func (p inPipe) close()           { p.w.Close() }

type outPipe struct {
	w     *io.PipeWriter
	lines chan []byte
}

func newPipe() (inPipe, outPipe) {
	ir, iw := io.Pipe()
	or, ow := io.Pipe()
	o := outPipe{w: ow, lines: make(chan []byte, 16)}
	go func() {
		sc := bufio.NewScanner(or)
		sc.Buffer(nil, 4<<20)
		for sc.Scan() {
			o.lines <- append([]byte(nil), sc.Bytes()...)
		}
		close(o.lines)
	}()
	return inPipe{ir, iw}, o
}

func (o outPipe) next(t testing.TB) map[string]any {
	t.Helper()
	select {
	case l, ok := <-o.lines:
		if !ok {
			t.Fatal("server closed its output")
		}
		var m map[string]any
		if err := json.Unmarshal(l, &m); err != nil {
			t.Fatalf("%v: %.200s", err, l)
		}
		return m
	case <-time.After(30 * time.Second):
		t.Fatal("no reply in 30 s")
	}
	return nil
}

// Error text is redacted before it is cut, so a token that straddles the cut
// is gone whole, in the reply and in the log.
func TestMCPRedactsBeforeClipping(t *testing.T) {
	token := "ghp_" + strings.Repeat("Y", 36)
	root := workspaceRoot(t, nil)
	var in []string
	for pad := 40; pad < 80; pad += 5 {
		m := strings.Repeat("m", pad) + token
		in = append(in, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q}`, pad, m))
		in = append(in, call(1000+pad, "get_callers", map[string]any{"symbol": strings.Repeat("s", pad) + token}))
		in = append(in, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q}}`, 2000+pad, strings.Repeat("n", pad)+token))
	}
	// a long message: the cut falls inside the token
	for pad := 270; pad < 300; pad += 4 {
		in = append(in, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q}`, 3000+pad, strings.Repeat("m", pad)+token))
	}
	s := serve(t, root, config.Settings{}, in...)
	for _, bad := range []string{"ghp_", "YYYY"} {
		if strings.Contains(string(s.out), bad) || strings.Contains(s.logs, bad) {
			t.Fatalf("a piece of the token (%s) is in the output or the log", bad)
		}
	}
	for _, r := range s.replies(t) {
		if e, ok := r["error"].(map[string]any); ok && len(e["message"].(string)) > maxMsgBytes {
			t.Errorf("message of %d bytes", len(e["message"].(string)))
		}
	}
}

func TestMCPPreviewRenameWritesNothing(t *testing.T) {
	files := map[string]string{"a.sh": "f() { :; }\nf\ncmd=f\n\"$cmd\"\n", "b.sh": "source ./a.sh\nf\n"}
	root := workspaceRoot(t, files)
	rs := serve(t, root, config.Settings{},
		call(1, "preview_rename", map[string]any{"symbol": "f", "new_name": "h"}),
		call(2, "preview_rename", map[string]any{"symbol": "f", "new_name": "if"}),
		call(3, "preview_rename", map[string]any{"symbol": "f"}),
		call(4, "preview_rename", map[string]any{"symbol": "f", "new_name": "h", "apply": true}),
		call(5, "preview_rename", map[string]any{"symbol": "", "new_name": "h"}),
		call(6, "preview_rename", map[string]any{"symbol": "a.sh:2:1", "new_name": strings.Repeat("x", 5000)}),
		call(7, "preview_rename", map[string]any{"symbol": "f", "new_name": "h\x00"}),
		call(8, "preview_rename", map[string]any{"symbol": "f\x00", "new_name": "h"}),
		call(9, "preview_rename", map[string]any{"symbol": strings.Repeat("f", 5000), "new_name": "h"}),
		call(10, "preview_rename", map[string]any{"symbol": "f", "new_name": ""}),
	).replies(t)
	if tr := result(t, rs[0]); tr.IsError || len(tr.Doc["edits"].([]any)) != 3 || len(tr.Doc["excluded"].([]any)) != 2 || tr.Doc["generation"] == nil {
		t.Errorf("preview: %v %s", tr.IsError, tr.Text)
	}
	if tr := result(t, rs[1]); !tr.IsError || !strings.HasPrefix(tr.Text, "refused: reserved-name: ") {
		t.Errorf("refusal: %v %s", tr.IsError, tr.Text)
	}
	for i := 2; i < len(rs); i++ {
		if tr := result(t, rs[i]); !tr.IsError {
			t.Errorf("call %d accepted: %s", i+1, tr.Text)
		}
	}
	for name, text := range files {
		if b, _ := os.ReadFile(filepath.Join(root, name)); string(b) != text {
			t.Errorf("%s changed", name)
		}
	}
}

func TestMCPPreviewRenameRefusesLoneCR(t *testing.T) {
	files := map[string]string{"a.sh": "x=1;\rf() { :; }\nf\n"}
	root := workspaceRoot(t, files)
	rs := serve(t, root, config.Settings{},
		call(1, "preview_rename", map[string]any{"symbol": "f", "new_name": "h"}),
		call(2, "preview_rename", map[string]any{"symbol": "a.sh:2:1", "new_name": "h"}),
	).replies(t)
	for i, r := range rs {
		if tr := result(t, r); !tr.IsError || !strings.HasPrefix(tr.Text, "refused: unsupported-line-breaks: ") || !strings.Contains(tr.Text, "a.sh") {
			t.Errorf("call %d: %v %s", i+1, tr.IsError, tr.Text)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(root, "a.sh")); string(b) != files["a.sh"] {
		t.Errorf("a.sh changed: %q", b)
	}
}

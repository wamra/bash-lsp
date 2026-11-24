package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/internal/config"
)

// FuzzMCPReader feeds arbitrary bytes to the request loop of a server that
// already holds a small workspace: whatever the bytes are, the loop returns
// without a panic, and every line it writes is one JSON-RPC 2.0 message of
// at most MaxResponse bytes.
func FuzzMCPReader(f *testing.F) {
	root := workspaceRoot(f, map[string]string{
		"a.sh":     "f() { g; }\ng() { :; }\nTOKEN=abc\nf\n\"$x\"\n",
		"lib/b.sh": "source ../a.sh\nh() { f; }\n",
	})
	s, err := newServer(root, config.Settings{}, "fuzz", io.Discard)
	if err != nil {
		f.Fatal(err)
	}
	seeds := []string{
		initialize,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":"x","method":"ping"}` + "\n" + `{"jsonrpc":"2.0","id":2,"method":"ping"}`,
		call(1, "find_symbol", map[string]any{"query": "f"}),
		call(2, "get_callers", map[string]any{"symbol": "g", "depth": 3}),
		call(3, "trace_dependency", map[string]any{"from": "f", "to": "g"}),
		call(4, "analyze_impact", map[string]any{"target": "lib/b.sh:2:1", "depth": 0}),
		call(5, "get_diagnostics", map[string]any{"paths": []string{"a.sh", "../x"}}),
		call(6, "list_unresolved", map[string]any{}),
		call(7, "get_index_status", map[string]any{}),
		call(8, "search_code", map[string]any{"text": "--mode=auto"}),
		call(9, "find_references", map[string]any{"symbol": "../../etc/passwd"}),
		`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`,
		`{"jsonrpc":"2.0","id":null,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":{"a":[1]},"method":"ping"}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"find_symbol","arguments":` + strings.Repeat(`{"a":`, 40) + `1` + strings.Repeat("}", 40) + `}}`,
		`{"jsonrpc":"2.0","id":12345678901234567890123,"method":"nope"}`,
		"{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"find_symbol\",\"arguments\":{\"query\":\"\xff\\ud800\"}}}",
		"\x00\x01\r\n\r\n   \n",
		strings.Repeat("a", 70000),
	}
	for _, sd := range seeds {
		f.Add([]byte(sd + "\n"))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var out bytes.Buffer
		if err := s.run(bytes.NewReader(data), &out); err != nil {
			t.Fatalf("run: %v", err)
		}
		for _, line := range bytes.Split(bytes.TrimRight(out.Bytes(), "\n"), []byte("\n")) {
			if len(line) == 0 && out.Len() == 0 {
				continue
			}
			if len(line) > MaxResponse {
				t.Fatalf("reply of %d bytes", len(line))
			}
			var m struct {
				JSONRPC string          `json:"jsonrpc"`
				ID      json.RawMessage `json:"id"`
				Result  json.RawMessage `json:"result"`
				Error   json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal(line, &m); err != nil || m.JSONRPC != "2.0" || len(m.ID) == 0 || (len(m.Result) == 0) == (len(m.Error) == 0) {
				t.Fatalf("bad reply %.200q for input %.200q", line, data)
			}
		}
	})
}

func TestReadFrame(t *testing.T) {
	for _, c := range []struct {
		in      string
		max     int
		frames  []string
		tooBigs []bool
	}{
		{"a\nbb\n", 10, []string{"a", "bb"}, []bool{false, false}},
		{"a\r\nbb", 10, []string{"a", "bb"}, []bool{false, false}},
		{"\n\n", 10, []string{"", ""}, []bool{false, false}},
		{"abcdefghij\nx\n", 10, []string{"abcdefghij", "x"}, []bool{false, false}},
		{"abcdefghijk\nx\n", 10, []string{"", "x"}, []bool{true, false}},
		{strings.Repeat("z", 200000) + "\nok\n", 10, []string{"", "ok"}, []bool{true, false}},
		{strings.Repeat("z", 200000), 10, []string{""}, []bool{true}},
		{"", 10, nil, nil},
	} {
		r := bufio.NewReaderSize(strings.NewReader(c.in), 16)
		var frames []string
		var big []bool
		for {
			f, tb, err := readFrame(r, c.max)
			if err != nil {
				if err != io.EOF {
					t.Fatalf("%q: %v", c.in, err)
				}
				break
			}
			frames, big = append(frames, string(f)), append(big, tb)
		}
		if strings.Join(frames, "|") != strings.Join(c.frames, "|") || len(big) != len(c.tooBigs) {
			t.Errorf("%.30q: frames %q, want %q", c.in, frames, c.frames)
			continue
		}
		for i := range big {
			if big[i] != c.tooBigs[i] {
				t.Errorf("%.30q: tooBig %v, want %v", c.in, big, c.tooBigs)
			}
		}
	}
}

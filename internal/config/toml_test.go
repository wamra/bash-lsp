package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseTOMLSubset(t *testing.T) {
	src := "\ufeff# header\ntop = 1\n\n[bash] # trailing\ntarget = \"3.2-5.3\"  # c\nflag = true\nneg = -7\n" +
		"esc = \"a\\\"b\\\\c\\n\\t\\u00e9\"\nlit = 'C:\\x\\y'\nhash = \"a # b\"\nlist = [ \"x\", 'y', ]\nempty = []\r\n[limits]\n"
	got, err := ParseTOML([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]any{
		"":       {"top": int64(1)},
		"bash":   {"target": "3.2-5.3", "flag": true, "neg": int64(-7), "esc": "a\"b\\c\n\té", "lit": `C:\x\y`, "hash": "a # b", "list": []string{"x", "y"}, "empty": []string{}},
		"limits": {},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %#v\nwant %#v", got, want)
	}
}

func TestParseTOMLRejects(t *testing.T) {
	cases := []struct{ src, line, frag string }{
		{"a = {x=1}", "line 1", "inline"},
		{"[a]\nb = 1.5", "line 2", "unsupported"},
		{"[a.b]", "line 1", "one-level"},
		{"[[a]]", "line 1", "arrays of tables"},
		{"a.b = 1", "line 1", "dotted"},
		{"\"a\" = 1", "line 1", "quoted"},
		{"a = \"\"\"x\"\"\"", "line 1", "multi-line"},
		{"a = '''x'''", "line 1", "multi-line"},
		{"a = [\n1]", "line 1", "multi-line"},
		{"a = [1, 2]", "line 1", "strings"},
		{"a = \"x\\q\"", "line 1", "escape"},
		{"a = \"x\\u12\"", "line 1", "escape"},
		{"a = \"x", "line 1", "unterminated"},
		{"a = 1\na = 2", "line 2", "duplicate"},
		{"[a]\n[a]", "line 2", "duplicate"},
		{"a = 1 2", "line 1", "trailing"},
		{"a = 0123", "line 1", "unsupported"},
		{"a = 99999999999999999999", "line 1", "range"},
		{"a", "line 1", "="},
		{"x = 1\n[ok]\nb = \"s\" junk", "line 3", "trailing"},
		{"a = \"x\x01\"", "line 1", "control"},
	}
	for _, c := range cases {
		_, err := ParseTOML([]byte(c.src))
		if err == nil || !strings.Contains(err.Error(), c.line) || !strings.Contains(err.Error(), c.frag) {
			t.Errorf("%q: error %v, want %q and %q", c.src, err, c.line, c.frag)
		}
	}
}

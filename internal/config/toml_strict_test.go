package config

import (
	"strings"
	"testing"
)

func TestParseTOMLStrictBytes(t *testing.T) {
	cases := []struct{ name, src, line string }{
		{"invalid utf-8", "a = 1\nb = \"\xff\"\n", "line 2"},
		{"nbsp before value", "a = 1", "line 1"},
		{"vertical tab trailing", "x = 1\na = 1\v", "line 2"},
		{"formfeed trailing", "a = 1\f", "line 1"},
		{"unicode space around key", " a = 1", "line 1"},
		{"control in literal", "x = 1\na = 'a\x01b'", "line 2"},
		{"control in comment", "# ok\n\na = 1 # c\x07", "line 3"},
		{"control in full-line comment", "x = 1\n# c\x1b", "line 2"},
		{"del in comment", "# c\x7f", "line 1"},
		{"lone CR", "a = 1\nb = 2\rc = 3\n", "line 2"},
		{"lone CR only", "a = 1\r b = 2", "line 1"},
	}
	for _, c := range cases {
		if _, err := ParseTOML([]byte(c.src)); err == nil || !strings.Contains(err.Error(), c.line) {
			t.Errorf("%s: error %v, want %q", c.name, err, c.line)
		}
	}
	if _, err := ParseTOML([]byte("a = 1 # tab\there\r\nb = 'x\ty'\r\n")); err != nil {
		t.Fatalf("tab and CRLF must stay valid: %v", err)
	}
}

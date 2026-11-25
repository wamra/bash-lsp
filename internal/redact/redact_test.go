package redact

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func TestStringPatterns(t *testing.T) {
	ghp := "ghp_" + strings.Repeat("a1B2", 9)
	for _, c := range []struct{ in, want string }{
		{"key AKIAABCDEFGHIJKLMNOP end", "key [redacted] end"},
		{"AKIAabcdefghijklmnop", "AKIAabcdefghijklmnop"}, // lower case: not a key id
		{"t=" + ghp + ".", "t=[redacted]."},
		{"ghs_" + strings.Repeat("x", 35), "ghs_" + strings.Repeat("x", 35)}, // too short
		{"github_pat_" + strings.Repeat("Z_9", 8), "[redacted]"},
		{"xoxb-1234567890-abc", "[redacted]"},
		{"xoxz-1234567890-abc", "xoxz-1234567890-abc"},
		{"-----BEGIN RSA PRIVATE KEY-----\nMIIE", "[redacted]\nMIIE"},
		{"-----BEGIN PRIVATE KEY-----", "[redacted]"},
		{"TOKEN=" + ghp, "TOKEN=[redacted]"},
		{"password=hunter2 next", "password=[redacted] next"},
		{"PASSWD=x", "PASSWD=[redacted]"},
		{"api_key=abc and apikey=def", "api_key=[redacted] and apikey=[redacted]"},
		{"secret=", "secret="}, // no value
		{"nothing to see", "nothing to see"},
		{"", ""},
	} {
		if got := String(c.in); got != c.want {
			t.Errorf("String(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestJSONKeepsStructureAndOrder(t *testing.T) {
	doc := `{
  "z": "TOKEN=abc123",
  "a": ["plain", "password=p\"q", 7, null, true],
  "k": {"signature": "x=1 secret=s3"}
}`
	want := `{
  "z": "TOKEN=[redacted]",
  "a": ["plain", "password=[redacted]", 7, null, true],
  "k": {"signature": "x=1 secret=[redacted]"}
}`
	got := string(JSON([]byte(doc)))
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	var v any
	if err := json.Unmarshal([]byte(got), &v); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	// an escaped secret is found too
	if got := string(JSON([]byte(`"a\u003d b TOKEN\u003dxyz"`))); strings.Contains(got, "xyz") {
		t.Errorf("escaped secret leaked: %s", got)
	}
	in := []byte(`{"a":"b"}`)
	if out := JSON(in); string(out) != string(in) {
		t.Error("a document with nothing to redact must come back byte-identical")
	}
}

func TestJSONMalformedInputDoesNotPanic(t *testing.T) {
	for _, in := range []string{``, `"`, `"\`, `"abc`, `{"a":"TOKEN=x`, `\\\\`, "\x00\"\\u12"} {
		_ = JSON([]byte(in))
	}
}

func TestWriterRedactsPerLineAndBoundsLines(t *testing.T) {
	var out bytes.Buffer
	w := Writer(&out)
	ghp := "ghp_" + strings.Repeat("q", 36)
	// one line split over three writes
	for _, p := range []string{"log TOKEN=", ghp[:10], ghp[10:] + " tail\nsecond line\n"} {
		if n, err := w.Write([]byte(p)); err != nil || n != len(p) {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	if got, want := out.String(), "log TOKEN=[redacted] tail\nsecond line\n"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	// a line with no newline for far longer than the bound is emitted in pieces, redacted
	out.Reset()
	long := strings.Repeat("word ", MaxLine/2) + "password=" + strings.Repeat("s", 10) + strings.Repeat(" x", 10)
	w.Write([]byte(long))
	w.Write([]byte("\n"))
	if strings.Contains(out.String(), "password=ssss") {
		t.Fatal("secret in an overlong line leaked")
	}
	if out.Len() == 0 {
		t.Fatal("nothing written")
	}
	// the buffer never grows past the bound
	var bw bytes.Buffer
	x := Writer(&bw).(*writer)
	for i := 0; i < 10; i++ {
		x.Write([]byte(strings.Repeat("a", MaxLine/2)))
		if len(x.buf) > MaxLine {
			t.Fatalf("buffer %d past the bound %d", len(x.buf), MaxLine)
		}
	}
}

func TestWriterFlushWritesPartialLine(t *testing.T) {
	var out bytes.Buffer
	w := Writer(&out)
	w.Write([]byte("no newline secret=abc"))
	if out.Len() != 0 {
		t.Fatal("a partial line must wait")
	}
	if f, ok := w.(interface{ Flush() error }); !ok {
		t.Fatal("no Flush")
	} else if err := f.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "no newline secret=[redacted]" {
		t.Fatalf("got %q", got)
	}
}

func TestWriterConcurrent(t *testing.T) {
	var out bytes.Buffer
	w := Writer(&out)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				w.Write([]byte("token=abc\n"))
			}
		}()
	}
	wg.Wait()
	if strings.Contains(out.String(), "abc") {
		t.Fatal("leak")
	}
}

func TestStringKeepsClosingQuote(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`no function matches "password=hunter2"`, `no function matches "password=[redacted]"`},
		{`x 'token=abc' y`, `x 'token=[redacted]' y`},
		{`password="p\"a\\ss=word"`, `password=[redacted]"`},
		{`secret=abc`, `secret=[redacted]`},
	} {
		if got := String(c.in); got != c.want {
			t.Errorf("String(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A token that straddles the cut of a long line with no blank never leaks,
// whichever half the writer holds.
func TestWriterLongRunWithTokenAtTheCut(t *testing.T) {
	token := "ghp_" + strings.Repeat("Z", 36)
	for _, pad := range []int{MaxLine - 20, MaxLine - 4, MaxLine - 40, MaxLine, MaxLine + 5, 3*MaxLine - 30} {
		var out bytes.Buffer
		w := Writer(&out)
		line := strings.Repeat("a", pad) + token + strings.Repeat("b", 10) + " after\nnext\n"
		for i := 0; i < len(line); i += 7777 { // arbitrary write sizes
			w.Write([]byte(line[i:min(i+7777, len(line))]))
		}
		got := out.String()
		if strings.Contains(got, "ZZZZ") || strings.Contains(got, "ghp_") {
			t.Fatalf("pad %d: token (or a piece of it) leaked", pad)
		}
		if !strings.HasSuffix(got, "after\nnext\n") {
			t.Errorf("pad %d: text after the long run was lost: %q", pad, got[max(0, len(got)-40):])
		}
	}
}

func TestChangesAgreesWithString(t *testing.T) {
	for _, s := range []string{
		"plain", "", "ls", "AKIAABCDEFGHIJKLMNOP", "ghp_" + strings.Repeat("a", 36), "github_pat_" + strings.Repeat("a", 22),
		"xoxb-1234567890-abc", "-----BEGIN RSA PRIVATE KEY-----", "token=abc", "a=b", "--xoxb-1234567890-abc", "ghost",
	} {
		if got, want := Changes(s), String(s) != s; got != want {
			t.Errorf("Changes(%q) = %v, want %v", s, got, want)
		}
	}
}

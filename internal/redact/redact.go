// Package redact removes what looks like a credential from text that leaves
// the server: MCP answers and log lines. The patterns are a fixed list; a
// secret in an unknown format passes through, and a harmless string that
// matches is replaced.
package redact

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"sync"
)

// Mark replaces every match.
const Mark = "[redacted]"

// MaxLine is the most a Writer holds back waiting for a newline.
const MaxLine = 64 << 10

var (
	// whole matches
	whole = regexp.MustCompile(`AKIA[0-9A-Z]{16}` +
		`|gh[pousr]_[A-Za-z0-9]{36,}` +
		`|github_pat_[A-Za-z0-9_]{22,}` +
		`|xox[abprs]-[A-Za-z0-9-]{10,}` +
		`|-----BEGIN [A-Z ]*PRIVATE KEY-----`)
	// the value after = is replaced, the name stays
	assign = regexp.MustCompile(`(?i)(password|passwd|secret|token|api_?key)=\S+`)
)

// String replaces every credential-looking match in s with "[redacted]".
func String(s string) string {
	s = whole.ReplaceAllLiteralString(s, Mark)
	return assign.ReplaceAllStringFunc(s, func(m string) string {
		name, value, _ := strings.Cut(m, "=")
		// closing quotes after the value are text around it, not part of it
		core := strings.TrimRight(value, `"'`)
		if core == "" { // no value, only quotes
			return m
		}
		return name + "=" + Mark + value[len(core):]
	})
}

// hints are bytes a JSON string literal must hold, raw, to be worth decoding:
// the fixed starts of the patterns above and "=". A literal with a backslash
// is always decoded (the escapes could hide any of them).
var hints = [][]byte{[]byte("="), []byte("AKIA"), []byte("gh"), []byte("xox"), []byte("-----BEGIN")}

// JSON redacts every string of the JSON text b (keys too) and returns text
// with the same structure and key order: numbers, literals, white space and
// strings with nothing to redact are copied byte for byte. It never changes a
// delimiter, which a pattern over the encoded text could (`=\S+` reaches the
// closing quote). Input that is not JSON is copied as far as it goes; it is
// never rejected.
func JSON(b []byte) []byte {
	var out bytes.Buffer
	last := 0 // b[last:] is not copied yet
	changed := false
	for i := 0; i < len(b); {
		if b[i] != '"' {
			i++
			continue
		}
		j := i + 1
		for j < len(b) && b[j] != '"' {
			if b[j] == '\\' {
				j++
			}
			j++
		}
		if j >= len(b) { // unterminated: leave the rest as it is
			break
		}
		lit := b[i : j+1]
		if worth(lit) {
			var s string
			if json.Unmarshal(lit, &s) == nil {
				if r := String(s); r != s {
					if enc, err := json.Marshal(r); err == nil {
						out.Write(b[last:i])
						out.Write(enc)
						last = j + 1
						changed = true
					}
				}
			}
		}
		i = j + 1
	}
	if !changed {
		return b
	}
	out.Write(b[last:])
	return out.Bytes()
}

func worth(lit []byte) bool {
	if bytes.IndexByte(lit, '\\') >= 0 {
		return true
	}
	for _, h := range hints {
		if bytes.Contains(lit, h) {
			return true
		}
	}
	return false
}

type writer struct {
	mu       sync.Mutex
	w        io.Writer
	buf      []byte
	skipping bool // inside a run with no blank that overflowed: dropped up to the next blank
}

// Writer returns a writer that redacts what it passes to w one line at a
// time: a line is held until its newline arrives, so a secret split across
// two Write calls is still seen whole. A line longer than MaxLine is passed
// on in pieces cut at the last blank; a run of MaxLine bytes with no blank
// at all is replaced by one "[redacted]" and dropped up to the next blank or
// newline, because a secret could straddle any cut inside it. Flush (the returned
// writer has it) writes out a partial line. Safe for concurrent use. A Write
// always reports all of p as written; the error is the first one w gave.
func Writer(w io.Writer) io.Writer { return &writer{w: w} }

func (x *writer) Write(p []byte) (int, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	var first error
	n := len(p)
	for len(p) > 0 {
		take := min(max(MaxLine-len(x.buf), 1), len(p))
		x.buf = append(x.buf, p[:take]...)
		p = p[take:]
		if err := x.drain(); err != nil && first == nil {
			first = err
		}
	}
	return n, first
}

// drain writes out every complete line of buf, and a piece of a line that
// has filled the buffer.
func (x *writer) drain() error {
	var first error
	put := func(s string) {
		if _, err := io.WriteString(x.w, s); err != nil && first == nil {
			first = err
		}
	}
	for {
		if x.skipping {
			i := bytes.IndexAny(x.buf, " \t\n")
			if i < 0 {
				x.buf = nil
				return first
			}
			x.buf, x.skipping = x.buf[i:], false
		}
		i := bytes.IndexByte(x.buf, '\n')
		if i < 0 {
			break
		}
		put(String(string(x.buf[:i])) + "\n")
		x.buf = x.buf[i+1:]
	}
	if len(x.buf) >= MaxLine {
		if cut := strings.LastIndexAny(string(x.buf), " \t"); cut > 0 {
			put(String(string(x.buf[:cut])))
			x.buf = x.buf[cut:]
		} else {
			put(Mark)
			x.buf, x.skipping = nil, true
		}
	}
	if len(x.buf) == 0 {
		x.buf = nil
	} else {
		x.buf = append([]byte(nil), x.buf...) // do not keep the written lines alive
	}
	return first
}

// Flush writes out the line being held, without a newline.
func (x *writer) Flush() error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(x.buf) == 0 {
		return nil
	}
	_, err := io.WriteString(x.w, String(string(x.buf)))
	x.buf = nil
	return err
}

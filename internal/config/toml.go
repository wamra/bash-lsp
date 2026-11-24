package config

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ParseError is a syntax error in a settings file.
type ParseError struct {
	Line int
	Msg  string
}

func (e *ParseError) Error() string { return fmt.Sprintf("line %d: %s", e.Line, e.Msg) }

// entry is one key with the line it was read from.
type entry struct {
	section, key string
	val          any
	line         int
}

// ParseTOML reads the TOML subset settings files use: one-level [sections],
// bare keys, basic strings (escapes \" \\ \n \t \uXXXX), literal strings,
// decimal integers, booleans, single-line arrays of strings, and # comments.
// Anything else is an error naming its line. The result maps section to key to
// string, int64, bool or []string; keys before the first section sit under "".
func ParseTOML(data []byte) (map[string]map[string]any, error) {
	entries, sections, err := parseEntries(data)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]any{}
	for _, s := range sections {
		out[s] = map[string]any{}
	}
	for _, e := range entries {
		if out[e.section] == nil {
			out[e.section] = map[string]any{}
		}
		out[e.section][e.key] = e.val
	}
	return out, nil
}

func parseEntries(data []byte) (entries []entry, sections []string, err error) {
	text := strings.TrimPrefix(string(data), "\xef\xbb\xbf")
	if !utf8.ValidString(text) {
		n := 1
		for i := 0; i < len(text); {
			r, size := utf8.DecodeRuneInString(text[i:])
			if r == utf8.RuneError && size == 1 {
				break
			}
			if r == '\n' {
				n++
			}
			i += size
		}
		return nil, nil, &ParseError{n, "file is not valid UTF-8"}
	}
	section := ""
	seenSection := map[string]bool{}
	seenKey := map[string]bool{}
	for i, raw := range strings.Split(text, "\n") {
		n := i + 1
		line := trimWS(strings.TrimSuffix(raw, "\r"))
		if strings.Contains(line, "\r") {
			return nil, nil, &ParseError{n, "bare carriage return (lines end with LF or CRLF)"}
		}
		if line == "" {
			continue
		}
		if line[0] == '#' {
			if err := checkComment(line, n); err != nil {
				return nil, nil, err
			}
			continue
		}
		if line[0] == '[' {
			if strings.HasPrefix(line, "[[") {
				return nil, nil, &ParseError{n, "arrays of tables are not supported"}
			}
			end := strings.IndexByte(line, ']')
			if end < 0 {
				return nil, nil, &ParseError{n, "section header without closing ]"}
			}
			name := trimWS(line[1:end])
			if strings.Contains(name, ".") {
				return nil, nil, &ParseError{n, "only one-level sections are supported"}
			}
			if !bareKey(name) {
				return nil, nil, &ParseError{n, fmt.Sprintf("bad section name %q", name)}
			}
			if err := endOfLine(line[end+1:], n); err != nil {
				return nil, nil, err
			}
			if seenSection[name] {
				return nil, nil, &ParseError{n, fmt.Sprintf("duplicate section [%s]", name)}
			}
			seenSection[name] = true
			section = name
			sections = append(sections, name)
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return nil, nil, &ParseError{n, "expected key = value (no = found)"}
		}
		key := trimWS(line[:eq])
		switch {
		case strings.ContainsAny(key, "\"'"):
			return nil, nil, &ParseError{n, "quoted keys are not supported"}
		case strings.Contains(key, "."):
			return nil, nil, &ParseError{n, "dotted keys are not supported"}
		case !bareKey(key):
			return nil, nil, &ParseError{n, fmt.Sprintf("bad key %q", key)}
		}
		id := section + "\x00" + key
		if seenKey[id] {
			return nil, nil, &ParseError{n, fmt.Sprintf("duplicate key %q", key)}
		}
		seenKey[id] = true
		val, rest, err := parseValue(trimWS(line[eq+1:]), n)
		if err != nil {
			return nil, nil, err
		}
		if err := endOfLine(rest, n); err != nil {
			return nil, nil, err
		}
		entries = append(entries, entry{section, key, val, n})
	}
	return entries, sections, nil
}

func bareKey(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// trimWS trims spaces and tabs only, the whitespace TOML allows around tokens.
func trimWS(s string) string { return strings.Trim(s, " \t") }

func isControl(c byte) bool { return (c < 0x20 && c != '\t') || c == 0x7f }

// checkComment rejects control characters other than tab in a comment.
func checkComment(c string, n int) error {
	for i := 0; i < len(c); i++ {
		if isControl(c[i]) {
			return &ParseError{n, "control character in comment"}
		}
	}
	return nil
}

// endOfLine accepts blanks and one comment.
func endOfLine(rest string, n int) error {
	rest = trimWS(rest)
	if rest != "" && rest[0] == '#' {
		return checkComment(rest, n)
	}
	if rest == "" {
		return nil
	}
	return &ParseError{n, fmt.Sprintf("trailing text after value: %q", rest)}
}

// parseValue reads one value from the start of s and returns the rest.
func parseValue(s string, n int) (any, string, error) {
	switch {
	case s == "":
		return nil, "", &ParseError{n, "missing value"}
	case strings.HasPrefix(s, `"""`), strings.HasPrefix(s, "'''"):
		return nil, "", &ParseError{n, "multi-line strings are not supported"}
	case s[0] == '"' || s[0] == '\'':
		return parseString(s, n)
	case s[0] == '[':
		return parseArray(s, n)
	case s[0] == '{':
		return nil, "", &ParseError{n, "inline tables are not supported"}
	}
	end := strings.IndexAny(s, " \t#")
	tok, rest := s, ""
	if end >= 0 {
		tok, rest = s[:end], s[end:]
	}
	switch tok {
	case "true":
		return true, rest, nil
	case "false":
		return false, rest, nil
	}
	if isDecimal(tok) {
		v, err := strconv.ParseInt(tok, 10, 64)
		if err != nil {
			return nil, "", &ParseError{n, fmt.Sprintf("integer %s is out of range", tok)}
		}
		return v, rest, nil
	}
	return nil, "", &ParseError{n, fmt.Sprintf("unsupported value %q (strings, integers, booleans and arrays of strings only)", tok)}
}

func isDecimal(s string) bool {
	d := strings.TrimLeft(s, "+-")
	if len(s)-len(d) > 1 || d == "" || (len(d) > 1 && d[0] == '0') {
		return false
	}
	for _, c := range d {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func parseString(s string, n int) (any, string, error) {
	if s[0] == '\'' {
		end := strings.IndexByte(s[1:], '\'')
		if end < 0 {
			return nil, "", &ParseError{n, "unterminated string"}
		}
		for i := 1; i < 1+end; i++ {
			if isControl(s[i]) {
				return nil, "", &ParseError{n, "control character in string"}
			}
		}
		return s[1 : 1+end], s[end+2:], nil
	}
	var b strings.Builder
	for i := 1; i < len(s); {
		c := s[i]
		switch {
		case c == '"':
			return b.String(), s[i+1:], nil
		case c == '\\':
			if i+1 >= len(s) {
				return nil, "", &ParseError{n, "unterminated string"}
			}
			switch s[i+1] {
			case '"', '\\':
				b.WriteByte(s[i+1])
				i += 2
			case 'n':
				b.WriteByte('\n')
				i += 2
			case 't':
				b.WriteByte('\t')
				i += 2
			case 'u':
				if i+6 > len(s) {
					return nil, "", &ParseError{n, "bad \\u escape"}
				}
				cp, err := strconv.ParseUint(s[i+2:i+6], 16, 32)
				if err != nil || !utf8.ValidRune(rune(cp)) {
					return nil, "", &ParseError{n, "bad \\u escape"}
				}
				b.WriteRune(rune(cp))
				i += 6
			default:
				return nil, "", &ParseError{n, fmt.Sprintf("unsupported escape \\%c", s[i+1])}
			}
		case isControl(c):
			return nil, "", &ParseError{n, "control character in string"}
		default:
			b.WriteByte(c)
			i++
		}
	}
	return nil, "", &ParseError{n, "unterminated string"}
}

func parseArray(s string, n int) (any, string, error) {
	out := []string{}
	rest := trimWS(s[1:])
	for {
		switch {
		case rest == "":
			return nil, "", &ParseError{n, "multi-line arrays are not supported (array not closed on this line)"}
		case rest[0] == ']':
			return out, rest[1:], nil
		case rest[0] != '"' && rest[0] != '\'':
			return nil, "", &ParseError{n, "arrays hold only strings"}
		}
		v, after, err := parseString(rest, n)
		if err != nil {
			return nil, "", err
		}
		out = append(out, v.(string))
		rest = trimWS(after)
		switch {
		case strings.HasPrefix(rest, ","):
			rest = trimWS(rest[1:])
		case strings.HasPrefix(rest, "]"):
		default:
			return nil, "", &ParseError{n, "expected , or ] in array"}
		}
	}
}

package helpdoc

import (
	"bytes"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Limits on what one parsed page keeps.
const (
	maxSynopsis     = 2 << 10 // bytes
	maxOptions      = 256
	maxOptionText   = 200 // bytes
	maxOptionIndent = 9   // columns: a deeper line continues a description
)

// StripOverstrike removes the overstrike a terminal pager would draw: "X\bX"
// (bold) becomes "X" and "_\bX" (underline) becomes "X". The character before
// a backspace is dropped whole, so a multi-byte character is handled too.
func StripOverstrike(b []byte) []byte {
	if bytes.IndexByte(b, '\b') < 0 {
		return b
	}
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c != '\b' {
			out = append(out, c)
			continue
		}
		if len(out) > 0 {
			_, size := utf8.DecodeLastRune(out)
			out = out[:len(out)-size]
		}
	}
	return out
}

func splitLines(b []byte) []string {
	lines := strings.Split(string(b), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, "\r")
	}
	return lines
}

// bracketed matches a bracketed option of a help synopsis: [-ers], [-u fd],
// [--name arg].
// An alternative such as [-L|-P] is one match.
var bracketed = regexp.MustCompile(`\[(-{1,2}[A-Za-z0-9][A-Za-z0-9-]*(?:\|-{1,2}[A-Za-z0-9][A-Za-z0-9-]*)*)(?: ([^\]\[\s]+))?\]`)

// ParseHelp reads the output of the bash `help` builtin. The output may hold
// several topics (a prefix matches many); only the section whose first line
// starts "name: " is kept, or, for a compound command, "name ... closer: "
// (`[[ ... ]]: [[ expression ]]`). Its synopsis is the rest of that line, and
// its options are the bracketed flags in it.
func ParseHelp(name string, out []byte) (Doc, bool) {
	lines := splitLines(out)
	for _, compound := range []bool{false, true} {
		for _, line := range lines {
			syn, ok := topicSynopsis(name, line, compound)
			if !ok {
				continue
			}
			if syn == "" {
				return Doc{}, false
			}
			return Doc{Name: name, Synopsis: clip(syn, maxSynopsis), Options: helpOptions(syn), Source: "help"}, true
		}
	}
	return Doc{}, false
}

// topicSynopsis reads the first line of a help topic: "name: synopsis", or
// with compound set "name ... closer: synopsis".
func topicSynopsis(name, line string, compound bool) (string, bool) {
	if !compound {
		if rest, ok := strings.CutPrefix(line, name+": "); ok {
			return strings.TrimSpace(rest), true
		}
		return "", false
	}
	title, rest, ok := strings.Cut(line, ": ")
	if !ok || !strings.HasPrefix(title, name+" ... ") || strings.ContainsAny(strings.TrimPrefix(title, name+" ... "), " \t") {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

func helpOptions(syn string) []Option {
	var opts []Option
	seen := map[string]bool{}
	add := func(flag, arg string) {
		if !seen[flag] && len(opts) < maxOptions {
			seen[flag] = true
			opts = append(opts, Option{Flag: flag, Arg: arg})
		}
	}
	for _, m := range bracketed.FindAllStringSubmatch(syn, -1) {
		alts := strings.Split(m[1], "|")
		for k, flag := range alts {
			arg := ""
			if k == len(alts)-1 {
				arg = m[2] // the argument belongs to the last alternative
			}
			if strings.HasPrefix(flag, "--") || len(flag) == 2 {
				add(flag, arg)
				continue
			}
			letters := flag[1:] // a cluster such as -ers: one option per letter, the argument goes to the last
			for i := 0; i < len(letters); i++ {
				a := ""
				if i == len(letters)-1 {
					a = arg
				}
				add("-"+letters[i:i+1], a)
			}
		}
	}
	return opts
}

var (
	optionLine = regexp.MustCompile(`^(-{1,2}[^\s,\-][^\s,]*)`)
	aliasFlags = regexp.MustCompile(`^(,\s*-\S+)+`)
)

// ParseMan reads the output of man (rendered text, overstrike allowed). The
// synopsis is the SYNOPSIS section, one trimmed line per line; a page without
// one gives no doc. Options come from the lines after it that start with
// white space and then a flag, such as "     -A      Include ...": the flag,
// an argument written after one space ("-D format"), and the description on
// the same line or on the next, deeper one. Lines indented deeper than an
// option line (wrapped description) are not read as options.
func ParseMan(name string, out []byte) (Doc, bool) {
	lines := splitLines(StripOverstrike(out))
	start := -1
	for i, l := range lines {
		if l == "SYNOPSIS" {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return Doc{}, false
	}
	end := start
	var syn []string
	for ; end < len(lines); end++ {
		l := lines[end]
		if l != "" && !isIndented(l) {
			break
		}
		if t := strings.TrimSpace(l); t != "" {
			syn = append(syn, t)
		}
	}
	if len(syn) == 0 {
		return Doc{}, false
	}
	d := Doc{Name: name, Synopsis: clip(strings.Join(syn, "\n"), maxSynopsis), Source: "man"}
	seen := map[string]bool{}
	for i := end; i < len(lines) && len(d.Options) < maxOptions; i++ {
		indent := indentOf(lines[i])
		if indent == 0 || indent > maxOptionIndent {
			continue
		}
		rest := lines[i][len(lines[i])-len(strings.TrimLeft(lines[i], " \t")):]
		m := optionLine.FindString(rest)
		if m == "" {
			continue
		}
		rest = rest[len(m):]
		flag, arg := strings.TrimSpace(m), ""
		if k := strings.IndexAny(flag, "=["); k > 0 {
			flag, arg = flag[:k], strings.Trim(flag[k:], "=[]<>_ \u00a0")
		}
		if arg == "" && strings.HasPrefix(rest, " ") && !strings.HasPrefix(rest, "  ") {
			fields := strings.Fields(rest)
			tok, after := "", ""
			if len(fields) > 0 {
				tok = fields[0]
				after = rest[strings.Index(rest, tok)+len(tok):]
			}
			switch {
			case tok != "" && !strings.HasPrefix(tok, "-") && (after == "" || strings.HasPrefix(after, "  ")):
				arg = strings.Trim(tok, "<>[]_")
				rest = after
			case placeholder.MatchString(tok) && strings.HasSuffix(tok, ","):
				// "-A num, --after-context=num": the other spellings follow.
				arg = strings.TrimSuffix(tok, ",")
				rest = dropFlags(after)
			case placeholder.MatchString(tok) && sentenceStart(after):
				// "-c string If the -c option ...": a one-space gap, then a sentence.
				arg = tok
				rest = after
			}
		}
		rest = aliasFlags.ReplaceAllString(rest, "")
		text := strings.TrimSpace(rest)
		if text == "" && i+1 < len(lines) && strings.TrimSpace(lines[i+1]) != "" && indentOf(lines[i+1]) > indent {
			text = strings.TrimSpace(lines[i+1])
		}
		if seen[flag] {
			continue
		}
		seen[flag] = true
		d.Options = append(d.Options, Option{Flag: flag, Arg: arg, Text: clip(text, maxOptionText)})
	}
	return d, true
}

// placeholder is the lower-case word a man page uses for an argument: num,
// string, file.
var placeholder = regexp.MustCompile(`^[a-z][a-z0-9_]*,?$`)

// sentenceStart reports whether s is one space and then a capital letter: the
// start of a description after an argument.
func sentenceStart(s string) bool {
	return len(s) > 2 && s[0] == ' ' && s[1] >= 'A' && s[1] <= 'Z'
}

// dropFlags removes the other spellings of an option (`--after-context=num`,
// `-B num`) that follow its first one and a comma.
func dropFlags(rest string) string {
	for {
		t := strings.TrimLeft(rest, " ")
		if !strings.HasPrefix(t, "-") {
			return rest
		}
		i := strings.IndexByte(t, ' ')
		if i < 0 {
			return ""
		}
		rest = t[i:]
	}
}

func isIndented(l string) bool { return l[0] == ' ' || l[0] == '\t' }

// indentOf counts leading columns (a tab is eight).
func indentOf(l string) int {
	n := 0
	for _, c := range l {
		switch c {
		case ' ':
			n++
		case '\t':
			n += 8
		default:
			return n
		}
	}
	return 0 // blank
}

// clip cuts s to at most n bytes at a character boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

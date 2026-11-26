// Package editorconfig reads the subset of .editorconfig files that the
// formatter needs: sections whose glob is one of a few supported shapes, and
// the properties of the files that apply to one path. It starts nothing and
// reads only through the function it is given.
//
// Supported globs: `*` (not across `/`), `**` (across `/`), literal text, and
// one level of `{a,b}` alternatives. A glob without a `/` matches the file
// name at any depth under the .editorconfig's directory; one with a `/`
// matches the path relative to that directory (a leading `/` is dropped).
// Any other glob (`?`, `[seq]`, `{n1..n2}`, nested or single-item braces,
// backslash escapes) is not supported: its section is skipped and
// LookupNotes reports it.
package editorconfig

import (
	"fmt"
	"regexp"
	"strings"
)

// maxFileBytes is the largest .editorconfig that is read; a larger file is
// treated as missing.
const maxFileBytes = 64 << 10

const name = ".editorconfig"

// Lookup returns the properties that apply to path: the .editorconfig files
// from path's directory up to root, farther files first so that closer ones
// override them, and within one file later sections override earlier ones.
// Keys and values are lower-cased; the value "unset" removes a property. The
// walk stops after a file whose preamble says `root = true`, and at root. A
// path outside root reads only its own directory's file. read returns a
// file's bytes and whether it exists.
func Lookup(path, root string, read func(string) ([]byte, bool)) map[string]string {
	props, _ := LookupNotes(path, root, read)
	return props
}

// LookupNotes is Lookup that also returns one line for every section skipped
// for an unsupported glob, as "FILE:LINE: glob X not supported".
func LookupNotes(file, root string, read func(string) ([]byte, bool)) (map[string]string, []string) {
	var dirs []string
	file = strings.ReplaceAll(file, "\\", "/")
	root = strings.ReplaceAll(root, "\\", "/")
	dir := slashDir(file)
	rootDir := strings.TrimRight(root, "/")
	inside := root != "" && (dir == rootDir || strings.HasPrefix(dir, rootDir+"/"))
	for {
		dirs = append(dirs, dir)
		if !inside || dir == rootDir || dir == "" || dir == "/" {
			break
		}
		dir = slashDir(dir)
	}
	// dirs is closest first. Find where root = true stops the walk.
	type parsed struct {
		dir      string
		sections []section
		isRoot   bool
	}
	var files []parsed
	var notes []string
	for _, d := range dirs {
		p := d + "/" + name
		b, ok := read(p)
		if !ok || len(b) > maxFileBytes {
			continue
		}
		secs, isRoot, n := parse(p, string(b))
		notes = append(notes, n...)
		files = append(files, parsed{d, secs, isRoot})
		if isRoot {
			break
		}
	}
	props := map[string]string{}
	for i := len(files) - 1; i >= 0; i-- {
		f := files[i]
		rel := strings.TrimPrefix(strings.TrimPrefix(file, f.dir), "/")
		for _, s := range f.sections {
			if !s.re.MatchString(rel) {
				continue
			}
			for _, kv := range s.props {
				if kv[1] == "unset" {
					delete(props, kv[0])
				} else {
					props[kv[0]] = kv[1]
				}
			}
		}
	}
	return props, notes
}

type section struct {
	re    *regexp.Regexp
	props [][2]string
}

// slashDir is the directory of p, with forward slashes only; "" for a bare
// name.
func slashDir(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	i := strings.LastIndexByte(p, '/')
	switch {
	case i < 0:
		return ""
	case i == 0:
		return "/"
	}
	return p[:i]
}

// parse reads one file. Lines starting with # or ; are comments; there are no
// inline comments. A property line outside a section is a preamble line:
// `root = true` is the only one that matters here.
func parse(file, text string) (secs []section, isRoot bool, notes []string) {
	var cur *section
	skipping := false
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "" || line[0] == '#' || line[0] == ';':
			continue
		case line[0] == '[' && line[len(line)-1] == ']':
			glob := line[1 : len(line)-1]
			re, ok := compile(glob)
			if !ok {
				notes = append(notes, fmt.Sprintf("%s:%d: glob %s not supported", file, i+1, glob))
				cur, skipping = nil, true
				continue
			}
			secs = append(secs, section{re: re})
			cur, skipping = &secs[len(secs)-1], false
		default:
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			k, v = strings.ToLower(strings.TrimSpace(k)), strings.ToLower(strings.TrimSpace(v))
			switch {
			case cur != nil:
				cur.props = append(cur.props, [2]string{k, v})
			case !skipping && k == "root" && v == "true":
				isRoot = true
			}
		}
	}
	return secs, isRoot, notes
}

// compile turns a supported glob into an anchored regular expression over a
// slash-separated path relative to the .editorconfig's directory.
func compile(glob string) (*regexp.Regexp, bool) {
	anchored := strings.HasPrefix(glob, "/")
	glob = strings.TrimPrefix(glob, "/")
	if strings.Contains(glob, "/") {
		anchored = true
	}
	var b strings.Builder
	b.WriteString("^")
	if !anchored {
		b.WriteString("(?:.*/)?")
	}
	for i := 0; i < len(glob); i++ {
		switch c := glob[i]; c {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString("[^/]*")
			}
		case '{':
			j := strings.IndexByte(glob[i:], '}')
			if j < 0 {
				return nil, false
			}
			body := glob[i+1 : i+j]
			alts := strings.Split(body, ",")
			if len(alts) < 2 || strings.ContainsAny(body, "{*?[\\") || strings.Contains(body, "..") && !strings.Contains(body, ",") {
				return nil, false
			}
			b.WriteString("(?:")
			for k, a := range alts {
				if k > 0 {
					b.WriteString("|")
				}
				b.WriteString(regexp.QuoteMeta(a))
			}
			b.WriteString(")")
			i += j
		case '?', '[', ']', '}', '\\':
			return nil, false
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	return re, err == nil
}

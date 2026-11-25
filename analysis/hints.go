package analysis

import (
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/wamra/bash-lsp/tree"
)

// maxHints is the most inlay hints one request returns.
const maxHints = 5000

// Hint is text an editor shows inline at Pos. Param marks a parameter-name
// hint (LSP InlayHintKind 2); a source hint carries no kind.
type Hint struct {
	Pos   tree.Pos
	Label string
	Param bool
}

// Hints returns the inline hints of path whose position lies in within, in
// position order, at most maxHints (the first ones):
//
//   - sources: after the path word of a `source` that resolved by a directive,
//     the script's own directory or a loop glob, `→ lib/util.sh`, or
//     `→ N files` for several targets. A literal path shows what it says and
//     gets none.
//   - params: before each argument of a call to a script function whose
//     parameter at that position has a name, `name:`.
//
// A path the snapshot does not hold gives nil, and so does an inverted range.
func (s *Snapshot) Hints(path string, within tree.Range, sources, params bool) []Hint {
	file, ok := s.idx[path]
	if !ok || (!sources && !params) {
		return nil
	}
	ff := s.facts[file]
	if ff == nil || ff.File == nil {
		return nil
	}
	lo, hi := within.Start.Offset, within.End.Offset
	var out []Hint
	if sources {
		out = s.sourceHints(out, file, lo, hi)
	}
	if params {
		out = s.paramHints(out, file, lo, hi)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Pos.Offset < out[j].Pos.Offset })
	if len(out) > maxHints {
		out = out[:maxHints]
	}
	return out
}

func (s *Snapshot) sourceHints(out []Hint, file, lo, hi int) []Hint {
	for _, e := range s.edges[file] {
		if e.Status != SourceResolved || len(e.Targets) == 0 || e.Ref.Word == nil {
			continue
		}
		switch e.How {
		case "directive", "script-dir", "loop-glob":
		default:
			continue
		}
		end := e.Ref.Word.Range.End
		if end.Offset < lo || end.Offset > hi {
			continue
		}
		label := "→ " + s.relative(e.Targets[0])
		if len(e.Targets) > 1 {
			label = "→ " + strconv.Itoa(len(e.Targets)) + " files"
		}
		out = append(out, Hint{Pos: end, Label: label})
	}
	return out
}

// relative is path as the workspace root sees it, with forward slashes; a
// path outside the root stays absolute.
func (s *Snapshot) relative(path string) string {
	if s.root != "" {
		if rel, err := filepath.Rel(s.root, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(path)
}

func (s *Snapshot) paramHints(out []Hint, file, lo, hi int) []Hint {
	want := map[*Use][]*tree.Node{}
	for _, u := range s.facts[file].Uses {
		if u.Kind != UseCall || u.Via != ViaNone || u.ViaAlias || u.Node == nil {
			continue
		}
		if u.Node.Range.End.Offset < lo || u.Node.Range.Start.Offset > hi { // the call, not just its command word
			continue
		}
		if words := callWords(u.Node); len(words) > 1 {
			want[u] = words
		}
	}
	if len(want) == 0 {
		return out
	}
	src := s.facts[file].File.Src
	sigs := map[*Symbol]*FuncSignature{}
	for _, b := range s.bindings[file] {
		words, ok := want[b.Use]
		if !ok || b.Sym == nil || b.Sym.Kind != DeclFunction {
			continue
		}
		delete(want, b.Use) // the first function binding decides, as for signature help
		sig, seen := sigs[b.Sym]
		if !seen {
			if v, ok := s.Signature(b.Sym); ok {
				sig = &v
			}
			sigs[b.Sym] = sig
		}
		if sig == nil || b.Reason == ReasonAlias {
			continue // a call through an alias: the alias may add arguments
		}
		for i, w := range words[1:] {
			if i >= len(sig.Params) || isSpread(w, src) {
				break // past the named parameters, or the position of this argument is unknown
			}
			name := sig.Params[i].Name
			start := w.Range.Start
			if name == "" || start.Offset < lo || start.Offset > hi {
				continue
			}
			out = append(out, Hint{Pos: start, Label: name + ":", Param: true})
		}
	}
	return out
}

// isSpread reports whether the word w contains an expansion to any number of
// words: `$@`, `$*`, `${@...}`, `${*...}` or `${name[@]...}` (also `${!name[@]}`),
// anywhere in the word, quoted or not. A count `${#name[@]}` and a
// single-quoted text are one word.
func isSpread(w *tree.Node, src []byte) bool {
	a, b := w.Range.Start.Offset, w.Range.End.Offset
	if a < 0 || b > len(src) || a >= b {
		return false
	}
	t := string(src[a:b])
	inSingle := false
	for i := 0; i < len(t); i++ {
		switch c := t[i]; {
		case c == '\\':
			i++
		case c == '\'':
			inSingle = !inSingle
		case inSingle || c != '$' || i+1 >= len(t):
		case t[i+1] == '@' || t[i+1] == '*':
			return true
		case t[i+1] == '{':
			body := t[i+2:]
			if end := strings.IndexByte(body, '}'); end >= 0 {
				body = body[:end]
			}
			if strings.HasPrefix(body, "#") {
				continue
			}
			if strings.HasPrefix(body, "@") || strings.HasPrefix(body, "*") ||
				strings.Contains(body, "[@]") || strings.Contains(body, "[*]") {
				return true
			}
		}
	}
	return false
}

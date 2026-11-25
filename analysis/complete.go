package analysis

import (
	"bytes"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/wamra/bash-lsp/tree"
)

// CompletionContext says what the cursor is completing.
type CompletionContext uint8

const (
	// CtxCommand is a command word. CtxArgument is any other word of a simple
	// command; the command is the second result of Completions. CtxOption is
	// a word that starts with '-' after a command that has a name. CtxVariable
	// is a variable name after `$` or `${`. CtxParamOp is the operator after
	// `${name`. CtxSourcePath is the file of `source` and `.`.
	CtxCommand CompletionContext = iota + 1
	CtxArgument
	CtxOption
	CtxVariable
	CtxParamOp
	CtxSourcePath
)

// CompletionKind says what an item stands for.
type CompletionKind uint8

const (
	CompFunction CompletionKind = iota + 1
	CompVariable
	CompAlias
	CompBuiltin
	CompKeyword
	CompFile
	CompFolder
	CompParamOp
)

// CompletionItem is one proposal. Insert replaces Replace. Sym is the script
// symbol behind a function, alias or variable (nil for an environment name).
// Feature is the FeatureInfo.Key of the bash version the item needs, "" when
// every bash has it; Detail already says so for the default target, and a
// caller with another target can redo it with FeatureNeeds.
type CompletionItem struct {
	Label, Detail, Insert string
	Kind                  CompletionKind
	Replace               tree.Range
	Sym                   *Symbol
	Feature               string
}

// maxCompletionItems caps the items of one answer.
const maxCompletionItems = 1000

// MaxCompletionItems is the cap of one answer: Completions never returns
// more, and a caller that adds items keeps to it.
const MaxCompletionItems = maxCompletionItems

// keywordNames are the reserved words of bash 3.2 (`compgen -k`), then the
// one later versions add. They are in the builtin list too.
var keywordNames = []string{
	"if", "then", "else", "elif", "fi", "case", "esac", "for", "select", "while", "until", "do",
	"done", "in", "function", "time", "{", "}", "!", "[[", "]]", "coproc",
}

var keywordSet = func() map[string]bool {
	m := map[string]bool{}
	for _, k := range keywordNames {
		m[k] = true
	}
	return m
}()

// paramOp is an operator that may follow `${name`.
type paramOp struct{ op, detail, feature string }

var paramOps = []paramOp{
	{":-", "use a default value", ""},
	{":=", "assign a default value", ""},
	{":?", "fail when unset or empty", ""},
	{":+", "use an alternative value", ""},
	{"#", "remove the shortest matching prefix", ""},
	{"##", "remove the longest matching prefix", ""},
	{"%", "remove the shortest matching suffix", ""},
	{"%%", "remove the longest matching suffix", ""},
	{"/", "replace a match", ""},
	{"^", "upper-case the first character", "case-modification"},
	{"^^", "upper-case every character", "case-modification"},
	{",", "lower-case the first character", "case-modification"},
	{",,", "lower-case every character", "case-modification"},
	{"@Q", "quote the value", "transform-op"},
}

// FeatureNeeds words the version a feature needs when it is above min, the
// first release series of the target: `needs bash 4.0; target starts at 3.2`.
// gated is false, and the text empty, for a feature min has, a feature with
// no version, and an unknown key.
func FeatureNeeds(key string, min BashVersion) (detail string, gated bool) {
	f, ok := featureByKey[key]
	if !ok || !min.Less(f.MinBash) {
		return "", false
	}
	return fmt.Sprintf("needs bash %s; target starts at %s", f.MinBash, min), true
}

// reservedLead are the words after which a command word follows.
var reservedLead = map[string]bool{
	"if": true, "then": true, "else": true, "elif": true, "while": true, "until": true,
	"do": true, "{": true, "!": true, "time": true,
}

var (
	assignWord = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	identName  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	plainCmd   = regexp.MustCompile(`^[A-Za-z0-9._+:@%/\[-]+$`)
)

// lineState is what the text of one line, up to the cursor, says about the
// word at the cursor.
type lineState struct {
	words    []string // the words of the current command before the cursor, assignments and redirects left out
	cur      int      // index in the line of the word being typed, -1 when the cursor is between words
	quote    byte     // the quote the cursor is inside, 0 for none
	comment  bool     // the cursor is in a comment
	redirect bool     // the word at the cursor is the target of a redirection
	arith    bool     // the cursor is inside (( )) or $(( )), where words are names and numbers
}

type frame struct {
	kind     byte
	words    []string
	cur      int
	redirect bool
}

// scanLine reads t, the text of a line up to the cursor, as a shell does for
// word boundaries: quotes, `${ }`, the separators that start a command, the
// reserved words that are followed by one, and redirections. quote is the
// quote a multi-line string carries into the line.
func scanLine(t []byte, quote byte) lineState {
	st := lineState{cur: -1, quote: quote}
	var words []string
	var stack []frame
	var curRedir, redir bool
	pdepth := 0
	if quote != 0 {
		st.cur = 0 // a string is a word
	}
	endWord := func(i int) {
		if st.cur < 0 {
			return
		}
		w := string(t[st.cur:i])
		st.cur = -1
		if curRedir {
			curRedir = false
			return
		}
		if len(words) == 0 && (assignWord.MatchString(w) || reservedLead[w]) {
			return
		}
		words = append(words, w)
	}
	begin := func(i int) {
		if st.cur < 0 {
			st.cur, curRedir, redir = i, redir, false
		}
	}
	push := func(kind byte) {
		stack = append(stack, frame{kind, words, st.cur, curRedir})
		words, st.cur, curRedir, redir = nil, -1, false, false
	}
	next := func(i int) byte {
		if i+1 < len(t) {
			return t[i+1]
		}
		return 0
	}
	for i := 0; i < len(t); i++ {
		c := t[i]
		if st.quote != 0 {
			if c == '\\' && st.quote == '"' {
				i++
			} else if c == st.quote {
				st.quote = 0
			}
			continue
		}
		if pdepth > 0 {
			switch {
			case c == '}':
				pdepth--
			case c == '{' && i > 0 && t[i-1] == '$':
				pdepth++
			}
			continue
		}
		switch {
		case c == '\\':
			begin(i)
			i++
		case c == '\'' || c == '"':
			begin(i)
			st.quote = c
		case c == '$' && next(i) == '{':
			begin(i)
			pdepth++
			i++
		case c == '$' && next(i) == '(' && i+2 < len(t) && t[i+2] == '(':
			begin(i)
			push('a')
			push('a')
			i += 2
		case c == '(' && next(i) == '(':
			endWord(i)
			push('a')
			push('a')
			i++
		case c == '$' && next(i) == '(', (c == '<' || c == '>') && next(i) == '(':
			begin(i)
			push('(')
			i++
		case c == '(':
			endWord(i)
			push('(')
		case c == ')':
			endWord(i)
			if n := len(stack); n > 0 && stack[n-1].kind != '`' {
				f := stack[n-1]
				stack = stack[:n-1]
				words, st.cur, curRedir = f.words, f.cur, f.redirect
			} else {
				words, redir = nil, false // the end of a case pattern
			}
		case c == '`':
			if n := len(stack); n > 0 && stack[n-1].kind == '`' {
				endWord(i)
				f := stack[n-1]
				stack = stack[:n-1]
				words, st.cur, curRedir = f.words, f.cur, f.redirect
			} else {
				begin(i)
				push('`')
			}
		case c == ';' || c == '|' || c == '&' && next(i) != '>':
			endWord(i)
			words, redir = nil, false
		case c == '<' || c == '>' || c == '&':
			if st.cur >= 0 && allDigits(string(t[st.cur:i])) {
				st.cur = -1 // the file descriptor of `2>`
			}
			endWord(i)
			for i+1 < len(t) && strings.IndexByte("<>&|", t[i+1]) >= 0 && t[i+1] != '(' {
				i++
			}
			redir = true
		case c == ' ' || c == '\t':
			endWord(i)
		case c == '#' && st.cur < 0:
			st.comment = true
			st.words = words
			return st
		default:
			begin(i)
		}
	}
	st.words = words
	st.redirect = st.cur >= 0 && curRedir || st.cur < 0 && redir
	for _, f := range stack {
		st.arith = st.arith || f.kind == 'a'
	}
	return st
}

// site is the answer to "what is being completed here".
type site struct {
	ctx    CompletionContext
	cmd    string
	prefix string     // the text before the cursor that the items replace
	rng    tree.Range // what the items replace
	word   string     // the whole word before the cursor
	file   int
	f      *tree.File
	name   string // CtxParamOp: the variable name
}

// locateSite works out the context at offset: the text of the line before the
// cursor gives the token, the nodes around it say whether the cursor is in a
// comment, a here-document or a single-quoted string that began on an earlier
// line.
func (s *Snapshot) locateSite(path string, offset int) (site, bool) {
	file, ok := s.idx[path]
	if !ok || s.facts[file].File == nil || s.facts[file].File.Root == nil {
		return site{}, false
	}
	f := s.facts[file].File
	offset = max(0, min(offset, len(f.Src)))
	cursor := f.PosAt(offset)
	lineStart := offset - (cursor.Col - 1)
	if lineStart == 0 && bytes.HasPrefix(f.Src, []byte("\xef\xbb\xbf")) {
		if offset < 3 {
			return site{}, false
		}
		lineStart = 3 // the byte order mark is not text of the line
	}
	quote := byte(0)
	if offset > 0 {
		for n := f.NewLocator().At(offset - 1); n != nil; n = n.Parent {
			multi := n.Range.Start.Line < cursor.Line
			switch {
			case n.Kind == tree.KComment || n.Kind == tree.KHeredoc && multi:
				return site{}, false
			case n.Kind == tree.KSglQuoted && multi && offset < n.Range.End.Offset:
				return site{}, false
			case n.Kind == tree.KDblQuoted && multi && offset < n.Range.End.Offset && quote == 0:
				quote = '"'
			}
		}
	}
	text := f.Src[lineStart:offset]
	st := scanLine(text, quote)
	if st.comment || st.quote == '\'' {
		return site{}, false
	}
	out := site{file: file, f: f}
	word := ""
	if st.cur >= 0 {
		word = string(text[st.cur:])
	}
	out.word = word
	if len(st.words) > 0 && plainCmd.MatchString(st.words[0]) {
		out.cmd = st.words[0]
	}
	span := func(from int) tree.Range { return tree.Range{Start: f.PosAt(offset - from), End: cursor} }

	// A variable: the last unescaped `$` of the word.
	if k := lastDollar(word); k >= 0 {
		tail := word[k+1:]
		switch {
		case tail == "" || identName.MatchString(tail):
			out.ctx, out.prefix, out.rng = CtxVariable, tail, span(len(tail))
			return out, true
		case tail == "{":
			out.ctx, out.rng = CtxVariable, span(0)
			return out, true
		case strings.HasPrefix(tail, "{") && identName.MatchString(tail[1:]):
			out.ctx, out.name, out.prefix, out.rng = CtxParamOp, tail[1:], tail[1:], span(len(tail)-1)
			return out, true
		}
	}
	if st.quote != 0 {
		return site{}, false
	}
	if st.arith {
		name := word[len(word)-identTail(word):]
		out.ctx, out.prefix, out.rng = CtxVariable, name, span(len(name))
		return out, true
	}
	out.prefix, out.rng = word, span(len(word))
	switch {
	case st.redirect:
		out.ctx = CtxArgument
	case len(st.words) == 0:
		if assignWord.MatchString(word) {
			out.ctx = CtxArgument // the value of an assignment
			return out, true
		}
		out.ctx = CtxCommand
	case strings.HasPrefix(word, "-") && out.cmd != "":
		out.ctx = CtxOption
	case (out.cmd == "source" || out.cmd == ".") && len(st.words) == 1:
		out.ctx = CtxSourcePath
		base := word
		if i := strings.LastIndexByte(word, '/'); i >= 0 {
			base = word[i+1:]
		}
		out.prefix, out.rng = base, span(len(base))
	default:
		out.ctx = CtxArgument
	}
	return out, true
}

// identTail is the length of the variable name that ends w, 0 when w ends in
// anything else.
func identTail(w string) int {
	n := 0
	for n < len(w) {
		c := w[len(w)-1-n]
		if c != '_' && (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			break
		}
		n++
	}
	for n > 0 && w[len(w)-n] >= '0' && w[len(w)-n] <= '9' {
		n-- // a name does not start with a digit
	}
	return n
}

// lastDollar is the index of the last `$` of w that no backslash escapes, -1
// for none.
func lastDollar(w string) int {
	for i := len(w) - 1; i >= 0; i-- {
		if w[i] != '$' {
			continue
		}
		n := 0
		for j := i - 1; j >= 0 && w[j] == '\\'; j-- {
			n++
		}
		if n%2 == 0 {
			return i
		}
	}
	return -1
}

// CompletionWord is the text before the cursor that completion replaces and
// the range it spans (empty at the cursor when nothing is typed yet). ok is
// false where nothing completes: an unknown file, a comment or a
// single-quoted string.
func (s *Snapshot) CompletionWord(path string, offset int) (prefix string, rng tree.Range, ok bool) {
	st, ok := s.locateSite(path, offset)
	if !ok {
		return "", tree.Range{}, false
	}
	return st.prefix, st.rng, true
}

type ranked struct {
	rank int
	it   CompletionItem
}

// Completions proposes what can stand at offset of path. cmd is the command
// word the cursor's word belongs to, "" at a command word and when the word
// is not literal. Items come in a fixed order: nearer scopes and kinds first,
// then by name. incomplete says more than maxCompletionItems matched and the
// rest was left out. It reads the snapshot only: nothing is run and no file
// is opened. PATH commands and the options of a command are not here, they
// need the machine (see the callers).
func (s *Snapshot) Completions(path string, offset int) (ctx CompletionContext, cmd string, items []CompletionItem, incomplete bool) {
	st, ok := s.locateSite(path, offset)
	if !ok {
		return 0, "", nil, false
	}
	var rs []ranked
	switch st.ctx {
	case CtxVariable:
		rs = s.variableItems(st, 0)
	case CtxParamOp:
		for _, p := range paramOps {
			it := CompletionItem{Label: p.op, Insert: p.op, Kind: CompParamOp, Detail: p.detail, Feature: p.feature,
				Replace: tree.Range{Start: st.rng.End, End: st.rng.End}}
			if d, gated := FeatureNeeds(p.feature, DefaultTarget.Min); gated {
				it.Detail = d
			}
			rs = append(rs, ranked{-1, it})
		}
		rs = append(rs, s.variableItems(st, len(st.name)+1)...)
	case CtxCommand:
		if !strings.ContainsAny(st.word, "/$`'\"\\") {
			rs = s.commandItems(st)
		}
	case CtxSourcePath:
		rs = s.sourceItems(st, path)
	}
	slices.SortStableFunc(rs, func(a, b ranked) int {
		if a.rank != b.rank {
			return a.rank - b.rank
		}
		return strings.Compare(a.it.Label, b.it.Label)
	})
	if len(rs) > maxCompletionItems {
		rs, incomplete = rs[:maxCompletionItems], true
	}
	for _, r := range rs {
		items = append(items, r.it)
	}
	return st.ctx, st.cmd, items, incomplete
}

// variableItems are the variables in view at the cursor whose name starts
// with the prefix and is at least minLen long: the private and local ones of
// the scopes around the cursor, the globals of the visible files and the
// names of the environment the file reads.
func (s *Snapshot) variableItems(st site, minLen int) []ranked {
	cursor := st.rng.End.Offset
	src := s.facts[st.file].File.Src
	have := map[string]int{}
	var out []ranked
	add := func(r ranked) {
		if i, dup := have[r.it.Label]; dup {
			if r.rank < out[i].rank {
				out[i] = r
			}
			return
		}
		have[r.it.Label] = len(out)
		out = append(out, r)
	}
	s.visible[st.file].each(func(f int) {
		for _, d := range s.facts[f].Decls {
			if d.Kind != DeclVariable || d.Temp || len(d.Name) < minLen || !strings.HasPrefix(d.Name, st.prefix) {
				continue
			}
			sym := s.declSym[d]
			if sym == nil {
				continue
			}
			rank, detail := 2, "variable"
			switch {
			case sym.Private != 0:
				if ps := privateScope(d.Scope); f != st.file || ps == nil || !around(ps.Node, src, cursor) {
					continue
				}
				rank, detail = 0, "variable of a "+privateName(sym.Private)
			case sym.File != "":
				if fn := funcBody(d.Scope); f != st.file || fn == nil || !around(fn, src, cursor) {
					continue
				}
				rank, detail = 1, "local variable"
			case d.Attrs.Exported:
				detail = "exported variable"
			}
			add(ranked{rank, CompletionItem{Label: d.Name, Insert: d.Name, Kind: CompVariable, Detail: detail, Replace: st.rng, Sym: sym}})
		}
	})
	for _, b := range s.bindings[st.file] {
		n := b.Use.Name
		if b.Reason != ReasonEnvironment || b.Use.Kind != UseVarRead || len(n) < minLen || n == st.prefix || !strings.HasPrefix(n, st.prefix) {
			continue
		}
		if _, dup := have[n]; !dup {
			add(ranked{3, CompletionItem{Label: n, Insert: n, Kind: CompVariable, Detail: "environment", Replace: st.rng}})
		}
	}
	return out
}

// around reports whether the cursor is inside n: after its first byte and
// before its end. At its end it is inside only for a node the parser closed
// itself, or one that runs to the end of the file (blanks may follow) with no
// closing brace or parenthesis (a function or subshell still being typed).
func around(n *tree.Node, src []byte, offset int) bool {
	if n == nil || n.Range.Start.Offset >= offset {
		return false
	}
	end := n.Range.End.Offset
	if offset < end {
		return true
	}
	return offset == end && (n.Recovered || end > 0 && src[end-1] != '}' && src[end-1] != ')' && len(bytes.TrimSpace(src[end:])) == 0)
}

// commandItems are the functions and aliases in view, the builtins and the
// reserved words whose name starts with the prefix.
func (s *Snapshot) commandItems(st site) []ranked {
	seen := map[string]bool{}
	var out []ranked
	add := func(rank int, it CompletionItem) {
		it.Insert, it.Replace = it.Label, st.rng
		out = append(out, ranked{rank, it})
	}
	s.visible[st.file].each(func(f int) {
		for _, d := range s.funcDecls[f] {
			if k := "f\x00" + d.Name; !seen[k] && strings.HasPrefix(d.Name, st.prefix) {
				seen[k] = true
				add(0, CompletionItem{Label: d.Name, Kind: CompFunction, Detail: "function in " + s.rel(s.paths[f]), Sym: s.declSym[d]})
			}
		}
		for _, d := range s.aliasDecls[f] {
			if k := "a\x00" + d.Name; !seen[k] && strings.HasPrefix(d.Name, st.prefix) {
				seen[k] = true
				detail := "alias"
				if d.AliasTo != "" {
					detail = "alias for " + d.AliasTo
				}
				add(1, CompletionItem{Label: d.Name, Kind: CompAlias, Detail: detail, Sym: s.declSym[d]})
			}
		}
	})
	for _, n := range BuiltinNames() {
		if !strings.HasPrefix(n, st.prefix) || keywordSet[n] {
			continue
		}
		add(2, CompletionItem{Label: n, Kind: CompBuiltin, Detail: "builtin"})
	}
	for _, n := range keywordNames {
		if !strings.HasPrefix(n, st.prefix) {
			continue
		}
		it := CompletionItem{Label: n, Kind: CompKeyword, Detail: "keyword"}
		if n == "coproc" {
			it.Feature = "coproc"
			if d, gated := FeatureNeeds(it.Feature, DefaultTarget.Min); gated {
				it.Detail = d
			}
		}
		add(3, it)
	}
	return out
}

// sourceItems are the files of the snapshot, and the folders that hold some,
// next to the typed path. The path is read relative to the directory of the
// file, and nothing outside the workspace root is offered. A word that is not
// a plain path (an expansion, a quote, `~`) offers nothing.
func (s *Snapshot) sourceItems(st site, path string) []ranked {
	w := st.word
	if s.root == "" || strings.ContainsAny(w, "$`'\"\\~*?[") {
		return nil
	}
	dirPart := w[:len(w)-len(st.prefix)]
	dir := filepath.Dir(path)
	if dirPart != "" {
		d := filepath.FromSlash(dirPart)
		if filepath.IsAbs(d) {
			dir = d
		} else {
			dir = filepath.Join(dir, d)
		}
	}
	dir = filepath.Clean(dir)
	if r, err := filepath.Rel(s.root, dir); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return nil
	}
	seen := map[string]bool{}
	var out []ranked
	for _, p := range s.paths {
		if p == path {
			continue
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		name, rest, deeper := strings.Cut(filepath.ToSlash(rel), "/")
		kind := CompFile
		if deeper && rest != "" {
			name += "/"
			kind = CompFolder
		}
		if !strings.HasPrefix(name, st.prefix) || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, ranked{int(kind), CompletionItem{Label: name, Insert: name, Kind: kind, Detail: s.rel(p), Replace: st.rng}})
	}
	return out
}

// SymbolDoc is the hover text of the symbol a completion item named: the
// function, alias or variable called name as path sees it.
func (s *Snapshot) SymbolDoc(path string, kind DeclKind, name string) (markdown string, ok bool) {
	file, found := s.idx[path]
	if !found {
		return "", false
	}
	var sym *Symbol
	switch kind {
	case DeclFunction:
		sym = s.pick(s.funcSyms[name], file)
	case DeclAlias:
		sym = s.pick(s.aliasSyms[name], file)
	case DeclVariable:
		if sym = s.pick(s.varSyms[name], file); sym == nil {
			ds := append([]*Decl(nil), s.facts[file].Decls...)
			sort.SliceStable(ds, func(i, j int) bool { return ds[i].NameRange.Start.Offset < ds[j].NameRange.Start.Offset })
			for _, d := range ds {
				if d.Kind == DeclVariable && d.Name == name && s.declSym[d] != nil {
					sym = s.declSym[d]
					break
				}
			}
		}
	}
	if sym == nil {
		return "", false
	}
	switch sym.Kind {
	case DeclFunction:
		return s.hoverFunction(sym), true
	case DeclAlias:
		return s.hoverAlias(sym), true
	}
	return s.hoverVariable(sym), true
}

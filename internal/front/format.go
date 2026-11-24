package front

import (
	"bytes"
	"errors"
	"fmt"
	"hash/maphash"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/wamra/bash-lsp/internal/front/syntax"
	"github.com/wamra/bash-lsp/tree"
)

// ErrFormatSyntax is wrapped by every refusal that comes from the text itself
// (a syntax error, a node the parser recovered, a nesting limit); the message
// after it is "line:col: text". ErrFormatUnsupported is wrapped by every other
// refusal: a dialect or a file shape the formatter does not handle, a size
// over the cap, and output that would change the script's structure.
var (
	ErrFormatSyntax      = errors.New("syntax error")
	ErrFormatUnsupported = errors.New("not formatted")
)

// maxFormatBytes is the largest file that is formatted; it is the size at
// which analysis stops reading a file.
const maxFormatBytes = 2 << 20

// FormatOptions are the printer's options. Indent is the number of spaces
// per level, 0 for tabs.
type FormatOptions struct {
	Indent                                                                       uint
	BinaryNextLine, SwitchCaseIndent, SpaceRedirects, FunctionNextLine, Simplify bool
}

func (o FormatOptions) printer() *syntax.Printer {
	return syntax.NewPrinter(
		syntax.Indent(o.Indent),
		syntax.BinaryNextLine(o.BinaryNextLine),
		syntax.SwitchCaseIndent(o.SwitchCaseIndent),
		syntax.SpaceRedirects(o.SpaceRedirects),
		syntax.FunctionNextLine(o.FunctionNextLine),
	)
}

func syntaxErr(line, col int, msg string) error {
	return fmt.Errorf("%w: %d:%d: %s", ErrFormatSyntax, line, col, msg)
}

func unsupported(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrFormatUnsupported, fmt.Sprintf(format, a...))
}

// prepared is a file that passed every check that does not need a strict
// parse: the text to parse (line endings normalised to LF) and how to put the
// endings back.
type prepared struct {
	text []byte
	crlf bool
	d    Dialect
}

// restore converts printed text back to the file's line endings.
func (p prepared) restore(b []byte) []byte {
	if p.crlf {
		return bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n"))
	}
	return b
}

// prepare refuses what the formatter does not handle and what analysis
// rejects. It never changes src.
//
//   - over the size cap, NUL bytes (the lexer drops them from words, so the
//     printed text would lose them) and a byte order mark (the shell reads it
//     as part of the first word): not formatted;
//   - line endings: all LF, or all CRLF (formatted as LF and converted back);
//     a lone CR or a mix is not formatted;
//   - zsh: not formatted;
//   - any syntax error, gap, recovered node or limit that analysis of the same
//     bytes reports: ErrFormatSyntax with the first error.
func prepare(path string, src []byte) (prepared, error) { return prepareWith(path, src, Bash) }

// prepareWith is prepare with fallback as the dialect of a file that names none
// (DetectWith).
func prepareWith(path string, src []byte, fallback Dialect) (prepared, error) {
	if len(src) > maxFormatBytes {
		return prepared{}, unsupported("file is %d bytes, over the %d byte limit", len(src), maxFormatBytes)
	}
	if bytes.IndexByte(src, 0) >= 0 {
		return prepared{}, unsupported("file contains NUL bytes")
	}
	if bytes.HasPrefix(src, []byte("\xef\xbb\xbf")) {
		return prepared{}, unsupported("file starts with a byte order mark")
	}
	p := prepared{text: src}
	if cr := bytes.Count(src, []byte("\r")); cr > 0 {
		if cr != bytes.Count(src, []byte("\r\n")) || cr != bytes.Count(src, []byte("\n")) {
			return prepared{}, unsupported("line endings are mixed or a carriage return stands alone")
		}
		p.crlf = true
		p.text = bytes.ReplaceAll(src, []byte("\r\n"), []byte("\n"))
	}
	p.d = DetectWith(path, src, fallback)
	if p.d == Zsh {
		return prepared{}, unsupported("zsh is not supported yet")
	}
	f := Parse(path, p.text, p.d)
	if f.Skipped != "" && len(f.Errors) == 0 {
		return prepared{}, unsupported("%s", f.Skipped)
	}
	if len(f.Errors) > 0 {
		e := f.Errors[0]
		return prepared{}, syntaxErr(e.Range.Start.Line, e.Range.Start.Col, e.Msg)
	}
	var bad error
	f.Root.Walk(func(n *tree.Node) bool {
		if bad == nil && (n.Kind == tree.KErrorGap || n.Recovered) {
			bad = syntaxErr(n.Range.Start.Line, n.Range.Start.Col, "the parser recovered part of this statement")
		}
		return bad == nil
	})
	if bad != nil {
		return prepared{}, bad
	}
	return p, nil
}

// strictParse parses text with recovery off. An sh file is read with the
// POSIX grammar and, only when that has an error, with the bash grammar: the
// one analysis settled on (shGrammar). lang is the grammar used.
//
// The POSIX grammar takes `[[` for a command, so `[[ $a<$b ]]` is a test and a
// redirection to it and `=~ a|b` a pipe: valid bash, read as other code. For an
// sh file both grammars therefore read the text, and it is formatted only when
// the one that is used does not differ from the other: both print the same
// text, or only the POSIX grammar reads it. Text that only bash reads, or that
// the two read differently, is refused as grammar-dependent. A here-document
// whose delimiter is quoted in one part and not in another is refused too.
func strictParse(path string, text []byte, d Dialect, o FormatOptions) (f *syntax.File, lang syntax.LangVariant, err error) {
	if d != POSIX {
		f, err = parseStrictLang(path, text, variant(d))
		if err == nil {
			err = checkHeredocDelimiters(f)
		}
		return f, variant(d), err
	}
	fp, ep := parseStrictLang(path, text, syntax.LangPOSIX)
	fb, eb := parseStrictLang(path, text, syntax.LangBash)
	switch {
	case ep != nil && eb != nil:
		return nil, syntax.LangBash, eb
	case ep != nil:
		return nil, syntax.LangBash, unsupported("grammar-dependent: only the bash grammar reads this sh file (%v)", ep)
	case eb == nil:
		pp, err1 := printForCompare(fp, o)
		pb, err2 := printForCompare(fb, o)
		if err1 != nil || err2 != nil || !bytes.Equal(pp, pb) {
			return nil, syntax.LangPOSIX, unsupported("grammar-dependent: the POSIX and the bash grammar read this sh file differently (a [[ ]] test or another bash form)")
		}
		if o.Simplify { // printForCompare simplified fp
			fp, _ = parseStrictLang(path, text, syntax.LangPOSIX)
		}
	}
	if err := checkHeredocDelimiters(fp); err != nil {
		return nil, syntax.LangPOSIX, err
	}
	return fp, syntax.LangPOSIX, nil
}

// printForCompare prints f as Format would the first time, simplified if o says
// so. It changes f.
func printForCompare(f *syntax.File, o FormatOptions) ([]byte, error) {
	if o.Simplify {
		syntax.Simplify(f)
	}
	return printTree(f, o)
}

// checkHeredocDelimiters refuses a here-document whose delimiter has quoted and
// unquoted parts (`<<"E"F`): bash takes the body as literal if any part is
// quoted, and a parser that read it otherwise would print the body differently.
func checkHeredocDelimiters(f *syntax.File) error {
	var bad error
	visitAll(f, func(n syntax.Node) {
		r, ok := n.(*syntax.Redirect)
		if !ok || bad != nil || (r.Op != syntax.Hdoc && r.Op != syntax.DashHdoc) || r.Word == nil || len(r.Word.Parts) < 2 {
			return
		}
		quoted, plain := false, false
		for _, p := range r.Word.Parts {
			switch p := p.(type) {
			case *syntax.SglQuoted, *syntax.DblQuoted:
				quoted = true
			case *syntax.Lit:
				if strings.Contains(p.Value, "\\") {
					quoted = true
				} else {
					plain = true
				}
			default:
				plain = true
			}
		}
		if quoted && plain {
			bad = unsupported("%d:%d: here-document delimiter is quoted in one part and not in another", r.OpPos.Line(), r.OpPos.Col())
		}
	})
	return bad
}

func parseStrictLang(path string, text []byte, lang syntax.LangVariant) (f *syntax.File, err error) {
	defer func() {
		if r := recover(); r != nil {
			f, err = nil, syntaxErr(1, 1, fmt.Sprintf("parser failure: %v", r))
		}
	}()
	f, err = syntax.NewParser(syntax.KeepComments(true), syntax.Variant(lang)).Parse(bytes.NewReader(text), path)
	if err != nil {
		var pe syntax.ParseError
		var le syntax.LangError
		switch {
		case errors.As(err, &pe):
			return nil, syntaxErr(int(pe.Pos.Line()), int(pe.Pos.Col()), pe.Text)
		case errors.As(err, &le):
			return nil, syntaxErr(int(le.Pos.Line()), int(le.Pos.Col()), le.Error())
		}
		return nil, syntaxErr(1, 1, err.Error())
	}
	return f, nil
}

// printTree prints f; a printer panic is a refusal.
func printTree(f *syntax.File, o FormatOptions) (out []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, unsupported("printer failure: %v", r)
		}
	}()
	var buf bytes.Buffer
	if err := o.printer().Print(&buf, f); err != nil {
		return nil, unsupported("printer failure: %v", err)
	}
	return buf.Bytes(), nil
}

// Format returns src printed with o: one text for the whole file. It returns
// an error wrapping ErrFormatSyntax or ErrFormatUnsupported, and no text,
// whenever it cannot be sure to keep the script's meaning; see prepare.
//
// Every printed text is parsed again with the same grammar, and its tree must
// equal the tree that was printed (positions and whitespace aside; treeSig),
// or the file is not formatted. The printer is not a fixed point for every
// input (a line continuation before a redirection is printed one pass and
// dropped the next), so the text is printed again from that parse, and
// returned once a pass gives the text it started from; a text that has not
// settled after three passes is not formatted. Hence Format(Format(x)) is
// Format(x). A file that holds no statement and no comment is returned as it
// is, and a missing final newline stays missing.
func Format(path string, src []byte, o FormatOptions) ([]byte, error) {
	return FormatWith(path, src, o, Bash)
}

// FormatWith is Format with fallback (DetectWith) as the dialect of a file that
// names none itself; 0 means Bash.
func FormatWith(path string, src []byte, o FormatOptions, fallback Dialect) ([]byte, error) {
	p, err := prepareWith(path, src, fallback)
	if err != nil {
		return nil, err
	}
	f, lang, err := strictParse(path, p.text, p.d, o)
	if err != nil {
		return nil, err
	}
	if len(f.Stmts) == 0 && len(f.Last) == 0 {
		return bytes.Clone(src), nil
	}
	out, err := settle(path, f, lang, o)
	if err != nil {
		return nil, err
	}
	if p.d == POSIX {
		// The next pass reads the printed text again, and an sh file must be
		// read the same way by both grammars then too.
		if _, _, err := strictParse(path, out, p.d, o); err != nil {
			return nil, unsupported("the formatted sh text would be read differently by the two grammars; not applied")
		}
	}
	if DetectWith(path, out, fallback) != p.d {
		// A comment printed without its leading space can become a shebang or
		// a directive; the next read would use another grammar.
		return nil, unsupported("the formatted text would be read as another dialect; not applied")
	}
	if !bytes.HasSuffix(p.text, []byte("\n")) {
		// Printed text always ends in a newline. Without it the tree must
		// still be the one printed.
		trimmed := bytes.TrimSuffix(out, []byte("\n"))
		g, err := parseStrictLang(path, out, lang)
		h, err2 := parseStrictLang(path, trimmed, lang)
		if err != nil || err2 != nil || treeSig(g) != treeSig(h) || bytes.HasSuffix(trimmed, []byte("\n")) {
			return nil, unsupported("the text without its final newline would not parse to the same script; not applied")
		}
		out = trimmed
	}
	return p.restore(out), nil
}

// maxPasses is how many times settle prints before it gives up.
const maxPasses = 3

// settle prints f, parses the output and checks its tree, and prints again,
// until a pass prints the text it parsed. It returns that text.
func settle(path string, f *syntax.File, lang syntax.LangVariant, o FormatOptions) ([]byte, error) {
	var prev []byte
	var want uint64
	known := false // want is the signature of f
	for pass := 0; pass < maxPasses; pass++ {
		if o.Simplify && syntax.Simplify(f) {
			known = false
		}
		if !known {
			want = treeSig(f)
		}
		out, err := printTree(f, o)
		if err != nil {
			return nil, err
		}
		if prev != nil && bytes.Equal(out, prev) {
			return prev, nil
		}
		g, err := parseStrictLang(path, out, lang)
		if err != nil {
			return nil, unsupported("the formatted text would not parse to the same script; not applied")
		}
		got := treeSig(g)
		if got != want {
			return nil, unsupported("the formatted text would not parse to the same script; not applied")
		}
		prev, f, want, known = out, g, got, true
	}
	return nil, unsupported("the formatted text does not settle after %d passes; not applied", maxPasses)
}

// sigSeed makes treeSig's hashes comparable within one process.
var sigSeed = maphash.MakeSeed()

// treeSig is a hash of f's syntax tree that ignores positions, a few spellings
// that mean the same (below) and nothing else: the type of every node, every
// string, number and flag field of it (words, operators, names, comment text,
// heredoc bodies) and where each child hangs, in a depth-first order with
// brackets. It keeps its own stack: a recursive walk took over 32 MiB of Go
// stack on the 10,000-level chains the parser accepts.
//
// What it ignores, because the printer legitimately changes it: trailing white
// space of a comment, whether a command substitution is written with backquotes,
// and the tabs at the start of the lines of a `<<-` body (dashHdoc).
func treeSig(f *syntax.File) uint64 {
	var h maphash.Hash
	h.SetSeed(sigSeed)
	var buf []byte
	stack := []sigItem{{v: reflect.ValueOf(f)}}
	for len(stack) > 0 {
		it := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if it.lit != 0 {
			h.WriteByte(it.lit)
			continue
		}
		v := it.v
		switch v.Kind() {
		case reflect.Interface, reflect.Pointer:
			if v.IsNil() {
				h.WriteByte('0')
			} else {
				stack = append(stack, sigItem{v: v.Elem()})
			}
		case reflect.Struct:
			t := v.Type()
			if t == commentType {
				text := strings.TrimRight(v.Field(commentText).String(), " \t\r\n\v\f")
				buf = append(append(strconv.AppendInt(append(buf[:0], '#'), int64(len(text)), 10), ':'), text...)
				h.Write(buf)
				continue
			}
			pl := planOf(t)
			if pl == nil {
				continue // a position
			}
			h.WriteByte('(')
			h.WriteString(pl.name)
			stack = append(stack, sigItem{lit: ')'})
			dash := pl.hdoc >= 0 && v.Field(pl.op).Uint() == uint64(syntax.DashHdoc)
			for k := len(pl.fields) - 1; k >= 0; k-- {
				if fi := pl.fields[k]; dash && fi == pl.hdoc {
					stack = append(stack, sigItem{v: dashHdoc(v.Field(fi))})
				} else {
					stack = append(stack, sigItem{v: v.Field(fi)})
				}
			}
		case reflect.Slice:
			buf = strconv.AppendInt(append(buf[:0], '['), int64(v.Len()), 10)
			h.Write(buf)
			stack = append(stack, sigItem{lit: ']'})
			for k := v.Len() - 1; k >= 0; k-- {
				stack = append(stack, sigItem{v: v.Index(k)})
			}
		case reflect.String:
			str := v.String()
			buf = append(strconv.AppendInt(append(buf[:0], 's'), int64(len(str)), 10), ':')
			h.Write(buf)
			h.WriteString(str)
		case reflect.Bool:
			if v.Bool() {
				h.WriteByte('T')
			} else {
				h.WriteByte('F')
			}
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			h.Write(strconv.AppendInt(append(buf[:0], 'i'), v.Int(), 10))
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			h.Write(strconv.AppendUint(append(buf[:0], 'u'), v.Uint(), 10))
		}
	}
	return h.Sum64()
}

// sigItem is a value still to hash, or (lit != 0) a closing bracket to write
// once the values pushed after it are done.
type sigItem struct {
	v   reflect.Value
	lit byte
}

// sigPlan is what treeSig hashes of one struct type: its name and the indices
// of its fields, without positions, unexported fields and the backquote flag.
// hdoc and op are the indices of a Redirect's body and operator, else -1.
type sigPlan struct {
	name     string
	fields   []int
	hdoc, op int
}

var (
	posType      = reflect.TypeOf(syntax.Pos{})
	commentType  = reflect.TypeOf(syntax.Comment{})
	redirectType = reflect.TypeOf(syntax.Redirect{})
	cmdSubstType = reflect.TypeOf(syntax.CmdSubst{})
	commentText  = func() int { f, _ := commentType.FieldByName("Text"); return f.Index[0] }()
	sigPlans     sync.Map // reflect.Type -> *sigPlan (nil plan for a position)
)

func planOf(t reflect.Type) *sigPlan {
	if p, ok := sigPlans.Load(t); ok {
		return p.(*sigPlan)
	}
	var pl *sigPlan
	if t != posType {
		pl = &sigPlan{name: t.Name(), hdoc: -1, op: -1}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			switch {
			case !f.IsExported(), t == cmdSubstType && f.Name == "Backquotes":
				continue
			case t == redirectType && f.Name == "Hdoc":
				pl.hdoc = i
			case t == redirectType && f.Name == "Op":
				pl.op = i
			}
			pl.fields = append(pl.fields, i)
		}
	}
	sigPlans.Store(t, pl)
	return pl
}

// dashHdoc returns the body v of a `<<-` here-document without the tabs at the
// start of its lines. The shell removes the leading tabs of every body line, and of the line with the closing
// delimiter, and the printer re-indents the body with the statement: the tabs
// at the start of a line are not part of the body. (The parser keeps the tabs
// before the delimiter in the body's last literal.) A literal that follows an
// expansion on the same line starts mid-line and keeps its tabs.
func dashHdoc(v reflect.Value) reflect.Value {
	w, _ := v.Interface().(*syntax.Word)
	if w == nil {
		return v
	}
	cp := *w
	cp.Parts = make([]syntax.WordPart, len(w.Parts))
	atStart := true
	for i, part := range w.Parts {
		l, ok := part.(*syntax.Lit)
		if !ok {
			cp.Parts[i], atStart = part, false
			continue
		}
		c := *l
		c.Value = stripLineTabs(l.Value, atStart)
		cp.Parts[i], atStart = &c, strings.HasSuffix(l.Value, "\n")
	}
	return reflect.ValueOf(&cp)
}

// stripLineTabs removes the tabs at the start of every line of s; the first
// line only if it starts a line (atStart).
func stripLineTabs(s string, atStart bool) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		if i > 0 || atStart {
			lines[i] = strings.TrimLeft(lines[i], "\t")
		}
	}
	return strings.Join(lines, "\n")
}

// FormatRange formats the top-level statements that the byte range
// [start, end) touches, with the comments that lead them and the comment that
// trails the last one on its line, and returns the one replacement of that
// text: the bytes [editStart, editEnd) of src become text. A range that
// touches no statement (blank lines, a comment on its own) gives an empty
// edit (editStart == editEnd, no text). The text is formatted alone, as a
// file, then put in place; the result is parsed again and its tree must equal
// the original's with the touched statements formatted, or nothing is
// returned. A file with any error is not formatted at all, in part or whole.
// An empty range selects the statement at that offset.
func FormatRange(path string, src []byte, start, end int, o FormatOptions) (editStart, editEnd int, text []byte, err error) {
	return FormatRangeWith(path, src, start, end, o, Bash)
}

// FormatRangeWith is FormatRange with fallback (DetectWith) as the dialect of a
// file that names none itself; 0 means Bash.
func FormatRangeWith(path string, src []byte, start, end int, o FormatOptions, fallback Dialect) (editStart, editEnd int, text []byte, err error) {
	if start < 0 || end < start || end > len(src) {
		return 0, 0, nil, unsupported("range %d-%d is outside the %d byte file", start, end, len(src))
	}
	p, err := prepareWith(path, src, fallback)
	if err != nil {
		return 0, 0, nil, err
	}
	// Offsets in the normalised text: each CRLF before the offset is one byte
	// shorter.
	toText, fromText := func(off int) int { return off }, func(off int) int { return off }
	if p.crlf {
		toText = func(off int) int { return off - bytes.Count(src[:off], []byte("\r")) }
		fromText = func(off int) int { return off + bytes.Count(p.text[:off], []byte("\n")) }
	}
	ts, te := toText(start), toText(end)
	f, lang, err := strictParse(path, p.text, p.d, o)
	if err != nil {
		return 0, 0, nil, err
	}
	// The extent of every top-level statement, its here-document bodies
	// included (they come after the line that starts them, not after the
	// statement), then the statements the range touches and, until nothing
	// changes, the ones that start inside the text so far.
	ext := make([][2]int, len(f.Stmts))
	ends := heredocEnds(f, p.text)
	for k, s := range f.Stmts {
		ext[k][0], ext[k][1] = stmtExtent(s, p.text)
		visitAll(s, func(n syntax.Node) {
			if r, ok := n.(*syntax.Redirect); ok {
				if e, ok := ends[r]; ok && e > ext[k][1] {
					ext[k][1] = e
				}
			}
		})
	}
	from, to := -1, -1
	var picked []*syntax.Stmt
	in := make([]bool, len(f.Stmts))
	for k := range f.Stmts {
		a, b := ext[k][0], ext[k][1]
		if a < te && b > ts || ts == te && a <= ts && ts <= b {
			in[k] = true
			if from < 0 {
				from = a
			}
			to = max(to, b)
		}
	}
	if from < 0 {
		return start, start, nil, nil
	}
	for grew := true; grew; {
		grew = false
		for k := range f.Stmts {
			if !in[k] && ext[k][0] >= from && ext[k][0] < to {
				in[k], grew = true, true
				to = max(to, ext[k][1])
			}
		}
	}
	for k, s := range f.Stmts {
		if in[k] {
			picked = append(picked, s)
		}
	}
	slice := p.text[from:to]
	g, err := parseStrictLang(path, slice, lang)
	if err != nil {
		return 0, 0, nil, unsupported("the statements in the range cannot be formatted on their own: %v", err)
	}
	if o.Simplify {
		syntax.Simplify(g)
		for _, s := range picked {
			syntax.Simplify(s)
		}
	}
	out, err := printTree(g, o)
	if err != nil {
		return 0, 0, nil, err
	}
	out = bytes.TrimSuffix(out, []byte("\n"))
	want := treeSig(f)
	joined := append(append(append([]byte(nil), p.text[:from]...), out...), p.text[to:]...)
	h, err := parseStrictLang(path, joined, lang)
	if err != nil || treeSig(h) != want {
		return 0, 0, nil, unsupported("the formatted range would not parse to the same script; not applied")
	}
	return fromText(from), fromText(to), p.restore(out), nil
}

// stmtExtent is the byte range of a top-level statement with its leading
// comments and its trailing comment: from the first comment or the statement's
// start, to the end of the trailing comment on the statement's last line or
// the statement's end. A `;` ends the statement but is not part of the extent
// (the printer drops it), and `&` is.
func stmtExtent(s *syntax.Stmt, text []byte) (from, to int) {
	from = int(s.Pos().Offset())
	to = rangeStmtEnd(s)
	for _, c := range s.Comments {
		if a := int(c.Pos().Offset()); a < from {
			from = a
		}
		if b := int(c.End().Offset()); b > to {
			to = b
		}
	}
	return from, to
}

func rangeStmtEnd(s *syntax.Stmt) int {
	if s.Semicolon.IsValid() && (s.Background || s.Coprocess || s.Disown) {
		return int(s.End().Offset())
	}
	end := int(s.Pos().Offset())
	if s.Negated {
		end++
	}
	if s.Cmd != nil {
		end = max(end, int(s.Cmd.End().Offset()))
	}
	if n := len(s.Redirs); n > 0 {
		end = max(end, int(s.Redirs[n-1].End().Offset()))
	}
	return end
}

// heredocEnds maps each here-document redirection of f to the offset of the
// end of its closing delimiter line (no newline). The body of a redirection
// follows the line the redirection is on, in the order of the redirections on
// that line; the parser has no body for an empty one, so its delimiter is the
// line after the previous body.
func heredocEnds(f *syntax.File, text []byte) map[*syntax.Redirect]int {
	var rs []*syntax.Redirect
	visitAll(f, func(n syntax.Node) {
		if r, ok := n.(*syntax.Redirect); ok && (r.Op == syntax.Hdoc || r.Op == syntax.DashHdoc) {
			rs = append(rs, r)
		}
	})
	sort.Slice(rs, func(a, b int) bool { return rs[a].OpPos.Offset() < rs[b].OpPos.Offset() })
	lineEnd := func(from int) int {
		if k := bytes.IndexByte(text[from:], '\n'); k >= 0 {
			return from + k
		}
		return len(text)
	}
	ends := make(map[*syntax.Redirect]int, len(rs))
	line, cursor := uint(0), 0
	for _, r := range rs {
		if r.OpPos.Line() != line {
			line, cursor = r.OpPos.Line(), lineEnd(int(r.OpPos.Offset()))
		}
		if r.Hdoc != nil {
			cursor = int(r.Hdoc.End().Offset())
		} else {
			cursor = lineEnd(min(cursor+1, len(text)))
		}
		ends[r] = cursor
	}
	return ends
}

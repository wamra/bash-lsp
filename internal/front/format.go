package front

import (
	"bytes"
	"errors"
	"fmt"
	"hash/maphash"
	"reflect"
	"strconv"
	"strings"

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
func prepare(path string, src []byte) (prepared, error) {
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
	p.d = Detect(path, src)
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
func strictParse(path string, text []byte, d Dialect) (f *syntax.File, lang syntax.LangVariant, err error) {
	langs := []syntax.LangVariant{variant(d)}
	if d == POSIX {
		langs = append(langs, syntax.LangBash)
	}
	for _, l := range langs {
		if f, err = parseStrictLang(path, text, l); err == nil {
			return f, l, nil
		}
		lang = l
	}
	return nil, lang, err
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
	p, err := prepare(path, src)
	if err != nil {
		return nil, err
	}
	f, lang, err := strictParse(path, p.text, p.d)
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
	if !bytes.HasSuffix(p.text, []byte("\n")) {
		// Printed text always ends in a newline. Without it the tree must
		// still be the one printed.
		trimmed := bytes.TrimSuffix(out, []byte("\n"))
		g, err := parseStrictLang(path, out, lang)
		h, err2 := parseStrictLang(path, trimmed, lang)
		if err != nil || err2 != nil || treeSig(g) != treeSig(h) {
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
	for pass := 0; pass < maxPasses; pass++ {
		if o.Simplify {
			syntax.Simplify(f)
		}
		want := treeSig(f)
		out, err := printTree(f, o)
		if err != nil {
			return nil, err
		}
		if prev != nil && bytes.Equal(out, prev) {
			return prev, nil
		}
		g, err := parseStrictLang(path, out, lang)
		if err != nil || treeSig(g) != want {
			return nil, unsupported("the formatted text would not parse to the same script; not applied")
		}
		prev, f = out, g
	}
	return nil, unsupported("the formatted text does not settle after %d passes; not applied", maxPasses)
}

// sigSeed makes treeSig's hashes comparable within one process.
var sigSeed = maphash.MakeSeed()

// treeSig is a hash of f's syntax tree that ignores positions, and comment
// trailing white space (the printer trims it), and nothing else: the type of
// every node, every string, number and flag field of it (words, operators,
// names, comment text, heredoc bodies) and where each child hangs, in a
// depth-first order with brackets. It recurses once per level, which the
// parser bounds at 10,000.
func treeSig(f *syntax.File) uint64 {
	var h maphash.Hash
	h.SetSeed(sigSeed)
	sigValue(&h, reflect.ValueOf(f))
	return h.Sum64()
}

var (
	posType      = reflect.TypeOf(syntax.Pos{})
	commentType  = reflect.TypeOf(syntax.Comment{})
	redirectType = reflect.TypeOf(syntax.Redirect{})
	cmdSubstType = reflect.TypeOf(syntax.CmdSubst{})
)

// sigDashHdoc is sigValue of the body of a `<<-` here-document. The tabs in
// front of the closing delimiter are kept in the body's last literal by the
// parser, but the shell removes them, and the printer puts the delimiter at
// the start of its line: they are not part of the body.
func sigDashHdoc(h *maphash.Hash, v reflect.Value) {
	w, _ := v.Interface().(*syntax.Word)
	if w == nil || len(w.Parts) == 0 {
		sigValue(h, v)
		return
	}
	last, ok := w.Parts[len(w.Parts)-1].(*syntax.Lit)
	if !ok {
		sigValue(h, v)
		return
	}
	trimmed := *last
	if t := strings.TrimRight(trimmed.Value, "\t"); t == "" || strings.HasSuffix(t, "\n") {
		trimmed.Value = t
	}
	cp := *w
	cp.Parts = append(append([]syntax.WordPart(nil), w.Parts[:len(w.Parts)-1]...), &trimmed)
	sigValue(h, reflect.ValueOf(&cp))
}

func sigValue(h *maphash.Hash, v reflect.Value) {
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			h.WriteByte('0')
			return
		}
		sigValue(h, v.Elem())
	case reflect.Struct:
		t := v.Type()
		switch t {
		case posType:
			return
		case commentType:
			h.WriteString("#" + strconv.Quote(strings.TrimRight(v.FieldByName("Text").String(), " \t\r\n\v\f")))
			return
		}
		h.WriteString("(" + t.Name())
		for i := 0; i < t.NumField(); i++ {
			switch {
			case !t.Field(i).IsExported(), t == cmdSubstType && t.Field(i).Name == "Backquotes":
			case t == redirectType && t.Field(i).Name == "Hdoc" && v.FieldByName("Op").Uint() == uint64(syntax.DashHdoc):
				sigDashHdoc(h, v.Field(i))
			default:
				sigValue(h, v.Field(i))
			}
		}
		h.WriteByte(')')
	case reflect.Slice:
		h.WriteString("[" + strconv.Itoa(v.Len()))
		for i := 0; i < v.Len(); i++ {
			sigValue(h, v.Index(i))
		}
		h.WriteByte(']')
	case reflect.String:
		h.WriteString(strconv.Quote(v.String()))
	case reflect.Bool:
		if v.Bool() {
			h.WriteByte('T')
		} else {
			h.WriteByte('F')
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		h.WriteString("i" + strconv.FormatInt(v.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		h.WriteString("u" + strconv.FormatUint(v.Uint(), 10))
	}
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
	if start < 0 || end < start || end > len(src) {
		return 0, 0, nil, unsupported("range %d-%d is outside the %d byte file", start, end, len(src))
	}
	p, err := prepare(path, src)
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
	f, lang, err := strictParse(path, p.text, p.d)
	if err != nil {
		return 0, 0, nil, err
	}
	from, to, picked := -1, -1, []*syntax.Stmt(nil)
	for _, s := range f.Stmts {
		a, b := stmtExtent(s, p.text)
		if !(a < te && b > ts || ts == te && a <= ts && ts <= b) {
			continue
		}
		if from < 0 {
			from = a
		}
		to = b
		picked = append(picked, s)
	}
	if from < 0 {
		return start, start, nil, nil
	}
	slice := p.text[from:to]
	g, err := parseStrictLang(path, slice, lang)
	if err != nil {
		return 0, 0, nil, err
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

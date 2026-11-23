package front

import "bytes"

// The parser (mvdan.cc/sh) is recursive descent with no depth limit, and a
// Go stack overflow is fatal: no recover() catches it. Parse therefore
// measures how deeply a file nests before handing it to the parser and
// refuses one that nests deeper than maxNesting.
//
// Two measures are taken, and either one over its limit refuses the file:
//
//   - a small lexer that follows bash quoting (single and double quotes,
//     backslashes, comments, heredoc bodies, case patterns) and keeps a stack
//     of the constructs that are open: ( { $(( $[ " ` case, plus runs of
//     ternary `?` and of `!` words, the two operator chains the parser
//     recurses on. Its deepest stack must stay within maxNesting.
//   - a raw count of ( { [ against ) } ] over every byte, quotes and comments
//     included, that never goes below zero. It must stay within
//     maxRawNesting. It is a backstop for input on which the lexer and the
//     parser disagree about where a quote, comment or heredoc starts.
//
// Measured on the parser at head 4f8e524, the cheapest overflow costs about
// 5 KB of stack per level and needs some 200,000 levels (`(` repeated). A file
// within both limits makes the parser recurse a few tens of thousands of
// levels at most, a few hundred megabytes of stack in the worst case and far
// less for real scripts, which nest a few dozen levels.
//
// Keyword nesting (if, while, for, case, functions) is not counted: the
// parser recursed through 2 MiB of it without trouble, and the lowerer's own
// depth limit (maxDepth in lower.go) bounds the tree that comes out.
const (
	maxNesting    = 1000
	maxRawNesting = 20000
)

// nestingTooDeep reports whether src nests deeper than the limits above, and
// the offset where the first limit was crossed.
func nestingTooDeep(src []byte) (bool, int) {
	if at := rawNesting(src, maxRawNesting); at >= 0 {
		return true, at
	}
	sc := &nestScanner{src: src, limit: maxNesting, over: -1, wordStart: true, cmdStart: true}
	sc.run(len(src))
	return sc.over >= 0, sc.over
}

// rawNesting returns the offset where the running count of ( { [ minus ) } ]
// (never below zero) first exceeds limit, or -1.
func rawNesting(src []byte, limit int) int {
	depth := 0
	for i, c := range src {
		switch c {
		case '(', '{', '[':
			depth++
			if depth > limit {
				return i
			}
		case ')', '}', ']':
			if depth > 0 {
				depth--
			}
		}
	}
	return -1
}

// Stack entries of nestScanner.
const (
	nParen    = '(' // ( $( <( >(
	nBrace    = '{' // { ${
	nBracket  = '[' // [
	nArith    = 'A' // (( $((, closed by ))
	nArithBrk = 'B' // $[, closed by ]
	nDquote   = '"' // "..." and $"..."
	nBackq    = '`' // `...`
	nCase     = 'c' // case ... esac: a `)` here ends a pattern, it closes nothing
	nHeredoc  = 'H' // unquoted heredoc body: only $ and ` expand
	nTernary  = '?' // soft: one `?` of an unfinished a?b:c chain
	nNot      = '!' // soft: one `!` word of a run (`! ! ! cmd`, `[[ ! ! x ]]`)
)

type pendingDoc struct {
	delim     []byte
	quoted    bool // <<'EOF', <<"EOF", <<\EOF: the body is literal
	stripTabs bool // <<-
}

type nestScanner struct {
	src   []byte
	i     int
	stack []byte
	arith int // nArith and nArithBrk entries on the stack
	limit int
	over  int // offset where the stack first grew past limit, -1 if never

	docs      []pendingDoc // heredocs whose bodies start at the next newline
	inBody    bool         // scanning a heredoc body: heredocs inside it are not followed
	wordStart bool         // the next byte starts a word (a '#' there opens a comment)
	cmdStart  bool         // the next word is in command position (case/esac are keywords)
}

func (s *nestScanner) top() byte {
	if len(s.stack) == 0 {
		return 0
	}
	return s.stack[len(s.stack)-1]
}

func (s *nestScanner) push(k byte) {
	s.stack = append(s.stack, k)
	if k == nArith || k == nArithBrk {
		s.arith++
	}
	if len(s.stack) > s.limit && s.over < 0 {
		s.over = s.i
	}
}

func (s *nestScanner) pop() {
	if k := s.top(); k == nArith || k == nArithBrk {
		s.arith--
	}
	s.stack = s.stack[:len(s.stack)-1]
}

// dropSoft pops the soft entries of kind k on top of the stack.
func (s *nestScanner) dropSoft(k byte) {
	for s.top() == k {
		s.pop()
	}
}

func (s *nestScanner) dropAllSoft() {
	for s.top() == nTernary || s.top() == nNot {
		s.pop()
	}
}

// run scans src[s.i:end], stopping early once the limit is crossed.
func (s *nestScanner) run(end int) {
	for s.i < end && s.over < 0 {
		switch s.top() {
		case nDquote:
			s.dquote()
		case nHeredoc:
			s.docBody()
		default:
			s.code(end)
		}
	}
}

// dquote handles one byte inside double quotes.
func (s *nestScanner) dquote() {
	switch s.src[s.i] {
	case '\\':
		s.i += 2
	case '"':
		s.pop()
		s.i++
		s.wordStart, s.cmdStart = false, false
	case '$':
		s.dollar()
	case '`':
		s.push(nBackq)
		s.i++
		s.wordStart, s.cmdStart = true, true
	default:
		s.i++
	}
}

// docBody handles one byte of an unquoted heredoc body: quotes are literal,
// expansions nest.
func (s *nestScanner) docBody() {
	switch s.src[s.i] {
	case '\\':
		s.i += 2
	case '$':
		s.dollar()
	case '`':
		s.push(nBackq)
		s.i++
		s.wordStart, s.cmdStart = true, true
	default:
		s.i++
	}
}

func isBlank(c byte) bool { return c == ' ' || c == '\t' }

// code handles one token's worth of bytes in command or arithmetic context.
func (s *nestScanner) code(end int) {
	src, c := s.src, s.src[s.i]
	if !isBlank(c) && c != '\n' && !(c == '!' && s.bangWord()) {
		s.dropSoft(nNot) // a run of `!` words ends at the next other token
	}
	switch c {
	case ' ', '\t', '\n':
		if s.arith == 0 {
			s.dropSoft(nTernary) // outside arithmetic, `?` is a glob character in a word
		}
		s.i++
		s.wordStart = true
		if c == '\n' {
			s.cmdStart = true
			if len(s.docs) > 0 && !s.inBody {
				s.heredocs(end)
			}
		}
		return
	case ';', '&', '|':
		if s.arith == 0 {
			s.dropSoft(nTernary)
		}
		s.i++
		s.wordStart, s.cmdStart = true, true
		return
	case '\\':
		s.i += 2
	case '\'':
		if j := bytes.IndexByte(src[s.i+1:], '\''); j >= 0 {
			s.i += j + 2
		} else {
			s.i = len(src)
		}
	case '"':
		s.push(nDquote)
		s.i++
	case '`':
		if s.top() == nBackq {
			s.pop()
			s.i++
		} else {
			s.push(nBackq)
			s.i++
			s.wordStart, s.cmdStart = true, true
			return
		}
	case '#':
		if s.wordStart && s.arith == 0 {
			if j := bytes.IndexByte(src[s.i:], '\n'); j >= 0 {
				s.i += j // the newline itself is scanned next
			} else {
				s.i = len(src)
			}
			return
		}
		s.i++
	case '$':
		s.dollar()
		return
	case '(':
		if s.cmdStart && s.i+1 < len(src) && src[s.i+1] == '(' {
			s.push(nArith) // (( arithmetic command, or two subshells: either way two levels at most
			s.i += 2
		} else {
			s.push(nParen)
			s.i++
		}
		s.wordStart, s.cmdStart = true, true
		return
	case ')':
		s.closeParen()
		return
	case '{':
		s.push(nBrace)
		s.i++
		s.wordStart, s.cmdStart = true, true
		return
	case '}':
		s.dropAllSoft()
		if s.top() == nBrace {
			s.pop()
		}
		s.i++
	case '[':
		// A glob bracket expression ([{(], [[:digit:]]) is literal text; a test
		// `[`, `[[` or a subscript opens nothing the parser recurses on, except
		// a subscript inside a subscript, which the raw count bounds.
		s.i = globEnd(src, s.i)
	case ']':
		s.dropAllSoft()
		if s.top() == nArithBrk {
			s.pop()
		}
		s.i++
	case '?':
		s.push(nTernary)
		s.i++
	case ':':
		if s.top() == nTernary {
			s.pop()
		}
		s.i++
	case '!':
		if s.bangWord() {
			s.push(nNot)
		}
		s.i++
		s.wordStart = false // the next blank sets it again; cmdStart stays: `! cmd`
		return
	case '<':
		switch {
		case bytes.HasPrefix(src[s.i:], []byte("<<<")): // here-string
			s.i += 3
		case bytes.HasPrefix(src[s.i:], []byte("<<")) && s.arith == 0: // in arithmetic, << is a shift
			s.heredocOp()
			return
		default:
			s.i++
		}
		s.wordStart = true
		return
	case '>':
		s.i++
		s.wordStart = true
		return
	default:
		if s.cmdStart && isWordByte(c) {
			s.keyword()
			return
		}
		s.i++
	}
	s.wordStart, s.cmdStart = false, false
}

// bangWord reports whether the '!' at s.i is a word of its own.
func (s *nestScanner) bangWord() bool {
	j := s.i + 1
	return s.wordStart && (j >= len(s.src) || isBlank(s.src[j]) || s.src[j] == '\n')
}

// keyword consumes a word in command position, noting case and esac, and
// whether the word after it is in command position too.
func (s *nestScanner) keyword() {
	j := s.i
	for j < len(s.src) && isWordByte(s.src[j]) {
		j++
	}
	if j < len(s.src) && !isBlank(s.src[j]) && s.src[j] != '\n' && s.src[j] != ';' && s.src[j] != ')' {
		// Part of a longer word (`case.sh`, `a=b`): not a keyword. Scan it as bytes.
		s.i++
		s.wordStart, s.cmdStart = false, false
		return
	}
	w := string(s.src[s.i:j])
	s.i = j
	s.wordStart = false
	switch w {
	case "case":
		s.push(nCase)
		s.cmdStart = false
	case "esac":
		if s.top() == nCase {
			s.pop()
		}
		s.cmdStart = false
	case "if", "then", "else", "elif", "while", "until", "do", "time", "in":
		s.cmdStart = true // `in`: the first pattern of a case may be followed straight by esac
	default:
		s.cmdStart = false
	}
}

// closeParen handles ')'.
func (s *nestScanner) closeParen() {
	s.dropAllSoft()
	s.i++
	s.wordStart, s.cmdStart = true, true
	switch s.top() {
	case nParen:
		s.pop()
	case nArith:
		if s.i < len(s.src) && s.src[s.i] == ')' {
			s.pop()
			s.i++
			s.wordStart, s.cmdStart = false, false
		} else { // `((a) )`: two subshells after all; one of them is still open
			s.pop()
			s.push(nParen)
		}
	}
	// nCase: the end of a pattern. Anything else: a stray ')', which closes nothing.
}

// dollar handles a '$' and what it opens.
func (s *nestScanner) dollar() {
	src, i := s.src, s.i
	next := byte(0)
	if i+1 < len(src) {
		next = src[i+1]
	}
	switch {
	case next == '(' && i+2 < len(src) && src[i+2] == '(':
		s.push(nArith)
		s.i += 3
		s.wordStart, s.cmdStart = false, false
		return
	case next == '(':
		s.push(nParen)
		s.i += 2
		s.wordStart, s.cmdStart = true, true
		return
	case next == '{':
		s.push(nBrace)
		s.i += 2
	case next == '[':
		s.push(nArithBrk)
		s.i += 2
	case next == '"':
		s.push(nDquote)
		s.i += 2
	case next == '\'' && s.top() != nDquote && s.top() != nHeredoc:
		j := i + 2
		for j < len(src) && src[j] != '\'' {
			if src[j] == '\\' {
				j++
			}
			j++
		}
		s.i = min(j+1, len(src))
	default:
		s.i++
	}
	s.wordStart, s.cmdStart = false, false
}

// heredocOp reads `<<[-]WORD` and queues the heredoc; its body starts at the
// next newline.
func (s *nestScanner) heredocOp() {
	src := s.src
	j := s.i + 2
	d := pendingDoc{}
	if j < len(src) && src[j] == '-' {
		d.stripTabs = true
		j++
	}
	for j < len(src) && isBlank(src[j]) {
		j++
	}
	var delim []byte
	for j < len(src) {
		c := src[j]
		if isBlank(c) || c == '\n' || c == ';' || c == '&' || c == '|' || c == '<' || c == '>' || c == '(' || c == ')' {
			break
		}
		switch c {
		case '\'', '"':
			d.quoted = true
			k := bytes.IndexByte(src[j+1:], c)
			if k < 0 {
				k = len(src) - j - 1
			}
			delim = append(delim, src[j+1:j+1+k]...)
			j += k + 2
			continue
		case '\\':
			d.quoted = true
			if j+1 < len(src) {
				delim = append(delim, src[j+1])
			}
			j += 2
			continue
		}
		delim = append(delim, c)
		j++
	}
	s.i = min(j, len(src))
	s.wordStart, s.cmdStart = false, false
	if len(delim) > 0 && !s.inBody {
		d.delim = delim
		s.docs = append(s.docs, d)
	}
}

// heredocs consumes the bodies of the queued heredocs, which start at s.i
// (just past a newline). A quoted body is skipped; an unquoted one is scanned
// for the expansions it may hold.
func (s *nestScanner) heredocs(end int) {
	docs := s.docs
	s.docs = nil
	for _, d := range docs {
		bodyEnd, next := s.findDelim(d)
		if !d.quoted {
			depth := len(s.stack)
			s.inBody = true
			s.push(nHeredoc)
			s.run(min(bodyEnd, end))
			s.inBody = false
			for len(s.stack) > depth {
				s.pop()
			}
		}
		s.i = next
	}
	s.wordStart, s.cmdStart = true, true
}

// findDelim returns where the body that starts at s.i ends and where the line
// after its delimiter starts (len(src) for both when the delimiter is missing).
func (s *nestScanner) findDelim(d pendingDoc) (bodyEnd, next int) {
	src := s.src
	for ls := s.i; ls < len(src); {
		le := len(src)
		nl := le
		if k := bytes.IndexByte(src[ls:], '\n'); k >= 0 {
			le = ls + k
			nl = le + 1
		}
		line := src[ls:le]
		if d.stripTabs {
			line = bytes.TrimLeft(line, "\t")
		}
		if bytes.Equal(line, d.delim) {
			return ls, nl
		}
		ls = nl
	}
	return len(src), len(src)
}

// globEnd returns the offset just past the bracket expression that starts at
// src[i] == '[' when it closes within the word, else i+1.
func globEnd(src []byte, i int) int {
	j := i + 1
	if j < len(src) && (src[j] == '!' || src[j] == '^') {
		j++
	}
	if j < len(src) && src[j] == ']' {
		j++ // a ']' first in the set is literal
	}
	for j < len(src) {
		switch c := src[j]; {
		case c == ']':
			return j + 1
		case c == '[' && j+1 < len(src) && (src[j+1] == ':' || src[j+1] == '.' || src[j+1] == '='):
			// [:class:], [.coll.], [=equiv=]
			end := bytes.Index(src[j+2:], []byte{src[j+1], ']'})
			if end < 0 {
				return i + 1
			}
			j += 2 + end + 2
		case isBlank(c) || c == '\n' || c == ';' || c == '&' || c == '|' || c == '<' || c == '>' || c == '(' || c == ')' || c == '\'' || c == '"' || c == '$' || c == '`' || c == '\\':
			return i + 1
		default:
			j++
		}
	}
	return i + 1
}

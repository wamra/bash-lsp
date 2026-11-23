package front

import "bytes"

// The parser (mvdan.cc/sh) is recursive descent with no depth limit, and a
// Go stack overflow is fatal: no recover() catches it. Parse therefore
// measures how deeply a file nests before handing it to the parser and
// refuses one that nests too deeply.
//
// Two measures are taken, and either one over its limit refuses the file:
//
//   - a small lexer that follows bash quoting (single and double quotes,
//     backslashes, comments, heredoc bodies, case patterns, glob brackets)
//     and keeps a stack of everything the parser recurses on: ( { ${ $(( $[
//     " ` and test or subscript brackets; if, while, until, for, select,
//     case and elif until they close; chains of time, coproc and function
//     headers; and, inside expressions, runs of unary operators and of the
//     operators the parser nests on (?: = ** && ||). Its deepest stack must
//     stay within maxNesting.
//   - a raw count of ( { [ against ) } ] over every byte, quotes and comments
//     included, that never goes below zero. It must stay within
//     maxRawNesting. It is a backstop for input on which the lexer and the
//     parser disagree about where a quote, comment or heredoc starts.
//
// Measured at 4f8e524: `(` repeated overflowed the 1 GB stack limit between
// 125,000 and 150,000 levels (the stack doubles, so about 512 MB is usable),
// and every shape above, at the 2 MiB file limit, either overflowed or used
// more than 128 MB. Nesting just under maxNesting parses within an 8 MB stack
// limit (TestNestingAtTheBoundParses runs it at 64 MB). Real scripts stay far
// below: the deepest of 1,691 on the development machine reached 55.
// Long chains the parser builds without recursing (&&, ||, |, 1+1+...) are not
// counted; the lowerer flattens them, and its own depth limit (maxDepth in
// lower.go) bounds whatever tree comes out.
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

// Stack entries of nestScanner. Hard entries are closed by their closer;
// soft entries count one link of an operator chain and are dropped when the
// chain ends.
const (
	nParen    = '(' // ( $( <( >(
	nBrace    = '{' // { group
	nParam    = 'P' // ${
	nBracket  = '[' // [ that is not a glob: a test or a subscript
	nArith    = 'A' // (( $((, closed by ))
	nArithBrk = 'B' // $[, closed by ]
	nDquote   = '"' // "..." and $"..."
	nBackq    = '`' // `...`
	nCase     = 'c' // case ... esac: a `)` here ends a pattern, it closes nothing
	nKeyword  = 'k' // if, while, until, for, select: closed by fi or done
	nElif     = 'e' // elif: the parser nests each branch; all close at fi
	nHeredoc  = 'H' // unquoted heredoc body: only $ and ` expand
	nTernary  = '?' // soft: one `?` of an unfinished a?b:c chain
	nNot      = '!' // soft: one `!` word of a run (`! ! ! cmd`)
	nUnary    = 'u' // soft: one unary operator of a run in an expression (`- - - 1`, `[[ ! ! x ]]`)
	nPrefix   = 't' // soft: one `time` or `coproc` of a chain (`time time cmd`)
	nFunc     = 'f' // soft: one function header of a chain (`f() g() { :; }`)
	nRight    = 'r' // soft: one binary operator the parser may nest on in an expression (= ** && ||), kept until the expression closes
)

func soft(k byte) bool {
	return k == nTernary || k == nNot || k == nUnary || k == nPrefix || k == nFunc || k == nRight
}

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

	docs        []pendingDoc // heredocs whose bodies start at the next newline
	inBody      bool         // scanning a heredoc body: heredocs inside it are not followed
	wordStart   bool         // the next byte starts a word (a '#' there opens a comment)
	cmdStart    bool         // the next word is in command position (keywords count there)
	afterHeader bool         // the last command token was a function header: the next is its body
}

func (s *nestScanner) top() byte {
	if len(s.stack) == 0 {
		return 0
	}
	return s.stack[len(s.stack)-1]
}

// hardTop is the topmost entry that is not soft, 0 if none.
func (s *nestScanner) hardTop() byte {
	for i := len(s.stack) - 1; i >= 0; i-- {
		if !soft(s.stack[i]) {
			return s.stack[i]
		}
	}
	return 0
}

// inExpr reports whether the scanner is inside an arithmetic expression,
// a parameter expansion or a test/subscript bracket, where operator chains
// may span blanks.
func (s *nestScanner) inExpr() bool {
	switch s.hardTop() {
	case nArith, nArithBrk, nParam, nBracket:
		return true
	}
	return false
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
	for soft(s.top()) {
		s.pop()
	}
}

// endCommand ends the chains that cannot cross a command separator.
func (s *nestScanner) endCommand() {
	for k := s.top(); k == nTernary || k == nPrefix || k == nNot || k == nUnary || k == nRight; k = s.top() {
		if (k == nTernary || k == nUnary || k == nRight) && s.inExpr() {
			return
		}
		s.pop()
	}
}

// command notes the start of a command token in command position: unless it
// is the body of the function header just read, a chain of headers ends.
func (s *nestScanner) command() {
	if s.afterHeader {
		s.afterHeader = false
		return
	}
	for k := s.top(); k == nFunc || k == nPrefix; k = s.top() {
		if k == nPrefix {
			return // `time f() { :; }` is still one chain
		}
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

// isUnaryOp reports whether c can be a unary operator in an expression.
func isUnaryOp(c byte) bool { return c == '-' || c == '+' || c == '!' || c == '~' }

// code handles one token's worth of bytes in command or expression context.
func (s *nestScanner) code(end int) {
	src, c := s.src, s.src[s.i]
	if !isBlank(c) && c != '\n' {
		if !(c == '!' && s.bangWord()) {
			s.dropSoft(nNot) // a run of `!` words ends at the next other token
		}
		if !isUnaryOp(c) {
			s.dropSoft(nUnary) // a run of unary operators ends at its operand
		}
		if s.cmdStart && !isWordByte(c) && c != '#' && c != ';' && c != '&' && c != '|' && c != ')' {
			s.command()
		}
	}
	switch c {
	case ' ', '\t', '\n':
		if !s.inExpr() {
			s.dropSoft(nTernary) // outside expressions, `?` is a glob character in a word
		}
		s.i++
		s.wordStart = true
		if c == '\n' {
			s.endCommand()
			s.cmdStart = true
			if len(s.docs) > 0 && !s.inBody {
				s.heredocs(end)
			}
		}
		return
	case ';', '&', '|':
		if s.inExpr() { // an operator: && and || chains nest in [[ ]], | and & are bitwise
			if c != ';' {
				s.push(nRight)
			}
			s.i++
			return
		}
		s.endCommand()
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
			s.wordStart, s.cmdStart = true, false
			return
		}
		s.push(nParen)
		s.i++
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
		if k := s.top(); k == nBrace || k == nParam {
			s.pop()
		}
		s.i++
	case '[':
		// A glob bracket expression ([{(], [[:digit:]]) is literal text. Any
		// other [ opens a test or a subscript, an expression up to its ].
		if e := globEnd(src, s.i); e > s.i+1 {
			s.i = e
		} else {
			s.push(nBracket)
			s.i++
			s.wordStart = true
			return
		}
	case ']':
		s.dropAllSoft()
		if k := s.top(); k == nBracket || k == nArithBrk {
			s.pop()
		}
		s.i++
	case '?':
		s.push(nTernary)
		s.i++
	case '=', '*':
		// Assignment chains (a=b=c) and ** are right-associative: the parser
		// recurses once per operator. Elsewhere these are word characters.
		if s.inExpr() {
			s.push(nRight)
		}
		s.i++
	case ':':
		if s.top() == nTernary {
			s.pop()
		}
		s.i++
	case '-', '+', '~', '!':
		switch {
		case s.inExpr():
			s.push(nUnary)
		case c == '!' && s.bangWord():
			s.push(nNot)
			s.i++
			s.wordStart = false // the next blank sets it again; cmdStart stays: `! cmd`
			return
		}
		s.i++
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

// isNameByte reports whether c can be part of a function name or keyword.
func isNameByte(c byte) bool {
	switch c {
	case ' ', '\t', '\n', ';', '&', '|', '(', ')', '<', '>', '\'', '"', '`', '$', '\\', '{', '}', '[', ']', '=', '#', '?', '*', '!', '~':
		return false
	}
	return true
}

// skipBlanks returns the first offset at or after j that is not a blank.
func (s *nestScanner) skipBlanks(j int) int {
	for j < len(s.src) && isBlank(s.src[j]) {
		j++
	}
	return j
}

// keyword consumes a word in command position. It counts the compound
// commands the parser nests on (if, while, until, for, select, case, elif),
// the prefix chains (time, coproc) and function headers, and decides whether
// the word after it is in command position too.
func (s *nestScanner) keyword() {
	src := s.src
	j := s.i
	for j < len(src) && isNameByte(src[j]) {
		j++
	}
	w := string(src[s.i:j])
	// NAME () is a function header, whatever NAME is spelled with.
	if k := s.skipBlanks(j); k < len(src) && src[k] == '(' {
		if k2 := s.skipBlanks(k + 1); k2 < len(src) && src[k2] == ')' {
			s.header()
			s.i = k2 + 1
			return
		}
	}
	s.i = j
	s.wordStart = false
	ends := j >= len(src) || isBlank(src[j]) || src[j] == '\n' || src[j] == ';' || src[j] == ')' || src[j] == '&' || src[j] == '|' || src[j] == '<' || src[j] == '>'
	if !ends {
		s.command() // part of a longer word (`a=b`, `x[1]=2`): a plain command
		s.cmdStart = false
		return
	}
	switch w {
	case "if", "while", "until":
		s.command()
		s.push(nKeyword)
		s.cmdStart = true
	case "for", "select":
		s.command()
		s.push(nKeyword)
		s.cmdStart = false
		if k := s.skipBlanks(s.i); bytes.HasPrefix(src[k:], []byte("((")) {
			s.push(nArith) // for (( init; cond; step ))
			s.i = k + 2
		}
	case "case":
		s.command()
		s.push(nCase)
		s.cmdStart = false
	case "fi", "done":
		for k := s.top(); soft(k) || k == nElif && w == "fi"; k = s.top() {
			s.pop()
		}
		if s.top() == nKeyword {
			s.pop()
		}
		s.cmdStart = false
	case "esac":
		s.dropAllSoft()
		if s.top() == nCase {
			s.pop()
		}
		s.cmdStart = false
	case "elif":
		s.dropAllSoft()
		s.push(nElif)
		s.cmdStart = true
	case "then", "else", "do":
		s.cmdStart = true
	case "in":
		s.cmdStart = s.top() == nCase // case patterns follow; a for list does not hold keywords
	case "time", "coproc":
		s.command()
		s.push(nPrefix)
		s.cmdStart = true
	case "function":
		s.funcKeyword()
	default:
		s.command()
		s.cmdStart = false
	}
}

// header counts a function header: one link more when it is the body of the
// header before it (`f() g() h() { :; }`), else the first of a new chain.
func (s *nestScanner) header() {
	if !s.afterHeader {
		s.command()
	}
	s.push(nFunc)
	s.afterHeader = true
	s.wordStart, s.cmdStart = true, true
}

// funcKeyword handles `function NAME [()]`.
func (s *nestScanner) funcKeyword() {
	src := s.src
	j := s.skipBlanks(s.i)
	for j < len(src) && isNameByte(src[j]) {
		j++
	}
	if k := s.skipBlanks(j); k < len(src) && src[k] == '(' {
		if k2 := s.skipBlanks(k + 1); k2 < len(src) && src[k2] == ')' {
			j = k2 + 1
		}
	}
	s.header()
	s.i = j
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
		s.push(nParam)
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

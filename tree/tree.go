// Package tree is the project's own syntax-tree data model. It imports only
// the standard library.
package tree

// Pos is a source position. Offset counts bytes from the file start, Line is
// 1-based, Col is 1-based and counts bytes.
type Pos struct{ Offset, Line, Col int }

// Range is a half-open source range; End is exclusive.
type Range struct{ Start, End Pos }

// Contains reports whether offset lies in [Start, End). A zero-width range
// contains its own offset.
func (r Range) Contains(offset int) bool {
	if r.Start.Offset == r.End.Offset {
		return offset == r.Start.Offset
	}
	return r.Start.Offset <= offset && offset < r.End.Offset
}

// Dialect is the shell language variant a file is parsed as.
type Dialect uint8

const (
	Bash Dialect = iota + 1
	POSIX
	Mksh
	Bats
	Zsh
)

// String returns the dialect's conventional shell name.
func (d Dialect) String() string {
	switch d {
	case Bash:
		return "bash"
	case POSIX:
		return "sh"
	case Mksh:
		return "mksh"
	case Bats:
		return "bats"
	case Zsh:
		return "zsh"
	}
	return "Dialect(?)"
}

// Kind identifies a node type.
type Kind uint8

const (
	KFile Kind = iota + 1
	KStmt
	KCall
	KFuncDecl
	KIf
	KWhile
	KFor
	KCStyleFor
	KCase
	KBlock
	KSubshell
	KPipeline
	// KAndOr is one node for a whole `&&`/`||` chain: its children are the
	// operands (KStmt nodes) in source order, and the operators are not recorded.
	KAndOr
	KDecl
	KTest
	KArith
	KLet
	KCoproc
	KTime
	KErrorGap
	KWord
	KLit
	KSglQuoted
	KDblQuoted
	KParamExp
	KCmdSubst
	KArithExp
	KProcSubst
	KBraceExp
	KExtGlob
	KAssign
	KRedirect
	KHeredoc
	KComment
)

var kindNames = [...]string{
	KFile: "File", KStmt: "Stmt", KCall: "Call", KFuncDecl: "FuncDecl", KIf: "If",
	KWhile: "While", KFor: "For", KCStyleFor: "CStyleFor", KCase: "Case",
	KBlock: "Block", KSubshell: "Subshell", KPipeline: "Pipeline", KAndOr: "AndOr",
	KDecl: "Decl", KTest: "Test", KArith: "Arith", KLet: "Let", KCoproc: "Coproc",
	KTime: "Time", KErrorGap: "ErrorGap", KWord: "Word", KLit: "Lit",
	KSglQuoted: "SglQuoted", KDblQuoted: "DblQuoted", KParamExp: "ParamExp",
	KCmdSubst: "CmdSubst", KArithExp: "ArithExp", KProcSubst: "ProcSubst",
	KBraceExp: "BraceExp", KExtGlob: "ExtGlob", KAssign: "Assign",
	KRedirect: "Redirect", KHeredoc: "Heredoc", KComment: "Comment",
}

// String returns the kind's name without the K prefix.
func (k Kind) String() string {
	if int(k) < len(kindNames) && kindNames[k] != "" {
		return kindNames[k]
	}
	return "Kind(?)"
}

// Node is one syntax-tree node.
type Node struct {
	Kind      Kind
	Range     Range
	Parent    *Node
	Children  []*Node
	Recovered bool   // the parser or resync invented part of this node
	Name      string // FuncDecl: function name. Assign: variable. ParamExp: parameter. Decl: "local"|"declare"|"export"|"readonly"|"typeset". For: loop variable. Heredoc: tag.
	NameRange Range  // range of Name in the source; zero when Name is empty
	Value     string // Lit and SglQuoted: text. Comment: text without the leading '#', trimmed.
}

// Walk visits n and its descendants in pre-order. If fn returns false the
// node's children are skipped.
func (n *Node) Walk(fn func(*Node) bool) {
	if n == nil || !fn(n) {
		return
	}
	for _, c := range n.Children {
		c.Walk(fn)
	}
}

// Enclosing returns the nearest ancestor of kind k (not n itself), or nil.
func (n *Node) Enclosing(k Kind) *Node {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Kind == k {
			return p
		}
	}
	return nil
}

// SyntaxError is a parse problem at a source range. Internal marks a panic
// in the parser or the lowerer that was recovered: the region it names is
// incomplete because of a defect in bash-lsp, not in the script.
type SyntaxError struct {
	Range    Range
	Msg      string
	Internal bool
}

// Limit is a work bound the parser hit. Name is "resync-attempts" or
// "resync-bytes"; Range is the error gap left unanalysed because of it (from
// the gap's start to the end of the file); Detail says what was used up.
type Limit struct {
	Name   string
	Range  Range
	Detail string
}

// File is a parsed (or skipped) source file.
type File struct {
	Path     string
	Src      []byte
	Root     *Node   // Kind == KFile
	Comments []*Node // every KComment, in source order
	Errors   []SyntaxError
	Limits   []Limit   // work bounds hit while parsing, in source order
	Features []Feature // syntactic evidence of dialect and version, in source order; none inside an error gap
	Skipped  string    // non-empty reason when the file was not parsed (e.g. "zsh is not supported yet")
	Dialect  Dialect   // the dialect the file was parsed as; set by every parse, including a skipped one

	lines []int // byte offset of each line start, built once by NewFile
}

// NewFile builds the line index; Root is an empty KFile node spanning Src.
func NewFile(path string, src []byte) *File {
	f := &File{Path: path, Src: src, lines: lineStarts(src)}
	f.Root = &Node{Kind: KFile, Range: Range{Start: f.PosAt(0), End: f.PosAt(len(src))}}
	return f
}

// NodeAt returns the deepest node whose Range contains offset; Root if none.
// It is the reference implementation that Locator.At must match (the tests
// use it as the oracle); production code uses a Locator, which is faster when
// many offsets are looked up in one tree.
// Sibling ranges may overlap (a heredoc body lies inside the first stage of a
// pipeline whose second stage starts earlier), so at each level it descends
// into the containing child with the greatest Range.Start.Offset; on a tie,
// the later child.
func (f *File) NodeAt(offset int) *Node {
	n := f.Root
	for {
		var next *Node
		for _, c := range n.Children {
			if c.Range.Contains(offset) && (next == nil || c.Range.Start.Offset >= next.Range.Start.Offset) {
				next = c
			}
		}
		if next == nil {
			return n
		}
		n = next
	}
}

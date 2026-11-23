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

// SyntaxError is a parse problem at a source range.
type SyntaxError struct {
	Range Range
	Msg   string
}

// File is a parsed (or skipped) source file.
type File struct {
	Path     string
	Src      []byte
	Root     *Node   // Kind == KFile
	Comments []*Node // every KComment, in source order
	Errors   []SyntaxError
	Skipped  string // non-empty reason when the file was not parsed (e.g. "zsh is not supported yet")

	lines []int // byte offset of each line start, built once by NewFile
}

// NewFile builds the line index; Root is an empty KFile node spanning Src.
func NewFile(path string, src []byte) *File {
	f := &File{Path: path, Src: src, lines: lineStarts(src)}
	f.Root = &Node{Kind: KFile, Range: Range{Start: f.PosAt(0), End: f.PosAt(len(src))}}
	return f
}

// NodeAt returns the deepest node whose Range contains offset; Root if none.
func (f *File) NodeAt(offset int) *Node {
	n := f.Root
	for {
		var next *Node
		for _, c := range n.Children {
			if c.Range.Contains(offset) {
				next = c
				break
			}
		}
		if next == nil {
			return n
		}
		n = next
	}
}

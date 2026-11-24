package analysis

import (
	"strconv"
	"strings"

	"github.com/wamra/bash-lsp/tree"
)

// maxGetoptsShown caps the options listed from a getopts string.
const maxGetoptsShown = 64

// argsLabel is the label of the parameter that stands for every argument
// past the named ones.
const argsLabel = "[args...]"

// Param is one positional parameter of a script function: Index is N of $N,
// Name the variable it is copied into, "" when only $N is read.
type Param struct {
	Index int
	Name  string
}

// FuncSignature is what a call to a script function accepts, as far as its
// body shows. Label is `name p1 p2 [args...]`, with `$N` where no name is
// known. Doc is the doc comment. Getopts lists the options of a literal
// `getopts` string in the body (`-a -b ARG`), "" when there is none.
type FuncSignature struct {
	Name, Label, Doc string
	Params           []Param
	Variadic         bool
	Getopts          string
}

// Signature describes the function sym. It is false for a symbol that is not
// a function defined in the workspace.
func (s *Snapshot) Signature(sym *Symbol) (FuncSignature, bool) {
	if sym == nil || sym.Kind != DeclFunction || len(sym.Decls) == 0 {
		return FuncSignature{}, false
	}
	d := sym.Decls[0]
	params, args := s.params(d)
	sig := FuncSignature{Name: d.Name, Params: params, Variadic: args, Getopts: getoptsOptions(d.Node)}
	for _, x := range sym.Decls {
		if x.Doc != "" {
			sig.Doc = x.Doc
			break
		}
	}
	parts := []string{d.Name}
	for _, p := range sig.Params {
		parts = append(parts, p.label())
	}
	if args {
		parts = append(parts, argsLabel)
	}
	sig.Label = strings.Join(parts, " ")
	return sig, true
}

// params are the positional parameters the body of d reads, and whether it
// reads "$@" or "$*". It touches only d's own body.
func (s *Snapshot) params(d *Decl) (params []Param, variadic bool) {
	names, max, args := positionalParams(d.Node, s.src(d))
	for i := 1; i <= max; i++ {
		params = append(params, Param{Index: i, Name: names[i]})
	}
	return params, args
}

func (p Param) label() string {
	if p.Name != "" {
		return p.Name
	}
	return "$" + strconv.Itoa(p.Index)
}

// SignatureAt describes the script function whose call has the cursor after
// its command word: in the argument list, or in the blanks after it on the
// same line. active is the 0-based index of the argument under the cursor,
// at most len(sig.Params) (the `[args...]` position) when the function is
// variadic. The innermost call decides: inside `f $(g ▮)` it is g's. ok is
// false on the command word itself, outside a call, and for a call that is
// not bound to a function.
func (s *Snapshot) SignatureAt(path string, offset int) (sig FuncSignature, active int, ok bool) {
	file, found := s.idx[path]
	if !found {
		return FuncSignature{}, 0, false
	}
	src := s.facts[file].File.Src
	var best *Use
	var bestWords []*tree.Node
	for _, u := range s.facts[file].Uses {
		if u.Kind != UseCall || u.Via != ViaNone || u.ViaAlias || u.Node == nil || u.Node.Kind != tree.KCall {
			continue
		}
		if u.Range.End.Offset >= offset || (best != nil && u.Range.Start.Offset <= best.Range.Start.Offset) {
			continue
		}
		words := callWords(u.Node)
		if len(words) == 0 || words[0].Range.Start.Offset != u.Range.Start.Offset {
			continue
		}
		end := words[len(words)-1].Range.End.Offset
		if offset > end && !blanks(src, end, offset) {
			continue
		}
		best, bestWords = u, words
	}
	if best == nil {
		return FuncSignature{}, 0, false
	}
	var sym *Symbol
	for _, b := range s.bindings[file] {
		if b.Use == best && b.Sym != nil && b.Sym.Kind == DeclFunction {
			sym = b.Sym
			break
		}
	}
	sig, ok = s.Signature(sym)
	if !ok {
		return FuncSignature{}, 0, false
	}
	for _, w := range bestWords[1:] {
		if w.Range.End.Offset < offset {
			active++
		}
	}
	if sig.Variadic {
		active = min(active, len(sig.Params))
	}
	return sig, active, true
}

// callWords are the word children of a call: the command word, then the arguments.
func callWords(n *tree.Node) []*tree.Node {
	var out []*tree.Node
	for _, c := range n.Children {
		if c.Kind == tree.KWord {
			out = append(out, c)
		}
	}
	return out
}

// blanks reports whether src[a:b] holds only spaces and tabs.
func blanks(src []byte, a, b int) bool {
	if a < 0 || b > len(src) || a > b {
		return false
	}
	for _, c := range src[a:b] {
		if c != ' ' && c != '\t' {
			return false
		}
	}
	return true
}

// getoptsOptions renders the first literal `getopts` option string in a
// function body (nested functions skipped) as `-a -b ARG`: a letter followed
// by ':' takes an argument. A leading ':' (silent errors) is not an option.
func getoptsOptions(fn *tree.Node) string {
	var out string
	fn.Walk(func(n *tree.Node) bool {
		if out != "" || (n != fn && n.Kind == tree.KFuncDecl) {
			return false
		}
		if n.Kind != tree.KCall {
			return true
		}
		words := callWords(n)
		if len(words) < 2 {
			return true
		}
		if name, ok := literalText(words[0]); !ok || name != "getopts" {
			return true
		}
		if spec, ok := literalText(words[1]); ok {
			out = optionList(spec)
		}
		return true
	})
	return out
}

func optionList(spec string) string {
	spec = strings.TrimPrefix(spec, ":")
	var opts []string
	for i := 0; i < len(spec); i++ {
		c := spec[i]
		if c == ':' || c >= 0x80 {
			continue
		}
		o := "-" + string(c)
		if i+1 < len(spec) && spec[i+1] == ':' {
			o += " ARG"
		}
		if len(opts) == maxGetoptsShown {
			opts = append(opts, "...")
			break
		}
		opts = append(opts, o)
	}
	return strings.Join(opts, " ")
}

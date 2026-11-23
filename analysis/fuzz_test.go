package analysis

import (
	"testing"
	"time"

	"github.com/wamra/bash-lsp/internal/front"
	"github.com/wamra/bash-lsp/tree"
)

// FuzzBind parses and binds arbitrary input: no panic, every variable use and
// declaration lies inside the file, and a use that came from arithmetic (an
// arithmetic node, or a subscript or slice inside an expansion or assignment)
// covers exactly its name's bytes. The name range of a plain `$x` is the
// parser's, not this package's, and is not asserted.
func FuzzBind(f *testing.F) {
	for _, s := range []string{
		arithProgram,
		"echo $(( $x + y ))\n",
		"(( a[b[c]] = d ? e : f++, --g ))\n",
		"let \"x=x+1\" y++ 'z=1'\n",
		"declare -A m; echo \"${m[key]} ${v:o+1:l*2} ${a[i]:1:n}\" m[k]=1\n",
		"for ((i=0;i<n;i++)); do ((s+=i)); done\n",
		"echo \"héllo ✓\" $(( é + yy )) ${arr[ñ]}\n",
		"x=${arr[$((i+1))]} y=$(( ${z:-3} ** 2 ))\n",
		"( (( n++ )) ); echo $n; f() { (( g-- )); }\n",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, src []byte) {
		if len(src) > 1<<16 {
			return
		}
		// front.Parse can spin on some inputs (known, owned by the nesting work, e.g.
		// "${0=$((((..." with stray bytes); this fuzz target is about Bind, so give the
		// parse a deadline and skip the input rather than hang the worker.
		parsed := make(chan *tree.File, 1)
		go func() { parsed <- front.Parse("/ws/f.sh", src, front.Bash) }()
		var file *tree.File
		select {
		case file = <-parsed:
		case <-time.After(3 * time.Second):
			t.Skip("front.Parse did not return")
		}
		ff := Bind(file)
		check := func(what, name string, s, e int, arith bool) {
			if s < 0 || e > len(src) || s > e {
				t.Fatalf("%s %q: range [%d,%d) outside file of %d bytes", what, name, s, e, len(src))
			}
			if arith && string(src[s:e]) != name {
				t.Fatalf("%s %q: range text %q", what, name, src[s:e])
			}
		}
		for _, u := range ff.Uses {
			if u.Kind == UseVarRead || u.Kind == UseVarWrite {
				arith := false
				switch u.Node.Kind {
				case tree.KArith, tree.KArithExp, tree.KLet, tree.KCStyleFor:
					arith = true
				case tree.KParamExp, tree.KAssign:
					arith = u.Range != u.Node.NameRange
				}
				check("use", u.Name, u.Range.Start.Offset, u.Range.End.Offset, arith)
			}
		}
		for _, d := range ff.Decls {
			if d.Kind == DeclVariable {
				check("decl", d.Name, d.NameRange.Start.Offset, d.NameRange.End.Offset, d.Implicit == "arith")
			}
		}
	})
}

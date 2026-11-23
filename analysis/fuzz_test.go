package analysis

import (
	"testing"

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
		"eval \"f \\\"$x\\\"\"; trap 'log \"a b\"' EXIT\n",
		"bash -c 'g $((i+1))'; sh -ec \"h ${v:-1}\"\n",
		"cmd=f\n\"$cmd\" x; ${cmd} y\n",
		"eval \"x; \\\n  f\"; eval \\f\n",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, src []byte) {
		if len(src) > 1<<16 {
			return
		}
		file := front.Parse("/ws/f.sh", src, front.Bash)
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

// FuzzEdges builds a snapshot of arbitrary input, so every eval, trap and
// shell -c string goes through string-body parsing and the offset map, and
// asks for every edge: no panic; every call use and every edge site lies
// inside the file, with line and column matching its offset.
func FuzzEdges(f *testing.F) {
	for _, s := range []string{
		"f() { :; }\ntrap 'log \"a b\"' EXIT\neval \"f \\\"$x\\\"\"\n",
		"eval \"eval 'eval \\\"eval f\\\"'\"\n",
		"bash -ec 'f; g' n a\nsh -o pipefail -c \"$x\"\ntrap -- 'f\n g' INT\n",
		"cmd=f\n\"$cmd\" x\n${a}_${b}\n$(pick) y\n./run.sh\nbash x.sh\n",
		"o() { local v=1; i; }\ni() { echo $v; }\n",
		"eval 'f'\"; g\" b\\ c $'x' \"$(h)\" \"\\$(k)\"\n",
		"eval \"",
		"trap '",
		"eval \"x; \\\n  f\"\neval \\f\neval f\\\noo\neval \"x; \\\r\nf\"\n",
		"k() { :; }\nunset -fv -- k\nk\n/bin/sh -c 'k'\n",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, src []byte) {
		if len(src) > 1<<14 {
			return
		}
		p := "/ws/f.sh"
		file := front.Parse(p, src, front.Bash)
		ff := Bind(file)
		check := func(what string, r tree.Range) {
			a, b := r.Start.Offset, r.End.Offset
			if a < 0 || b > len(src) || a > b {
				t.Fatalf("%s: range [%d,%d) outside file of %d bytes", what, a, b, len(src))
			}
			if r.Start != file.PosAt(a) || r.End != file.PosAt(b) {
				t.Fatalf("%s: range %+v disagrees with its offsets", what, r)
			}
		}
		for _, u := range ff.Uses {
			if u.Kind == UseCall {
				check("call use "+u.Name, u.Range)
				if u.NamesTarget() && string(src[u.Range.Start.Offset:u.Range.End.Offset]) != u.Name {
					t.Fatalf("call use %q (via %d) claims its range spells it, text %q", u.Name, u.Via, src[u.Range.Start.Offset:u.Range.End.Offset])
				}
			}
		}
		s := Build(map[string]*FileFacts{p: ff}, Options{Root: "/ws"}, 1)
		for _, bs := range s.bindings {
			for _, b := range bs {
				if u := b.Use; u.Kind == UseCall && u.NamesTarget() {
					check("binding "+u.Name, u.Range)
					if string(src[u.Range.Start.Offset:u.Range.End.Offset]) != u.Name {
						t.Fatalf("binding %q (via %d): range text %q", u.Name, u.Via, src[u.Range.Start.Offset:u.Range.End.Offset])
					}
				}
			}
		}
		for _, e := range s.Edges() {
			if e.Site.Path == p {
				check("edge "+e.Reason, e.Site.Range)
			}
		}
	})
}

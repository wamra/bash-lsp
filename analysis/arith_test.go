package analysis

import (
	"fmt"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/internal/front"
)

func bindSrcT(src string) *FileFacts {
	b := []byte(src)
	return Bind(front.Parse("/ws/a.sh", b, front.Bash))
}

// usesOf returns the uses of name with their kinds, each checked to be an exact byte range of the name.
func usesOf(t *testing.T, ff *FileFacts, name string) []*Use {
	t.Helper()
	var out []*Use
	for _, u := range ff.Uses {
		if u.Name != name || (u.Kind != UseVarRead && u.Kind != UseVarWrite) {
			continue
		}
		if got := string(ff.File.Src[u.Range.Start.Offset:u.Range.End.Offset]); got != name {
			t.Fatalf("use of %q has range text %q", name, got)
		}
		out = append(out, u)
	}
	return out
}

func countKinds(us []*Use) (reads, writes int) {
	for _, u := range us {
		if u.Kind == UseVarRead {
			reads++
		} else {
			writes++
		}
	}
	return
}

const arithProgram = "n=3\nfor ((i=0;i<n;i++)); do echo $((n+1)); done\n(( n++ ))\nlet n=n+1\narr=(a b); echo \"${arr[n]}\"\n"

func TestArithmeticReferences(t *testing.T) {
	q := buildMem(t, map[string]string{"m.sh": arithProgram})
	// n=3, i<n, n+1, n++ (one location for its read and write), both n in let, arr[n]
	got := q.refs("m.sh", "n=3", 0, 0)
	if len(got) != 7 {
		t.Fatalf("references of n: %v", got)
	}
	wantEq(t, got, "m.sh:1", "m.sh:2", "m.sh:2", "m.sh:3", "m.sh:4", "m.sh:4", "m.sh:5")
	// i has a symbol: i=0, i<n, i++
	wantEq(t, q.refs("m.sh", "i=0", 0, 0), "m.sh:2", "m.sh:2", "m.sh:2")
	wantEq(t, q.def("m.sh", "i<n", 0, 0), "m.sh:2")
}

func TestArithmeticWriteKinds(t *testing.T) {
	ff := bindSrcT(arithProgram)
	n := usesOf(t, ff, "n")
	r, w := countKinds(n)
	// writes: n=3, n++, let n=; reads: i<n, n+1, n++, let n+1, arr[n]
	if r != 5 || w != 3 {
		t.Fatalf("n: %d reads, %d writes", r, w)
	}
	var decls int
	for _, d := range ff.Decls {
		if d.Kind == DeclVariable && d.Name == "n" {
			decls++
			if d.Implicit == "arith" {
				t.Fatalf("n already declared by n=3: arithmetic wrote a new declaration")
			}
		}
	}
	if decls != 1 {
		t.Fatalf("n decls: %d", decls)
	}
	for _, d := range ff.Decls {
		if d.Name == "i" && d.Implicit != "arith" {
			t.Fatalf("i decl implicit %q", d.Implicit)
		}
	}
}

func TestArithmeticOperators(t *testing.T) {
	cases := []struct {
		src          string
		reads, write int
	}{
		{"(( x = 1 ))", 0, 1},
		{"(( x == 1 ))", 1, 0},
		{"(( x <= 1 ))", 1, 0},
		{"(( x != 1 ))", 1, 0},
		{"(( x += 1 ))", 1, 1},
		{"(( x -= 1 ))", 1, 1},
		{"(( x *= 1 ))", 1, 1},
		{"(( x /= 1 ))", 1, 1},
		{"(( x %= 1 ))", 1, 1},
		{"(( x <<= 1 ))", 1, 1},
		{"(( x >>= 1 ))", 1, 1},
		{"(( x &= 1 ))", 1, 1},
		{"(( x ^= 1 ))", 1, 1},
		{"(( x |= 1 ))", 1, 1},
		{"(( x++ ))", 1, 1},
		{"(( x-- ))", 1, 1},
		{"(( ++x ))", 1, 1},
		{"(( --x ))", 1, 1},
		{"(( x ))", 1, 0},
		{"(( x ? 1 : 2 ))", 1, 0},
		{"(( 1 + x ))", 1, 0},
		{"let x=1", 0, 1},
		{"let x++", 1, 1},
		{"let \"x=x+1\"", 1, 1},
		{"echo $((x+=2))", 1, 1},
		{"for ((x=0;x<3;x++)); do :; done", 2, 2},
	}
	for _, c := range cases {
		r, w := countKinds(usesOf(t, bindSrcT(c.src+"\n"), "x"))
		if r != c.reads || w != c.write {
			t.Errorf("%q: %d reads %d writes, want %d and %d", c.src, r, w, c.reads, c.write)
		}
	}
}

func TestArithmeticNotNames(t *testing.T) {
	for _, src := range []string{
		"echo $(( 0x1F + 16#ff ))",
		"echo $(( 1e3 + 64#a_b + 2#101 ))",
		"(( 07 + 0XfF ))",
	} {
		for _, u := range bindSrcT(src + "\n").Uses {
			if u.Kind != UseCall {
				t.Errorf("%q: unexpected use %s at %d", src, u.Name, u.Range.Start.Offset)
			}
		}
	}
	// the associative subscript is a string
	ff := bindSrcT("declare -A m\necho \"${m[key]}\"\nm[k2]=1\n(( m[k3] ))\n")
	for _, name := range []string{"key", "k2", "k3"} {
		if us := usesOf(t, ff, name); len(us) != 0 {
			t.Errorf("%s used as a name inside an associative subscript", name)
		}
	}
	// declared later in the file (a function above the declaration) is still associative
	ff = bindSrcT("f() { echo \"${m[late]}\"; }\ndeclare -A m\n")
	if us := usesOf(t, ff, "late"); len(us) != 0 {
		t.Errorf("late used as a name")
	}
	// indexed arrays keep theirs
	ff = bindSrcT("declare -a a\necho \"${a[idx]}\"\na[w]=1\n")
	for _, name := range []string{"idx", "w"} {
		if us := usesOf(t, ff, name); len(us) != 1 {
			t.Errorf("%s: %d uses", name, len(us))
		}
	}
}

func TestArithmeticExpansionsAreNotCountedTwice(t *testing.T) {
	ff := bindSrcT("echo $(( $x + y ))\necho $(( ${z} * $(echo w) + $((v+1)) ))\n")
	for name, want := range map[string]int{"x": 1, "y": 1, "z": 1, "v": 1, "w": 0} {
		if got := len(usesOf(t, ff, name)); got != want {
			t.Errorf("%s: %d uses, want %d", name, got, want)
		}
	}
	// $x and ${x} inside arithmetic stay with the parameter path: their range starts at the name
	us := usesOf(t, ff, "x")
	if len(us) != 1 || us[0].Node == nil || us[0].Node.Kind.String() != "ParamExp" {
		t.Fatalf("x: %+v", us)
	}
}

func TestArithmeticRangesAfterUTF8(t *testing.T) {
	src := "echo \"héllo wörld ✓\" $((  yy  + zz )) \"ñ\"; (( kk++ ))\n"
	ff := bindSrcT(src)
	for _, name := range []string{"yy", "zz", "kk"} {
		if us := usesOf(t, ff, name); len(us) == 0 { // usesOf asserts the exact range text
			t.Errorf("%s not found", name)
		}
	}
	for _, u := range ff.Uses {
		if u.Kind != UseCall && strings.ContainsAny(u.Name, "éöñ✓") {
			t.Errorf("non-ASCII token became a name: %q", u.Name)
		}
	}
}

func TestArithmeticSubscriptsAndSlices(t *testing.T) {
	ff := bindSrcT("echo \"${arr[a+b]} ${s:off:len} ${s: -n} ${t:-dflt} ${arr[$i+j]}\"\narr[k+1]=2\n")
	for name, want := range map[string]int{"a": 1, "b": 1, "off": 1, "len": 1, "n": 1, "dflt": 0, "i": 1, "j": 1, "k": 1} {
		if got := len(usesOf(t, ff, name)); got != want {
			t.Errorf("%s: %d uses, want %d", name, got, want)
		}
	}
	// arithmetic-form subscript writes
	ff = bindSrcT("(( arr[k] = 5, other[j]++ ))\n")
	if r, w := countKinds(usesOf(t, ff, "arr")); r != 0 || w != 1 {
		t.Errorf("arr: %d reads %d writes", r, w)
	}
	if r, w := countKinds(usesOf(t, ff, "other")); r != 1 || w != 1 {
		t.Errorf("other: %d reads %d writes", r, w)
	}
	if len(usesOf(t, ff, "k")) != 1 || len(usesOf(t, ff, "j")) != 1 {
		t.Errorf("subscript names missing")
	}
}

func TestArithmeticWriteBindsExistingSymbol(t *testing.T) {
	q := buildMem(t, map[string]string{"m.sh": "n=1\n(( n += 2 ))\necho \"$n\"\nf() {\n  (( g++ ))\n  (( loc++ ))\n}\necho \"$g\"\n"})
	wantEq(t, q.refs("m.sh", "n=1", 0, 0), "m.sh:1", "m.sh:2", "m.sh:3")
	// inside a function an undeclared write declares a global, exactly like g=1 does
	wantEq(t, q.def("m.sh", "$g", 0, 1), "m.sh:5")
	q = buildMem(t, map[string]string{"m.sh": "f() {\n  local c=0\n  (( c++ ))\n  (( c++ ))\n}\nc=9\n"})
	wantEq(t, q.refs("m.sh", "local c", 0, 6), "m.sh:2", "m.sh:3", "m.sh:4")
	wantEq(t, q.refs("m.sh", "c=9", 0, 0), "m.sh:6")
}

func TestArithmeticFollowsScopeRules(t *testing.T) {
	// first declared inside the subshell: the outer read must not bind the inner write
	q := buildMem(t, map[string]string{"m.sh": "( (( n++ )) )\necho \"$n\"\n"})
	wantEq(t, q.def("m.sh", "$n", 0, 1))
	if r := q.reasonAt("m.sh", "$n", 0, 1); r != ReasonEnvironment {
		t.Fatalf("reason %q", r)
	}
	// declared outside: the subshell write declares a private n, like n=2
	// there would (review M1), so neither the outer read nor n=1 sees it
	q = buildMem(t, map[string]string{"m.sh": "n=1\n( (( n++ )) )\necho \"$n\"\n"})
	wantEq(t, q.def("m.sh", "$n", 0, 1), "m.sh:1")
	wantEq(t, q.refs("m.sh", "n=1", 0, 0), "m.sh:1", "m.sh:3")
	// pipeline stage and command substitution
	q = buildMem(t, map[string]string{"m.sh": "echo | (( p++ ))\nv=$(( q = 1 ))\nw=$( (( r++ )) )\necho \"$p $q $r\"\n"})
	wantEq(t, q.def("m.sh", "$p", 0, 1))
	wantEq(t, q.def("m.sh", "$r", 0, 1))
	wantEq(t, q.def("m.sh", "$q", 0, 1), "m.sh:2")
	// an inner read still binds the inner write
	q = buildMem(t, map[string]string{"m.sh": "( (( n = 5 )); echo \"$n\" )\n"})
	wantEq(t, q.def("m.sh", "$n", 0, 1), "m.sh:1")
}

func TestIntegerAttributeAssignmentIsArithmetic(t *testing.T) {
	q := buildMem(t, map[string]string{"m.sh": "declare -i n\nn=n+1\nm=2\nn+=m\n"})
	// n: declaration, the RHS read on line 2, the assignment targets; m: m=2 and the read on line 4
	wantEq(t, q.refs("m.sh", "n\nn=", 0, 0), "m.sh:1", "m.sh:2", "m.sh:2", "m.sh:4")
	wantEq(t, q.refs("m.sh", "m=2", 0, 0), "m.sh:3", "m.sh:4")
	for _, src := range []string{
		"f() { local -i n=0; n=n+1; }\n", "typeset -i n; n=n+1\n", "declare -i n=n+1\n", "export -i n; n+=n\n",
	} {
		ff := bindSrcT(src)
		if r, _ := countKinds(usesOf(t, ff, "n")); r == 0 {
			t.Errorf("%q: no read of n", src)
		}
	}
	// a plain variable's value is a string
	ff := bindSrcT("s=n+1\nt=\"x+y\"\n")
	for _, name := range []string{"n", "x", "y"} {
		if us := usesOf(t, ff, name); len(us) != 0 {
			t.Errorf("%s read in a non-integer assignment", name)
		}
	}
	// the attribute is scoped like the declaration: a subshell's -i does not reach the outer assignment
	ff = bindSrcT("( declare -i n )\nn=a+1\n")
	if us := usesOf(t, ff, "a"); len(us) != 0 {
		t.Errorf("integer attribute escaped its subshell")
	}
	// the value's expansions are not counted twice
	ff = bindSrcT("declare -i n\nn=$k+j\n")
	if len(usesOf(t, ff, "k")) != 1 || len(usesOf(t, ff, "j")) != 1 {
		t.Errorf("k or j count wrong")
	}
}

func TestArithmeticWriteAcrossWhitespace(t *testing.T) {
	cases := []struct {
		src          string
		reads, write int
	}{
		{"echo $((x ++))", 1, 1},
		{"(( x ++ ))", 1, 1},
		{"(( x -- ))", 1, 1},
		{"(( -- x ))", 1, 1},
		{"(( ++\tx ))", 1, 1},
		{"(( x \\\n += 2 ))", 1, 1},
		{"(( x \\\n = 2 ))", 0, 1},
		{"(( x\r\n= 2 ))", 0, 1},
		{"(( x \\\r\n ++ ))", 1, 1},
		{"(( x + y ))", 1, 0},
		{"(( x - -y ))", 1, 0},
		{"(( x + +y ))", 1, 0},
	}
	for _, c := range cases {
		r, w := countKinds(usesOf(t, bindSrcT(c.src+"\n"), "x"))
		if r != c.reads || w != c.write {
			t.Errorf("%q: %d reads %d writes, want %d and %d", c.src, r, w, c.reads, c.write)
		}
	}
}

func TestCStyleForClauseBoundsWithQuotedParens(t *testing.T) {
	src := "for ((i=0;i<\"(\";i++)); do echo \"$j\"; (( kk++ )); done\n"
	ff := bindSrcT(src)
	if r, w := countKinds(usesOf(t, ff, "i")); r != 2 || w != 2 {
		t.Errorf("i: %d reads %d writes", r, w)
	}
	// the body is not scanned as part of the clauses: kk once (read+write), j once (parameter path)
	if r, w := countKinds(usesOf(t, ff, "kk")); r != 1 || w != 1 {
		t.Errorf("kk: %d reads %d writes", r, w)
	}
	ff = bindSrcT("for ((i=0;i<')';i++)); do (( z++ )); done\nfor ((a=0;a<$(echo \")\");a++)); do :; done\n")
	if r, w := countKinds(usesOf(t, ff, "z")); r != 1 || w != 1 {
		t.Errorf("z: %d reads %d writes", r, w)
	}
	if r, w := countKinds(usesOf(t, ff, "a")); r != 2 || w != 2 {
		t.Errorf("a: %d reads %d writes", r, w)
	}
}

func TestQuotedTextInArithmetic(t *testing.T) {
	// bash: inside (( )), $(( )) and for (( )) the text is treated as double-quoted, so a
	// double-quoted name is evaluated after quote removal and a single quote is an ordinary character.
	// As a let argument, single quotes are shell quoting around the expression.
	cases := []struct {
		src  string
		name string
		want int
	}{
		{"(( \"d\" + 1 ))", "d", 1},
		{"echo $(( \"d\" + 1 ))", "d", 1},
		{"(( 'u' + 1 ))", "u", 0},
		{"echo $(( 'u' + 1 ))", "u", 0},
		{"for (( i=0; i<'u'; i++ )); do :; done", "u", 0},
		{"let 'q=q+1'", "q", 2},
		{"let \"q=q+1\"", "q", 2},
	}
	for _, c := range cases {
		if got := len(usesOf(t, bindSrcT(c.src+"\n"), c.name)); got != c.want {
			t.Errorf("%q: %d uses of %s, want %d", c.src, got, c.name, c.want)
		}
	}
}

func TestArrayLiteralSubscripts(t *testing.T) {
	ff := bindSrcT("a=([i+1]=x [j]=\"p q\" c [$k+l]=$(echo m) )\nb+=([n]=1)\ndeclare -A m=([key]=v)\n")
	for name, want := range map[string]int{"i": 1, "j": 1, "k": 1, "l": 1, "n": 1, "key": 0, "x": 0, "c": 0} {
		if got := len(usesOf(t, ff, name)); got != want {
			t.Errorf("%s: %d uses, want %d", name, got, want)
		}
	}
}

// review M1: an arithmetic write inside a private scope declares a variable
// private to it, exactly like a plain assignment there: the read inside binds
// to it, the read after the scope to the outer declaration. For every private
// scope kind and every arithmetic write form.
func TestArithmeticWriteInPrivateScopeIsPrivate(t *testing.T) {
	scopes := map[string]string{
		"subshell":     "( %s\necho $y )\n",
		"cmdsubst":     "x=$( %s\necho $y )\n",
		"pipeline":     "cmd | { %s\necho $y; }\n",
		"procsubst":    "cat <( %s\necho $y )\n",
		"coproc":       "coproc { %s\necho $y; }\n",
		"cmdsubst-arg": "echo \"$( %s\necho $y )\"\n",
	}
	forms := []string{"((y=2))", "((y++))", "((--y))", "((y+=1))", "let y=2", "let y++", "echo $((y=2))", "for ((y=0;;)); do :; done"}
	for kind, shape := range scopes {
		for _, form := range forms {
			src := "y=1\n" + fmt.Sprintf(shape, form) + "echo $y\n"
			q := buildMem(t, map[string]string{"m.sh": src})
			p := q.abs("m.sh")
			line3 := strings.Index(src, "\necho $y") + 1 // the scope's read, on line 3
			if got := q.strs(q.s.Definition(p, line3+6)); strings.Join(got, ",") != "m.sh:2" {
				t.Errorf("%s, %s: inner read defined at %v, want the write on line 2\n%s", kind, form, got, src)
			}
			if got := q.strs(q.s.Definition(p, len(src)-2)); strings.Join(got, ",") != "m.sh:1" {
				t.Errorf("%s, %s: read after the scope defined at %v, want y=1\n%s", kind, form, got, src)
			}
		}
	}
}

// In arithmetic the binder reads the source text, and a NUL ends a name: the
// bytes after it start the next (docs/m2a-acceptance.md, NUL name rules).
func TestArithmeticSplitsNameAtNUL(t *testing.T) {
	for src, want := range map[string]string{
		"((A\x00B=1))\n":       "read A 2-3, write B 4-5, decl B 4-5",
		"let A\x00B=1\n":       "read A 4-5, write B 6-7, decl B 6-7",
		"echo $((A\x00B+1))\n": "read A 8-9, read B 10-11",
	} {
		ff := bindSrcT(src)
		var got []string
		for _, u := range ff.Uses {
			switch u.Kind {
			case UseVarRead:
				got = append(got, fmt.Sprintf("read %s %d-%d", u.Name, u.Range.Start.Offset, u.Range.End.Offset))
			case UseVarWrite:
				got = append(got, fmt.Sprintf("write %s %d-%d", u.Name, u.Range.Start.Offset, u.Range.End.Offset))
			}
		}
		for _, d := range ff.Decls {
			got = append(got, fmt.Sprintf("decl %s %d-%d", d.Name, d.NameRange.Start.Offset, d.NameRange.End.Offset))
		}
		if g := strings.Join(got, ", "); g != want {
			t.Errorf("%q: %s, want %s", src, g, want)
		}
	}
}

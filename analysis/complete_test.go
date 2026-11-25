package analysis

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/internal/front"
)

// compAt builds srcs (the file named by at holds the ▮ cursor mark, removed)
// and asks for completions there.
func compAt(t *testing.T, srcs map[string]string, at string) (CompletionContext, string, []CompletionItem, bool) {
	t.Helper()
	off := strings.Index(srcs[at], "▮")
	if off < 0 {
		t.Fatal("no cursor mark")
	}
	m := map[string]string{}
	for k, v := range srcs {
		m[k] = v
	}
	m[at] = strings.Replace(m[at], "▮", "", 1)
	q := buildMem(t, m)
	return q.s.Completions(filepath.Join(q.root, at), off)
}

func labels(items []CompletionItem) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.Label)
	}
	sort.Strings(out)
	return out
}

func has(items []CompletionItem, label string) (CompletionItem, bool) {
	for _, it := range items {
		if it.Label == label {
			return it, true
		}
	}
	return CompletionItem{}, false
}

func TestCompleteVariablesInFunction(t *testing.T) {
	ctx, _, items, inc := compAt(t, map[string]string{"a.sh": "g=1; f() { local a; echo $▮; }; h() { local b; }"}, "a.sh")
	if ctx != CtxVariable || inc {
		t.Fatalf("ctx %d incomplete %v", ctx, inc)
	}
	for _, want := range []string{"a", "g"} {
		if _, ok := has(items, want); !ok {
			t.Errorf("%q missing from %v", want, labels(items))
		}
	}
	if _, ok := has(items, "b"); ok {
		t.Errorf("b of another function offered: %v", labels(items))
	}
}

func TestCompleteVariableReplaceRange(t *testing.T) {
	_, _, items, _ := compAt(t, map[string]string{"a.sh": "name=1\necho \"$na▮\"\n"}, "a.sh")
	it, ok := has(items, "name")
	if !ok || it.Kind != CompVariable {
		t.Fatalf("got %v", labels(items))
	}
	if it.Replace.Start.Col != 8 || it.Replace.End.Col != 10 || it.Replace.Start.Line != 2 {
		t.Fatalf("replace %+v", it.Replace)
	}
	if _, ok := has(items, "x"); ok {
		t.Fatal("prefix not applied")
	}
}

func TestCompleteCommandSourcedFunctions(t *testing.T) {
	ctx, cmd, items, _ := compAt(t, map[string]string{
		"main.sh":     "source ./lib.sh\nlib▮\n",
		"lib.sh":      "libfn() { :; }\n",
		"isolated.sh": "isolated() { :; }\n",
	}, "main.sh")
	if ctx != CtxCommand || cmd != "" {
		t.Fatalf("ctx %d cmd %q", ctx, cmd)
	}
	it, ok := has(items, "libfn")
	if !ok || it.Kind != CompFunction || it.Sym == nil {
		t.Fatalf("libfn: %v", labels(items))
	}
	_, _, items, _ = compAt(t, map[string]string{
		"main.sh":     "source ./lib.sh\n▮\n",
		"lib.sh":      "libfn() { :; }\n",
		"isolated.sh": "isolated() { :; }\n",
	}, "main.sh")
	if _, ok := has(items, "libfn"); !ok {
		t.Fatalf("libfn missing at an empty command position: %d items", len(items))
	}
	if _, ok := has(items, "isolated"); ok {
		t.Fatal("isolated of an unrelated file offered")
	}
}

func TestCompleteCommandKinds(t *testing.T) {
	_, _, items, _ := compAt(t, map[string]string{"a.sh": "alias ll='ls -l'\nfunction mine() { :; }\n▮"}, "a.sh")
	for label, kind := range map[string]CompletionKind{"ll": CompAlias, "mine": CompFunction, "printf": CompBuiltin, "while": CompKeyword} {
		if it, ok := has(items, label); !ok || it.Kind != kind {
			t.Errorf("%s: %+v ok %v, want kind %d", label, it, ok, kind)
		}
	}
	if it, ok := has(items, "coproc"); !ok || it.Kind != CompKeyword || it.Feature != "coproc" {
		t.Errorf("coproc: %+v ok %v", it, ok)
	}
}

func TestCompleteCommandPositions(t *testing.T) {
	for _, src := range []string{
		"echo hi; ▮", "echo hi && ▮", "a | ▮", "if ▮", "while ▮", "x=1 ▮", "( ▮", "echo $(▮", "{ ▮", "if true; then ▮",
	} {
		ctx, _, _, _ := compAt(t, map[string]string{"a.sh": src}, "a.sh")
		if ctx != CtxCommand {
			t.Errorf("%q: ctx %d, want command", src, ctx)
		}
	}
	for _, src := range []string{"echo ▮", "echo $(date) ▮", "ls > ▮", "echo \"▮", "echo '▮", "# note ▮", "echo hi # x▮"} {
		ctx, _, items, _ := compAt(t, map[string]string{"a.sh": src}, "a.sh")
		if ctx == CtxCommand || len(items) != 0 {
			t.Errorf("%q: ctx %d with %d items, want none and not a command", src, ctx, len(items))
		}
	}
}

func TestCompleteParamOps(t *testing.T) {
	ctx, _, items, _ := compAt(t, map[string]string{"a.sh": "echo ${x▮"}, "a.sh")
	if ctx != CtxParamOp {
		t.Fatalf("ctx %d", ctx)
	}
	for _, op := range []string{":-", ":=", ":?", ":+", "#", "##", "%", "%%", "/", "^", "^^", ",", ",,", "@Q"} {
		if it, ok := has(items, op); !ok || it.Kind != CompParamOp || it.Insert != op {
			t.Errorf("%q: %+v ok %v", op, it, ok)
		}
	}
	it, _ := has(items, "^^")
	if it.Detail != "needs bash 4.0; target starts at 3.2" || it.Feature != "case-modification" {
		t.Errorf("^^: %+v", it)
	}
	if it, _ := has(items, "@Q"); it.Detail != "needs bash 4.4; target starts at 3.2" || it.Feature != "transform-op" {
		t.Errorf("@Q: %+v", it)
	}
	if it, _ := has(items, ":-"); strings.Contains(it.Detail, "needs bash") || it.Feature != "" {
		t.Errorf(":-: %+v", it)
	}
	// The op goes in at the cursor.
	if it.Replace.Start != it.Replace.End {
		t.Errorf("replace not empty: %+v", it.Replace)
	}
}

func TestCompleteParamOpsAlsoContinuesName(t *testing.T) {
	_, _, items, _ := compAt(t, map[string]string{"a.sh": "name_long=1\necho ${name▮"}, "a.sh")
	if _, ok := has(items, "name_long"); !ok {
		t.Fatalf("got %v", labels(items))
	}
	if _, ok := has(items, ":-"); !ok {
		t.Fatalf("no ops: %v", labels(items))
	}
}

func TestCompleteBracedVariable(t *testing.T) {
	ctx, _, items, _ := compAt(t, map[string]string{"a.sh": "foo=1\necho ${f▮"}, "a.sh")
	if ctx != CtxParamOp {
		t.Fatalf("ctx %d", ctx)
	}
	if _, ok := has(items, "foo"); !ok {
		t.Fatalf("got %v", labels(items))
	}
	ctx, _, items, _ = compAt(t, map[string]string{"a.sh": "foo=1\necho ${▮"}, "a.sh")
	if ctx != CtxVariable {
		t.Fatalf("ctx %d", ctx)
	}
	if _, ok := has(items, "foo"); !ok {
		t.Fatalf("got %v", labels(items))
	}
}

func TestCompleteSourcePath(t *testing.T) {
	srcs := map[string]string{
		"main.sh":      "source ./l▮",
		"lib.sh":       "x=1\n",
		"notes.txt":    "plain",
		"lib/inner.sh": "y=1\n",
		"other.sh":     "z=1\n",
	}
	ctx, cmd, items, _ := compAt(t, srcs, "main.sh")
	if ctx != CtxSourcePath || cmd != "source" {
		t.Fatalf("ctx %d cmd %q", ctx, cmd)
	}
	if it, ok := has(items, "lib.sh"); !ok || it.Kind != CompFile || it.Insert != "lib.sh" {
		t.Fatalf("lib.sh: %v", labels(items))
	}
	if it, ok := has(items, "lib/"); !ok || it.Kind != CompFolder {
		t.Fatalf("lib/: %v", labels(items))
	}
	if _, ok := has(items, "other.sh"); ok {
		t.Fatal("prefix not applied")
	}
	// The replace range is the part after the last slash.
	it, _ := has(items, "lib.sh")
	if it.Replace.Start.Col != 10 || it.Replace.End.Col != 11 {
		t.Fatalf("replace %+v", it.Replace)
	}
	srcs["main.sh"] = ". lib/▮"
	srcs["lib/inner.sh"] = "y=1\n"
	ctx, _, items, _ = compAt(t, srcs, "main.sh")
	if ctx != CtxSourcePath {
		t.Fatalf("dot: ctx %d", ctx)
	}
	if _, ok := has(items, "inner.sh"); !ok {
		t.Fatalf("lib/: %v", labels(items))
	}
	// Outside the root nothing is offered.
	srcs["main.sh"] = "source ../▮"
	_, _, items, _ = compAt(t, srcs, "main.sh")
	if len(items) != 0 {
		t.Fatalf("../ offered %v", labels(items))
	}
}

func TestCompleteSubshellVariableScope(t *testing.T) {
	_, _, items, _ := compAt(t, map[string]string{"a.sh": "( z=1; echo $▮ )"}, "a.sh")
	if _, ok := has(items, "z"); !ok {
		t.Fatalf("z missing: %v", labels(items))
	}
	_, _, items, _ = compAt(t, map[string]string{"a.sh": "( z=1 ); echo $▮"}, "a.sh")
	if _, ok := has(items, "z"); ok {
		t.Fatalf("z leaked out of the subshell: %v", labels(items))
	}
}

func TestCompleteEnvironmentNames(t *testing.T) {
	_, _, items, _ := compAt(t, map[string]string{"a.sh": "echo \"$HOME\"\necho $HO▮"}, "a.sh")
	if _, ok := has(items, "HOME"); !ok {
		t.Fatalf("HOME missing: %v", labels(items))
	}
	if _, ok := has(items, "HO"); ok {
		t.Fatal("the word being typed offered as a name")
	}
}

func TestCompleteOptionContext(t *testing.T) {
	ctx, cmd, items, _ := compAt(t, map[string]string{"a.sh": "grep -▮"}, "a.sh")
	if ctx != CtxOption || cmd != "grep" || len(items) != 0 {
		t.Fatalf("ctx %d cmd %q items %d", ctx, cmd, len(items))
	}
	ctx, cmd, _, _ = compAt(t, map[string]string{"a.sh": "echo hi | sort --re▮"}, "a.sh")
	if ctx != CtxOption || cmd != "sort" {
		t.Fatalf("ctx %d cmd %q", ctx, cmd)
	}
}

func TestCompleteArgumentGivesCommand(t *testing.T) {
	ctx, cmd, items, _ := compAt(t, map[string]string{"a.sh": "ls foo▮"}, "a.sh")
	if ctx != CtxArgument || cmd != "ls" || len(items) != 0 {
		t.Fatalf("ctx %d cmd %q items %d", ctx, cmd, len(items))
	}
}

func TestCompletionIncomplete(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 1200; i++ {
		fmt.Fprintf(&sb, "fn%04d() { :; }\n", i)
	}
	sb.WriteString("▮")
	ctx, _, items, inc := compAt(t, map[string]string{"a.sh": sb.String()}, "a.sh")
	if ctx != CtxCommand || !inc || len(items) != maxCompletionItems {
		t.Fatalf("ctx %d incomplete %v items %d", ctx, inc, len(items))
	}
	// A prefix that narrows the list under the cap is complete.
	src := strings.Replace(sb.String(), "▮", "fn00▮", 1)
	_, _, items, inc = compAt(t, map[string]string{"a.sh": src}, "a.sh")
	if inc || len(items) != 100 {
		t.Fatalf("narrowed: incomplete %v items %d", inc, len(items))
	}
}

func TestCompletionUTF16(t *testing.T) {
	// Columns of the replace range are byte columns in the tree; the server
	// converts them. A multi-byte name before the cursor must not shift it.
	_, _, items, _ := compAt(t, map[string]string{"a.sh": "abc=1; echo \"é\" $ab▮"}, "a.sh")
	it, ok := has(items, "abc")
	if !ok {
		t.Fatalf("got %v", labels(items))
	}
	if it.Replace.Start.Col != 19 || it.Replace.End.Col != 21 {
		t.Fatalf("replace %+v", it.Replace)
	}
}

func TestCompleteUnknownPathAndBadOffset(t *testing.T) {
	q := buildMem(t, map[string]string{"a.sh": "echo\n"})
	if ctx, _, items, _ := q.s.Completions(filepath.Join(q.root, "nope.sh"), 0); ctx != 0 || items != nil {
		t.Fatal("unknown path")
	}
	for _, off := range []int{-5, 1 << 20} {
		q.s.Completions(filepath.Join(q.root, "a.sh"), off) // must not panic
	}
}

func TestFeatureNeeds(t *testing.T) {
	if d, gated := FeatureNeeds("case-modification", BashVersion{3, 2}); !gated || d != "needs bash 4.0; target starts at 3.2" {
		t.Fatalf("%q %v", d, gated)
	}
	if d, gated := FeatureNeeds("case-modification", BashVersion{4, 0}); gated || d != "" {
		t.Fatalf("%q %v", d, gated)
	}
	if d, gated := FeatureNeeds("", BashVersion{3, 2}); gated || d != "" {
		t.Fatalf("%q %v", d, gated)
	}
}

func TestKeywordsAreBuiltinNames(t *testing.T) {
	for _, k := range keywordNames {
		if !IsBuiltin(k) {
			t.Errorf("keyword %q is not in the builtin list", k)
		}
	}
}

func TestCompleteNothingInHeredocOrMultilineString(t *testing.T) {
	ctx, _, items, _ := compAt(t, map[string]string{"a.sh": "cat <<EOF\nline\n▮\nEOF\n"}, "a.sh")
	if ctx != 0 || len(items) != 0 {
		t.Errorf("heredoc body: ctx %d, %d items", ctx, len(items))
	}
	ctx, _, items, _ = compAt(t, map[string]string{"a.sh": "x=1\necho 'one\n▮\ntwo'\n"}, "a.sh")
	if ctx != 0 || len(items) != 0 {
		t.Errorf("single-quoted string: ctx %d, %d items", ctx, len(items))
	}
	ctx, _, items, _ = compAt(t, map[string]string{"a.sh": "x=1\necho \"one\n$▮\ntwo\"\n"}, "a.sh")
	if ctx != CtxVariable {
		t.Errorf("double-quoted string: ctx %d", ctx)
	}
	if _, ok := has(items, "x"); !ok {
		t.Errorf("x missing: %v", labels(items))
	}
	ctx, _, _, _ = compAt(t, map[string]string{"a.sh": "echo \"one\n▮\ntwo\"\n"}, "a.sh")
	if ctx != 0 {
		t.Errorf("a word inside a string is no command: ctx %d", ctx)
	}
	ctx, _, _, _ = compAt(t, map[string]string{"a.sh": "echo 'x'\n▮"}, "a.sh")
	if ctx != CtxCommand {
		t.Errorf("after a closed string: ctx %d", ctx)
	}
}

// BenchmarkCompletionBig is completion on the 561 KB script (2,000
// functions): a variable inside the last function, a command word with and
// without a prefix, and the operators after `${x`.
func BenchmarkCompletionBig(b *testing.B) {
	src := string(bigFunctions(2000))
	cases := []struct{ name, tail string }{
		{"variable", "\nlast() {\n  local mine=1\n  echo $m"},
		{"command-prefix", "\nfn_19"},
		{"command-empty", "\n"},
		{"paramop", "\necho ${x"},
		{"source", "\nsource ./"},
	}
	for _, c := range cases {
		text := src + c.tail
		root := "/ws"
		p := root + "/big.sh"
		s := Build(map[string]*FileFacts{p: Bind(front.Parse(p, []byte(text), front.Bash))}, Options{Root: root}, 1)
		b.Run(c.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if ctx, _, _, _ := s.Completions(p, len(text)); ctx == 0 {
					b.Fatal("no context")
				}
			}
		})
	}
}

// TestCompleteNeverPanics feeds scrap built from shell punctuation, with the
// cursor at every offset.
func TestCompleteNeverPanics(t *testing.T) {
	pieces := []string{"$", "${", "}", "(", ")", "$(", "`", "'", "\"", "\\", ";", "|", "&", "<", ">", "<(", "#", " ", "\n", "x", "-", "/", "source ", ". ", "if ", "{ ", "<<E\n", "é", "=", "[[", "((", "${x:", "a=("}
	seed := uint32(7)
	next := func(n int) int {
		seed = seed*1664525 + 1013904223
		return int(seed>>8) % n
	}
	for round := 0; round < 60; round++ {
		var sb strings.Builder
		for i := 0; i < 5+next(30); i++ {
			sb.WriteString(pieces[next(len(pieces))])
		}
		q := buildMem(t, map[string]string{"a.sh": sb.String(), "b.sh": "x=1\n"})
		p := filepath.Join(q.root, "a.sh")
		for off := -1; off <= sb.Len()+1; off++ {
			q.s.Completions(p, off)
			q.s.CompletionWord(p, off)
		}
	}
}

// A function still being typed keeps its locals in view at the cursor, also
// when only blank lines and comment lines follow.
func TestCompleteLocalsOfUnterminatedFunction(t *testing.T) {
	for name, src := range map[string]string{
		"blank":          "f() {\n local aa\n echo $a▮\n\n",
		"comment":        "f() {\n local aa\n echo $a▮\n\n# comment\n",
		"comment spaced": "f() {\n local aa\n echo $a▮  \n  # one\n\n\t# two",
		"same line":      "f() {\n local aa\n echo $a▮ # note\n",
	} {
		_, _, items, _ := compAt(t, map[string]string{"a.sh": src}, "a.sh")
		if _, ok := has(items, "aa"); !ok {
			t.Errorf("%s: aa missing from %v", name, labels(items))
		}
	}
	// code after the cursor is not a blank tail: the function is not simply cut off
	_, _, items, _ := compAt(t, map[string]string{"a.sh": "h() {\n echo $a▮\n}\nf() { local aa; }\n"}, "a.sh")
	if _, ok := has(items, "aa"); ok {
		t.Errorf("aa of another function offered: %v", labels(items))
	}
}

// `f() local aa=1 b=$a▮ ; echo x`: a function body that is no compound command
// is a parse error, and code follows the cursor, so the cursor is not at the
// end of a function still being typed: the local is not in view.
func TestCompleteLocalsAfterMalformedFunctionHead(t *testing.T) {
	_, _, items, _ := compAt(t, map[string]string{"a.sh": "f() local aa=1 b=$a▮ ; echo x\n"}, "a.sh")
	if _, ok := has(items, "aa"); ok {
		t.Errorf("aa offered: %v", labels(items))
	}
}

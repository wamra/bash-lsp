package syntax

// This file is not upstream's. It tests the changes listed in
// bash-lsp.patch (UPGRADING.md).

import (
	"runtime"
	"runtime/metrics"
	"strings"
	"testing"
	"time"
)

func parseBash(src string) error {
	_, err := NewParser(KeepComments(true), RecoverErrors(50), Variant(LangBash)).Parse(strings.NewReader(src), "x.sh")
	return err
}

// The limit this package is vendored for: 10,000 levels parse, one more is
// an error with this text (internal/front pins the limit per construct).
func TestNestingLimit(t *testing.T) {
	nested := func(n int) string { return strings.Repeat("(", n) + "a" + strings.Repeat(")", n) + "\n" }
	if err := parseBash(nested(maxNesting)); err != nil {
		t.Fatalf("%d levels: %v", maxNesting, err)
	}
	err := parseBash(nested(maxNesting + 1))
	if err == nil || !strings.Contains(err.Error(), "nesting is deeper than 10000 levels") {
		t.Fatalf("%d levels: %v", maxNesting+1, err)
	}
}

// An invalid-UTF-8 error raised inside a lexer function whose result the
// caller assigns to p.tok left the parser with an error set and a token that
// is not _EOF; callExpr then reported that token as unexpected forever
// (errPass in the patch). Found by a fuzz target of package analysis.
func TestErrorInsideLexerFunctionEndsTheParse(t *testing.T) {
	src := "${0=$((((\x11ke1\x05]@\x00\x00\x00\xad\xaeq\x16@\"\x80>\x00\x83|k(((($"
	done := make(chan error, 1)
	go func() { done <- parseBash(src) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "invalid UTF-8 encoding") {
			t.Fatalf("error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Parse still running after 10 s")
	}
}

// A comment line must not cost a stack frame: the lexer called itself for
// the token after each one (lexer.go in the patch), so 200,000 comment
// lines took 32 MiB of stack and 2 MiB of them 256 MiB.
func TestCommentLinesUseNoStack(t *testing.T) {
	stacks := func() uint64 {
		s := []metrics.Sample{{Name: "/memory/classes/heap/stacks:bytes"}}
		metrics.Read(s)
		return s[0].Value.Uint64()
	}
	src := strings.Repeat("# c\n", 200000) + "a\n"
	runtime.GC()
	before := stacks()
	grown := make(chan uint64)
	go func() {
		if err := parseBash(src); err != nil {
			t.Error(err)
		}
		grown <- stacks()
	}()
	if after := <-grown; after > before+(4<<20) {
		t.Fatalf("stack memory grew from %d to %d bytes while parsing comments", before, after)
	}
}

// A here-document delimiter with any quoting in it, in any part, makes the
// body literal (bash). Upstream kept only the last part's flag.
func TestHeredocDelimiterQuotedInAnyPart(t *testing.T) {
	for delim, quoted := range map[string]bool{
		`"E"F`: true, `E"F"`: true, `'E'F`: true, `E'F'`: true, `\E`: true, `E\F`: true,
		`EF`: false, `E`: false,
	} {
		src := "cat <<" + delim + "\n$(echo a)\n" + strings.NewReplacer(`"`, "", `'`, "", `\`, "").Replace(delim) + "\n"
		f, err := NewParser(Variant(LangBash)).Parse(strings.NewReader(src), "x.sh")
		if err != nil {
			t.Errorf("%s: %v", delim, err)
			continue
		}
		var body *Word
		Walk(f, func(n Node) bool {
			if r, ok := n.(*Redirect); ok && r.Hdoc != nil {
				body = r.Hdoc
			}
			return true
		})
		if body == nil {
			t.Errorf("%s: no body", delim)
			continue
		}
		expands := false
		for _, p := range body.Parts {
			if _, ok := p.(*CmdSubst); ok {
				expands = true
			}
		}
		if expands == quoted {
			t.Errorf("delimiter %s: body expandable = %v, want %v", delim, expands, !quoted)
		}
	}
}

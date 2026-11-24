package front

import (
	"strings"
	"testing"
	"time"

	"github.com/wamra/bash-lsp/tree"
)

// runResync parses src the way Parse does and returns the resyncer, so a
// test can read what it spent.
func runResync(src string) (*resyncer, []*tree.Node, []tree.Limit) {
	f := tree.NewFile("x.sh", []byte(src))
	r := newResyncer(f, Bash)
	stmts, _, _, limits := r.run()
	return r, stmts, limits
}

func limitNamed(ls []tree.Limit, name string) *tree.Limit {
	for i := range ls {
		if ls[i].Name == name {
			return &ls[i]
		}
	}
	return nil
}

// Review Focus 3, T4-9: a 2 MiB file whose every candidate fails re-parsed
// about 2 MiB fifty times (17.58 s). The byte budget stops it after what
// 4 MiB + 2 x size allows, and says so.
func TestResyncByteBudget(t *testing.T) {
	if raceBuild || testing.Short() {
		t.Skip("parses 8 MiB; skipped under -race and -short")
	}
	src := resyncArith()
	start := time.Now()
	r, stmts, limits := runResync(src)
	took := time.Since(start)
	budget := resyncByteBudget(len(src))
	l := limitNamed(limits, "resync-bytes")
	if l == nil {
		t.Fatalf("no resync-bytes limit: %+v", limits)
	}
	if r.parsed > budget || r.parsed <= budget-len(src) {
		t.Fatalf("parsed %d bytes, budget %d, file %d", r.parsed, budget, len(src))
	}
	t.Logf("attempts %d, reads %v, parsed %d of %d, %v", len(r.reads), r.reads, r.parsed, budget, took)
	if len(r.reads) != 4 {
		t.Fatalf("%d attempts ran, want 4 (reads %v)", len(r.reads), r.reads)
	}
	last := stmts[len(stmts)-1]
	if last.Kind != tree.KErrorGap || last.Range.End.Offset != len(src) {
		t.Fatalf("last root child %v %+v", last.Kind, last.Range)
	}
	if l.Range != last.Range {
		t.Fatalf("limit range %+v, gap %+v", l.Range, last.Range)
	}
	want := "resync stopped after re-parsing "
	if !strings.HasPrefix(l.Detail, want) || !strings.Contains(l.Detail, "-byte file)") {
		t.Fatalf("detail %q", l.Detail)
	}
	if took > 6*time.Second {
		t.Fatalf("took %v", took)
	}
}

// The same line shape in a file of about 600 KB: the budget is 4 MiB + 2 x
// 600 KB, about nine attempts of about 575 KB each.
func TestResyncByteBudgetMidSize(t *testing.T) {
	if raceBuild {
		t.Skip("skipped under -race")
	}
	line := "echo $(( " + strings.Repeat("(a[", 2000) + "1" + strings.Repeat("])", 2000) + " ))\n"
	src := "echo '\n" + strings.Repeat(line, 60)
	start := time.Now()
	r, _, limits := runResync(src)
	took := time.Since(start)
	budget := resyncByteBudget(len(src))
	if limitNamed(limits, "resync-bytes") == nil {
		t.Fatalf("no resync-bytes limit: %+v", limits)
	}
	if r.parsed > budget {
		t.Fatalf("parsed %d bytes, budget %d", r.parsed, budget)
	}
	t.Logf("file %d, attempts %d, reads %v, parsed %d of %d, %v", len(src), len(r.reads), r.reads, r.parsed, budget, took)
	if len(r.reads) != 9 {
		t.Fatalf("%d attempts ran, want 9 (reads %v)", len(r.reads), r.reads)
	}
	if took > 3*time.Second {
		t.Fatalf("took %v", took)
	}
}

// The 80-line input of TestResyncBudget is far under the byte floor: it
// stops at the attempt count, as before.
func TestResyncBudgetSmallFileUnchanged(t *testing.T) {
	src := "a() { echo hi; }\n) ) fi done\n" + strings.Repeat("echo ${x\n", 80) + "z() { :; }\n"
	r, stmts, limits := runResync(src)
	if limitNamed(limits, "resync-bytes") != nil {
		t.Fatalf("byte limit on a small file: %+v", limits)
	}
	l := limitNamed(limits, "resync-attempts")
	if l == nil || l.Detail != "resync stopped after 50 attempts" {
		t.Fatalf("limits %+v", limits)
	}
	if len(r.reads) != maxResyncAttempts {
		t.Fatalf("%d attempts", len(r.reads))
	}
	last := stmts[len(stmts)-1]
	if last.Kind != tree.KErrorGap || l.Range != last.Range {
		t.Fatalf("limit %+v, last %v %+v", l.Range, last.Kind, last.Range)
	}
}

// Stopping because no candidate line is left is no limit.
func TestResyncNoCandidateIsNoLimit(t *testing.T) {
	_, _, limits := runResync("a() { :; }\necho 'x\n  indented\n")
	if len(limits) != 0 {
		t.Fatalf("limits %+v", limits)
	}
}

// parseSafely reports the bytes the parser took from its reader, also when
// the parser panics.
func TestParseSafelyRead(t *testing.T) {
	src := []byte(strings.Repeat("echo hi\n", 1000))
	_, read, err, crashed := parseSafely(src, "x.sh", Bash)
	if err != nil || crashed || read != len(src) {
		t.Fatalf("read %d of %d, err %v, crashed %v", read, len(src), err, crashed)
	}
	_, read, _, crashed = parseSafely([]byte("A=$((0 A\nf() { :; }\n"), "x.sh", Bash)
	if !crashed || read <= 0 {
		t.Fatalf("crash: read %d, crashed %v", read, crashed)
	}
}

// T3-5, T4-3: a recovered panic is an internal error, marked as such.
func TestParserPanicIsInternal(t *testing.T) {
	f := Parse("x.sh", []byte("A=$((0 A\nf() { :; }\n"), Bash)
	n := 0
	for _, e := range f.Errors {
		if e.Internal {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("errors %+v", f.Errors)
	}
}

// T4-2: a statement that starts before the failing line and runs into it is
// dropped; the gap now starts where it starts, so no code goes missing
// without a gap.
func TestStraddlingStatementInGap(t *testing.T) {
	src := "x=$(foo \\\n\"bar\nf() { :; }\n"
	f := Parse("x.sh", []byte(src), Bash)
	var gap *tree.Node
	for _, c := range f.Root.Children {
		if c.Kind == tree.KErrorGap {
			gap = c
		}
	}
	if gap == nil || gap.Range.Start.Offset != 0 || gap.Range.End.Line != 3 {
		t.Fatalf("gap %+v", gap)
	}
	if find(f, tree.KFuncDecl, "f") == nil {
		t.Fatal("f not recovered")
	}
	checkInvariants(t, f)
}

// Parse stores the limits on the file.
func TestParseStoresLimits(t *testing.T) {
	src := "a() { echo hi; }\n) ) fi done\n" + strings.Repeat("echo ${x\n", 80) + "z() { :; }\n"
	f := Parse("x.sh", []byte(src), Bash)
	if len(f.Limits) != 1 || f.Limits[0].Name != "resync-attempts" {
		t.Fatalf("limits %+v", f.Limits)
	}
}

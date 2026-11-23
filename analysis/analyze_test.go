package analysis_test

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/analysis"
)

func abs(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(string(filepath.Separator)+"proj", p)
}

func TestAnalyzeFileSet(t *testing.T) {
	root := abs("")
	main, util := abs("main.sh"), abs("lib/util.sh")
	src := map[string][]byte{
		main: []byte("#!/bin/bash\nsource lib/util.sh\nhelper arg\n"),
		util: []byte("helper() {\n  echo \"$1\"\n}\n"),
	}
	s := analysis.Analyze(src, analysis.Options{Root: root})
	if s.Generation() != 1 {
		t.Fatalf("generation %d", s.Generation())
	}
	off := strings.Index(string(src[main]), "helper")
	locs := s.Definition(main, off)
	if len(locs) != 1 || locs[0].Path != util || locs[0].Range.Start.Line != 1 {
		t.Fatalf("Definition = %+v", locs)
	}
	if got := s.Paths(); !reflect.DeepEqual(got, []string{util, main}) {
		t.Fatalf("Paths = %v", got)
	}
}

func TestAnalyzeOversizeFileIsSkipped(t *testing.T) {
	p := abs("big.sh")
	big := []byte(strings.Repeat("a=1\n", analysis.MaxFileBytes/4+1))
	s := analysis.Analyze(map[string][]byte{p: big}, analysis.Options{Root: abs("")})
	ff := s.Facts(p)
	if ff == nil || ff.File.Skipped == "" || len(ff.Decls) != 0 {
		t.Fatalf("facts %+v", ff)
	}
	if analysis.MaxFileBytes != 2<<20 {
		t.Fatalf("MaxFileBytes = %d", analysis.MaxFileBytes)
	}
}

func TestReturnedSlicesAreCopies(t *testing.T) {
	main, util := abs("main.sh"), abs("lib/util.sh")
	src := map[string][]byte{
		main: []byte("source lib/util.sh\nhelper\nx=1\necho $x\n"),
		util: []byte("helper() { :; }\n"),
	}
	s := analysis.Analyze(src, analysis.Options{Root: abs("")})

	edges := s.SourceEdges(main)
	if len(edges) == 0 {
		t.Fatal("no source edges")
	}
	want := append([]analysis.SourceEdge(nil), edges...)
	wantTargets := append([]string(nil), edges[0].Targets...)
	for i := range edges {
		edges[i] = analysis.SourceEdge{}
	}
	if got := s.SourceEdges(main); !reflect.DeepEqual(got, want) {
		t.Fatalf("SourceEdges changed:\n got %+v\nwant %+v", got, want)
	}
	if len(wantTargets) > 0 {
		s.SourceEdges(main)[0].Targets[0] = "mutated"
		if got := s.SourceEdges(main)[0].Targets; !reflect.DeepEqual(got, wantTargets) {
			t.Fatalf("edge Targets changed: %v", got)
		}
	}

	vis := s.Visible(main)
	wantVis := append([]string(nil), vis...)
	for i := range vis {
		vis[i] = "mutated"
	}
	if got := s.Visible(main); !reflect.DeepEqual(got, wantVis) {
		t.Fatalf("Visible changed: %v", got)
	}

	paths := s.Paths()
	wantPaths := append([]string(nil), paths...)
	paths[0] = "mutated"
	if got := s.Paths(); !reflect.DeepEqual(got, wantPaths) {
		t.Fatalf("Paths changed: %v", got)
	}

	syms := s.WorkspaceSymbols("helper", 10)
	if len(syms) == 0 {
		t.Fatal("no helper symbol")
	}
	sym, ok := s.SymbolAt(main, strings.Index(string(src[main]), "helper"))
	if !ok {
		t.Fatal("no symbol at helper")
	}
	b := s.BindingsOf(sym)
	wantB := append([]analysis.Binding(nil), b...)
	for i := range b {
		b[i] = analysis.Binding{}
	}
	if got := s.BindingsOf(sym); !reflect.DeepEqual(got, wantB) {
		t.Fatalf("BindingsOf changed")
	}
}

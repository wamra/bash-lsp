package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func bigScript(n int) []byte {
	var b strings.Builder
	b.WriteString("#!/usr/bin/env bash\necho start\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "# Function number %d.\nfn_%d() {\n  local a=\"$1\" b=\"$2\"\n", i, i)
		fmt.Fprintf(&b, "  if [[ -n \"$a\" && \"$b\" != \"x%d\" ]]; then\n    printf '%%s %%s\\n' \"$a\" \"$b\" | tr a-z A-Z\n  else\n", i)
		b.WriteString("    for f in \"$@\"; do echo \"${f:-none}\" >&2; done\n  fi\n")
		fmt.Fprintf(&b, "  case \"$a\" in start|stop) echo \"$a\";; *) return %d;; esac\n}\n\n", i%250)
		if i > 0 {
			fmt.Fprintf(&b, "fn_%d a b\n", i-1)
		}
	}
	fmt.Fprintf(&b, "fn_%d a b\n", n-1)
	return []byte(b.String())
}

// BenchmarkSetOverlayBig is one editor keystroke on the 2,000-function script:
// parse, bind, hash and rebuild the snapshot.
func BenchmarkSetOverlayBig(b *testing.B) {
	dir := b.TempDir()
	src := bigScript(2000)
	p := filepath.Join(dir, "big.sh")
	if err := os.WriteFile(p, src, 0o644); err != nil {
		b.Fatal(err)
	}
	w, err := Open(dir, Limits{})
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(src)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// The text must differ from the disk file and from the last edit, or the
		// overlay would reuse already-built facts and measure nothing.
		w.SetOverlay(p, i+1, append(append([]byte{}, src...), fmt.Sprintf("# edit %d\n", i)...))
	}
}

// Opening a document whose text equals the file on disk (the common case) must
// not parse it a second time: the snapshot keeps the facts built by the walk.
func TestOverlayIdenticalToDiskReusesFacts(t *testing.T) {
	root := t.TempDir()
	src := []byte("#!/usr/bin/env bash\nfoo() { :; }\nfoo\n")
	p := filepath.Join(root, "a.sh")
	if err := os.WriteFile(p, src, 0o644); err != nil {
		t.Fatal(err)
	}
	w := open(t, root)
	p = w.Canon(p)
	before := w.Snapshot().Facts(p)
	if before == nil {
		t.Fatal("no facts after Open")
	}
	w.SetOverlay(p, 1, src)
	if after := w.Snapshot().Facts(p); after != before {
		t.Fatal("identical overlay was parsed again")
	}
	w.SetOverlay(p, 2, append(append([]byte{}, src...), "bar() { :; }\n"...))
	changed := w.Snapshot().Facts(p)
	if changed == before || len(changed.Decls) != len(before.Decls)+1 {
		t.Fatalf("edited overlay not reflected: %d decls, was %d", len(changed.Decls), len(before.Decls))
	}
	w.SetOverlay(p, 3, src)
	if w.Snapshot().Facts(p) == changed || len(w.Snapshot().Facts(p).Decls) != len(before.Decls) {
		t.Fatal("reverting the text did not revert the facts")
	}
}

// chainWorkspace writes files of funcs functions each into dir; file i sources
// file i-1 and function i_k calls function (i-1)_k (same shape as
// bench/gen_workspace.py).
func chainWorkspace(tb testing.TB, dir string, files, funcs int) {
	tb.Helper()
	for i := 0; i < files; i++ {
		var b strings.Builder
		b.WriteString("#!/usr/bin/env bash\n")
		if i > 0 {
			fmt.Fprintf(&b, "source ./s%04d.sh\n", i-1)
		}
		for k := 0; k < funcs; k++ {
			fmt.Fprintf(&b, "# Function %d_%d.\nf%d_%d() {\n  local a=\"$1\"\n", i, k, i, k)
			if i > 0 {
				fmt.Fprintf(&b, "  f%d_%d \"$a\"\n", i-1, k)
			}
			b.WriteString("  echo \"$a\"\n}\n\n")
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("s%04d.sh", i)), []byte(b.String()), 0o644); err != nil {
			tb.Fatal(err)
		}
	}
}

// BenchmarkSetOverlayChain1000 is one edit in the middle of a 1,000-file,
// 20-functions-each source chain: the whole-workspace relink.
func BenchmarkSetOverlayChain1000(b *testing.B) {
	dir := b.TempDir()
	chainWorkspace(b, dir, 1000, 20)
	w, err := Open(dir, Limits{})
	if err != nil {
		b.Fatal(err)
	}
	p := filepath.Join(w.Root(), "s0500.sh")
	src, _ := os.ReadFile(p)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w.SetOverlay(p, i+1, append(append([]byte{}, src...), fmt.Sprintf("# edit %d\n", i)...))
	}
}

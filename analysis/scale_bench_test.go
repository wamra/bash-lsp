package analysis

import (
	"fmt"
	"strings"
	"testing"

	"github.com/wamra/bash-lsp/internal/front"
)

// bigFunctions mirrors bench/gen_big.py: n functions, about 280 bytes each.
func bigFunctions(n int) []byte {
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

func commentLines(n int) []byte {
	var b strings.Builder
	b.WriteString("#!/usr/bin/env bash\necho start\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "# comment line number %d with some text to make it realistic\n", i)
	}
	b.WriteString("echo end\n")
	return []byte(b.String())
}

func benchParseBind(b *testing.B, src []byte) {
	b.SetBytes(int64(len(src)))
	for i := 0; i < b.N; i++ {
		Bind(front.Parse("big.sh", src, front.Bash))
	}
}

func BenchmarkParseBindFunctions2000(b *testing.B) { benchParseBind(b, bigFunctions(2000)) }
func BenchmarkParseBindComments40000(b *testing.B) { benchParseBind(b, commentLines(40000)) }
func BenchmarkParseBindComments10000(b *testing.B) { benchParseBind(b, commentLines(10000)) }

// chainFiles binds, in memory, the source chain of bench/gen_workspace.py:
// file i sources file i-1 and function i_k calls function (i-1)_k.
func chainFiles(root string, files, funcs int) map[string]*FileFacts {
	out := map[string]*FileFacts{}
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
		p := fmt.Sprintf("%s/s%04d.sh", root, i)
		out[p] = Bind(front.Parse(p, []byte(b.String()), front.Bash))
	}
	return out
}

// BenchmarkBuildEdgesChain1000 is the edge table alone, on the 1,000-file chain.
func BenchmarkBuildEdgesChain1000(b *testing.B) {
	s := Build(chainFiles("/ws", 1000, 20), Options{Root: "/ws"}, 1)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.buildEdges(len(s.recs))
	}
}

// BenchmarkBuildChain1000 is a whole Build of the 1,000-file chain with the
// previous snapshot given, as after a body edit.
func BenchmarkBuildChain1000(b *testing.B) {
	files := chainFiles("/ws", 1000, 20)
	prev := Build(files, Options{Root: "/ws"}, 1)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		prev = Build(files, Options{Root: "/ws", Prev: prev}, uint64(i+2))
	}
}

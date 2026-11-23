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

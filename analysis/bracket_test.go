package analysis

import (
	"crypto/sha256"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wamra/bash-lsp/internal/front"
)

// scanBracket is the matcher Bind used before the bracket table: from the `[`
// at open, the index of the `]` that brings the depth back to zero before
// limit, or -1. It is the oracle the table must agree with.
func scanBracket(src []byte, open, limit int) int {
	depth := 0
	for i := open; i < limit && i < len(src); i++ {
		switch src[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func checkBrackets(t *testing.T, src []byte, limits func(open int) []int) {
	t.Helper()
	b := &binder{src: src}
	for open, c := range src {
		if c != '[' {
			continue
		}
		for _, lim := range limits(open) {
			if got, want := b.matchBracket(open, lim), scanBracket(src, open, lim); got != want {
				t.Fatalf("%q: bracket at %d, limit %d: got %d, want %d", src, open, lim, got, want)
			}
		}
	}
}

// The table answers exactly what the scan answered, for every `[` and limit.
func TestMatchBracketMatchesScan(t *testing.T) {
	rnd := rand.New(rand.NewPCG(3, 4))
	alphabet := []byte("[[]]a $()")
	for range 2000 {
		src := make([]byte, rnd.IntN(30))
		for i := range src {
			src[i] = alphabet[rnd.IntN(len(alphabet))]
		}
		checkBrackets(t, src, func(open int) []int {
			var out []int
			for l := open; l <= len(src)+1; l++ {
				out = append(out, l)
			}
			return out
		})
	}
	// Real scripts: every bracket, against the end of the file and a few nearer limits.
	files, _ := filepath.Glob("../testdata/corpus/*/*")
	more, _ := filepath.Glob("../testdata/corpus/*")
	files = append(files, more...)
	checked := 0
	for _, p := range files {
		src, err := os.ReadFile(p)
		if err != nil || len(src) > 1<<16 { // the oracle is quadratic
			continue
		}
		checked++
		checkBrackets(t, src, func(open int) []int {
			return []int{open + 1, open + 2, open + 8, open + 64, len(src)}
		})
	}
	if checked == 0 {
		t.Fatal("no corpus file read")
	}
	checkBrackets(t, []byte(genArith()), func(open int) []int { return []int{open + 16, open + 4096} })
}

// genArith is bench/gen_arith.py in Go: about 560 KB of functions with one or
// two arithmetic constructs each.
func genArith() string {
	forms := []func(i int) string{
		func(i int) string { return fmt.Sprintf("  (( cnt += %d * n + 1, total++ ))", i) },
		func(i int) string { return fmt.Sprintf(`  echo "$(( cnt + n * %d ))" >&2`, i) },
		func(i int) string { return "  for ((k=0; k<n; k++)); do :; done" },
		func(i int) string { return "  let cnt=cnt+n total--" },
		func(i int) string { return `  echo "${arr[n+1]} ${v:off:len-1}" >&2` },
	}
	out := []string{"#!/usr/bin/env bash", "echo start"}
	for i := range 1620 {
		out = append(out, fmt.Sprintf("# Function number %d.", i), fmt.Sprintf("fn_%d() {", i), `  local a="$1" b="$2" n=3 cnt=0`,
			fmt.Sprintf(`  if [[ -n "$a" && "$b" != "x%d" ]]; then`, i), `    printf '%s %s\n' "$a" "$b" | tr a-z A-Z`, "  else",
			`    for f in "$@"; do echo "${f:-none}" >&2; done`, "  fi",
			fmt.Sprintf(`  case "$a" in start|stop) echo "$a";; *) return %d;; esac`, i%250),
			forms[i%5](i))
		if i%2 == 0 {
			out = append(out, forms[(i+2)%5](i))
		}
		out = append(out, "}", "")
		if i > 0 {
			out = append(out, fmt.Sprintf("fn_%d a b", i-1))
		}
	}
	out = append(out, "fn_1619 a b")
	return strings.Join(out, "\n") + "\n"
}

// factsDump is every variable declaration and use Bind records, with ranges and kinds.
func factsDump(ff *FileFacts) string {
	var sb strings.Builder
	for _, d := range ff.Decls {
		fmt.Fprintf(&sb, "D %d %q %d-%d sc%d %q %+v\n", d.Kind, d.Name, d.NameRange.Start.Offset, d.NameRange.End.Offset, d.Scope.Kind, d.Implicit, d.Attrs)
	}
	for _, u := range ff.Uses {
		fmt.Fprintf(&sb, "U %d %q %d-%d sc%d %v\n", u.Kind, u.Name, u.Range.Start.Offset, u.Range.End.Offset, u.Scope.Kind, u.Node.Kind)
	}
	return sb.String()
}

// arithSources are the arithmetic test programs: the FuzzBind seeds and the
// bracket shapes of review C1, small.
var arithSources = []string{
	arithProgram,
	"echo $(( $x + y ))\n",
	"(( a[b[c]] = d ? e : f++, --g ))\n",
	"let \"x=x+1\" y++ 'z=1'\n",
	"declare -A m; echo \"${m[key]} ${v:o+1:l*2} ${a[i]:1:n}\" m[k]=1\n",
	"for ((i=0;i<n;i++)); do ((s+=i)); done\n",
	"echo \"héllo ✓\" $(( é + yy )) ${arr[ñ]}\n",
	"x=${arr[$((i+1))]} y=$(( ${z:-3} ** 2 ))\n",
	"( (( n++ )) ); echo $n; f() { (( g-- )); }\n",
	"let \"" + strings.Repeat("a[", 50) + "\"\n",
	"declare -i n\nn=" + strings.Repeat("a[", 50) + "\n",
	"declare -i n\nn=" + strings.Repeat("a[", 50) + "1" + strings.Repeat("]", 50) + "\n",
	"(( a[i]=b[j[k]]+c[ ]] ))\nx[a[1]]=2 y=([a[b]]=1 [c]=2)\necho ${a[b[c]]:x[1]:y}\n",
}

// review C1: replacing the per-name bracket scan by the table changes no
// declaration or use. The digest was taken from the scan (af4a1a8) on the
// same inputs.
func TestArithFactsUnchangedByBracketTable(t *testing.T) {
	h := sha256.New()
	for _, s := range append(arithSources, genArith()) {
		h.Write([]byte(factsDump(Bind(front.Parse("/ws/a.sh", []byte(s), front.Bash)))))
	}
	if got := fmt.Sprintf("%x", h.Sum(nil)); got != arithFactsDigest {
		t.Fatalf("facts digest %s, want %s", got, arithFactsDigest)
	}
}

const arithFactsDigest = "991e3d00e3da298835f9fb52dd2f7bd45f8f85700ae77b53b96ce8b9cd85ff4a"

// review C1: matchBracket scanned to the end of the text (or to the matching
// bracket) once per name followed by `[`, so a run of `a[` cost O(n^2): 256
// KB took 13 s and 2 MiB 19 minutes. Each input is about 2 MiB, the file
// limit; the bound is generous. Under the race detector a quarter of the size
// and a looser bound.
func TestBracketRunsBindInBoundedTime(t *testing.T) {
	bound := 5 * time.Second
	n := 1<<20 - 16 // pairs of bytes: each input stays under 2 MiB
	if raceBuild {
		n /= 4
		bound = 60 * time.Second
	}
	for name, src := range map[string]string{
		"let, unclosed":     "let \"" + strings.Repeat("a[", n) + "\"\n",
		"integer, unclosed": "declare -i n\nn=" + strings.Repeat("a[", n) + "\n",
		"integer, balanced": "declare -i n\nn=" + strings.Repeat("a[", n*2/3) + "1" + strings.Repeat("]", n*2/3) + "\n",
	} {
		if len(src) > 2<<20 {
			t.Fatalf("%s: %d bytes, over the 2 MiB file limit", name, len(src))
		}
		f := front.Parse("/ws/a.sh", []byte(src), front.Bash)
		if f.Skipped != "" {
			t.Fatalf("%s: not parsed: %s", name, f.Skipped)
		}
		done := make(chan time.Duration, 1)
		go func() {
			start := time.Now()
			Bind(f)
			done <- time.Since(start)
		}()
		select {
		case d := <-done:
			t.Logf("%s: %d bytes bound in %v", name, len(src), d.Round(time.Millisecond))
		case <-time.After(bound):
			t.Errorf("%s: %d bytes not bound within %v", name, len(src), bound)
		}
	}
}

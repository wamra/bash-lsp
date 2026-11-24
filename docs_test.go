package bashlsp_test

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The documents quote figures from committed result files; these tests read
// the file and require the document to agree with it.

const shapeSizesFile = "bench/results/shape-sizes-m2b-final.txt"

type sizeRow struct {
	shape, metric string
	ratios        []float64
	times         []string
}

var (
	ratioRe = regexp.MustCompile(`^x(\d+\.\d+)$`)
	timesRe = regexp.MustCompile(`(\d+) / (\d+) / (\d+) / (\d+) ms`)
)

// readSizeRows are the rows of the size table, up to the first "# load after".
func readSizeRows(t *testing.T) []sizeRow {
	t.Helper()
	b, err := os.ReadFile(shapeSizesFile)
	if err != nil {
		t.Fatal(err)
	}
	var rows []sizeRow
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "# load after") {
			break
		}
		f := strings.Fields(line)
		if len(f) < 4 || strings.HasPrefix(line, "#") {
			continue
		}
		r := sizeRow{shape: f[0], metric: f[1]}
		if m := timesRe.FindStringSubmatch(line); m != nil {
			r.times = m[1:]
		}
		for _, w := range f[2:] {
			if w == "overall" {
				break
			}
			if m := ratioRe.FindStringSubmatch(w); m != nil {
				v, _ := strconv.ParseFloat(m[1], 64)
				r.ratios = append(r.ratios, v)
			}
		}
		rows = append(rows, r)
	}
	if len(rows) == 0 {
		t.Fatalf("%s: no rows", shapeSizesFile)
	}
	return rows
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

func readDoc(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return collapse(string(b))
}

func TestAcceptanceDoublingRangeMatchesResults(t *testing.T) {
	doc := readDoc(t, "docs/m2b-acceptance.md")
	lo, hi := 1e9, 0.0
	var high []sizeRow
	for _, r := range readSizeRows(t) {
		if r.shape == "func_defs_2k_calls_2k" {
			continue
		}
		for _, v := range r.ratios {
			lo, hi = min(lo, v), max(hi, v)
			if v > 2.8 {
				high = append(high, sizeRow{r.shape, r.metric, []float64{v}, nil})
			}
		}
	}
	rng := fmt.Sprintf("x%.1f to x%.1f", lo, hi)
	if !regexp.MustCompile(`scatter\w*[^.]{0,40}` + regexp.QuoteMeta(rng)).MatchString(doc) {
		t.Errorf("docs/m2b-acceptance.md does not give the single-doubling scatter %q next to the word scatter (%s)", rng, shapeSizesFile)
	}
	if len(high) != 3 {
		t.Fatalf("expected three single doublings above x2.8 outside func_defs_2k_calls_2k, got %v", high)
	}
	for _, h := range high {
		if !strings.Contains(doc, h.shape) || !strings.Contains(doc, fmt.Sprintf("x%.1f", h.ratios[0])) {
			t.Errorf("docs/m2b-acceptance.md does not name the cell %s %s at x%.1f", h.shape, h.metric, h.ratios[0])
		}
	}
}

func TestDeferredStatesResrefsRatio(t *testing.T) {
	doc := readDoc(t, "docs/m1-deferred.md")
	var row *sizeRow
	for _, r := range readSizeRows(t) {
		if r.shape == "func_defs_2k_calls_2k" && r.metric == "resrefs" {
			r := r
			row = &r
			break
		}
	}
	if row == nil || len(row.ratios) == 0 || len(row.times) != 4 {
		t.Fatalf("%s: no func_defs_2k_calls_2k resrefs row", shapeSizesFile)
	}
	const marker = "about x4 as the edges do"
	at := strings.Index(doc, marker)
	if at < 0 {
		t.Fatalf("docs/m1-deferred.md lacks %q", marker)
	}
	start := strings.LastIndex(doc[:at], ". ") + 1
	end := len(doc)
	if i := strings.Index(doc[at:], ". "); i >= 0 {
		end = at + i + 1
	}
	sentence := doc[start:end]
	for _, want := range []string{
		fmt.Sprintf("x%.1f", row.ratios[len(row.ratios)-1]), "resrefs", row.times[2] + " to " + row.times[3] + " ms",
	} {
		if !strings.Contains(sentence, want) {
			t.Errorf("the sentence %q lacks %q", sentence, want)
		}
	}
}

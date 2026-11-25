package analysis

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// Every measured probe result agrees with the table: a row fails below its
// first version and passes from it, on each real bash that was probed.
func TestTableMatchesProbeResults(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "bench", "results", "version-probe-*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no probe results: %v", err)
	}
	re := regexp.MustCompile(`^(\d+)\.(\d+)`)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var rec struct {
			Version string `json:"BASH_VERSION"`
			Rows    []struct {
				Key     string `json:"key"`
				Verdict string `json:"verdict"`
			} `json:"rows"`
		}
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		m := re.FindStringSubmatch(rec.Version)
		if m == nil {
			t.Fatalf("%s: version %q", f, rec.Version)
		}
		maj, _ := strconv.Atoi(m[1])
		min, _ := strconv.Atoi(m[2])
		have := BashVersion{maj, min}
		verdict := map[string]string{}
		for _, r := range rec.Rows {
			verdict[r.Key] = r.Verdict
		}
		for _, row := range FeatureTable() {
			if row.MinBash == (BashVersion{}) {
				continue
			}
			want := "fails"
			if !have.Less(row.MinBash) {
				want = "passes"
			}
			if got := verdict[row.Key]; got != want {
				t.Errorf("%s: %s on bash %s: measured %q, table (first %s) says %q",
					filepath.Base(f), row.Key, rec.Version, got, row.MinBash, want)
			}
		}
	}
}
